package apiv1

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/httpapi"
)

type countedSingleExecutor struct {
	recoveryExecutor
	executions atomic.Int32
}

func (e *countedSingleExecutor) Execute(ctx context.Context, worker createops.Worker, command createops.Command) ([]createops.Outcome, error) {
	e.executions.Add(1)
	return e.recoveryExecutor.Execute(ctx, worker, command)
}

func postAsyncSingle(h http.Handler, body, requestID string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/sandbox-creations", strings.NewReader(body))
	r.Header.Set("Idempotency-Key", "recoverable-single")
	r.Header.Set(httpapi.RequestIDHeader, requestID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func readSingleOperation(t *testing.T, h http.Handler, id string) Operation {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/operations/"+id, nil))
	var op Operation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &op) != nil {
		t.Fatalf("GET operation: %d %s", w.Code, w.Body.String())
	}
	return op
}

func TestSingleAsyncAcceptanceReplayAndReopen(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failed], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "operations.db")
			release := make(chan struct{})
			executor := &countedSingleExecutor{recoveryExecutor: recoveryExecutor{release: release}}
			if failed {
				executor.failure = &createops.Failure{Status: 500, Code: "guest_readiness_failed", Detail: "The guest agent did not become ready."}
			}
			h, closeHandler := recoveryHandler(t, path, executor)
			h = httpapi.Middleware(h)
			closed := false
			defer func() {
				if !closed {
					closeHandler()
				}
			}()
			const body = `{"name":"single"}`
			accepted := postAsyncSingle(h, body, "original-single-request")
			var receipt Operation
			if accepted.Code != 202 || json.Unmarshal(accepted.Body.Bytes(), &receipt) != nil || receipt.Type != "sandbox_create" || receipt.Requested != 1 || receipt.CompletedAt != nil {
				t.Fatalf("accept: %d %s", accepted.Code, accepted.Body.String())
			}
			if accepted.Header().Get("Location") != "/v1/operations/"+receipt.ID {
				t.Fatalf("missing operation Location: %v", accepted.Header())
			}
			var parallel sync.WaitGroup
			for i := 0; i < 8; i++ {
				parallel.Add(1)
				go func() {
					defer parallel.Done()
					replay := postAsyncSingle(h, body, "retry-request")
					if replay.Code != 202 || !bytes.Equal(replay.Body.Bytes(), accepted.Body.Bytes()) || replay.Header().Get("Idempotency-Replayed") != "true" {
						t.Errorf("concurrent replay: %d %s", replay.Code, replay.Body.String())
					}
				}()
			}
			parallel.Wait()
			pending := readSingleOperation(t, h, receipt.ID)
			if pending.CompletedAt != nil || len(pending.Results) != 1 || pending.Results[0].Sandbox != nil {
				t.Fatalf("blocked executor completed: %+v", pending)
			}
			conflict := postAsyncSingle(h, `{"name":"different"}`, "conflicting-request")
			if conflict.Code != 409 {
				t.Fatalf("changed body: %d %s", conflict.Code, conflict.Body.String())
			}
			close(release)
			deadline := time.Now().Add(2 * time.Second)
			for pending.CompletedAt == nil && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
				pending = readSingleOperation(t, h, receipt.ID)
			}
			if pending.CompletedAt == nil || executor.executions.Load() != 1 {
				t.Fatalf("completion: %+v executions=%d", pending, executor.executions.Load())
			}
			closeHandler()
			closed = true
			reopened, closeReopened := recoveryHandler(t, path, executor)
			defer closeReopened()
			reopened = httpapi.Middleware(reopened)
			replay := postAsyncSingle(reopened, body, "after-reopen")
			if replay.Code != 202 || !bytes.Equal(replay.Body.Bytes(), accepted.Body.Bytes()) || replay.Header().Get(httpapi.RequestIDHeader) != "original-single-request" {
				t.Fatalf("completed receipt changed: %d %s %v", replay.Code, replay.Body.String(), replay.Header())
			}
			final := readSingleOperation(t, reopened, receipt.ID)
			if final.Type != "sandbox_create" || final.CompletedAt == nil || len(final.Results) != 1 || (final.Results[0].Error != nil) != failed {
				t.Fatalf("retained result: %+v", final)
			}
			list := httptest.NewRecorder()
			reopened.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/v1/operations", nil))
			var page struct {
				Operations []Operation `json:"operations"`
			}
			if list.Code != 200 || json.Unmarshal(list.Body.Bytes(), &page) != nil || len(page.Operations) != 1 || page.Operations[0].ID != receipt.ID {
				t.Fatalf("single absent from list: %d %s", list.Code, list.Body.String())
			}
		})
	}
}

func TestSingleAsyncConcurrentFirstAcceptanceAndPendingRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operations.db")
	store, err := createops.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := NewWithCreateOperations(http.NotFoundHandler(), store, recoveryExecutor{})
	mux := http.NewServeMux()
	h.Register(mux)
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 8)
	for i := 0; i < cap(responses); i++ {
		go func() { <-start; responses <- postAsyncSingle(mux, `{}`, "race-request") }()
	}
	close(start)
	var original []byte
	created := 0
	for i := 0; i < cap(responses); i++ {
		w := <-responses
		if w.Code != 202 {
			t.Fatalf("first acceptance: %d %s", w.Code, w.Body.String())
		}
		if w.Header().Get("Idempotency-Replayed") == "" {
			created++
		}
		if original == nil {
			original = bytes.Clone(w.Body.Bytes())
		}
		if !bytes.Equal(original, w.Body.Bytes()) {
			t.Fatal("concurrent first receipts differed")
		}
	}
	page, err := store.List(context.Background(), createops.ListQuery{Limit: 100})
	ops := page.Operations
	if err != nil || len(ops) != 1 || len(ops[0].Members) != 1 || created != 1 {
		t.Fatalf("initial acceptance duplicated: %+v created=%d err=%v", ops, created, err)
	}
	if ops[0].Members[0].Worker != nil || ops[0].CompletedAt != nil {
		t.Fatal("operation executed without a dispatcher")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	executor := &countedSingleExecutor{}
	reopened, closeHandler := recoveryHandler(t, path, executor)
	defer closeHandler()
	replay := postAsyncSingle(reopened, `{}`, "after-pending-restart")
	if replay.Code != 202 || !bytes.Equal(original, replay.Body.Bytes()) {
		t.Fatalf("pending restart changed acceptance: %d %s", replay.Code, replay.Body.String())
	}
	op := readSingleOperation(t, reopened, ops[0].ID)
	deadline := time.Now().Add(2 * time.Second)
	for op.CompletedAt == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		op = readSingleOperation(t, reopened, ops[0].ID)
	}
	if op.CompletedAt == nil || executor.executions.Load() != 1 {
		t.Fatalf("pending restart did not execute exactly once: %+v count=%d", op, executor.executions.Load())
	}
}
