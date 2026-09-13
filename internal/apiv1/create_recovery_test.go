package apiv1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/httpapi"
	"github.com/ayush6624/sandbox/internal/registry"
)

type recoveryExecutor struct {
	started chan struct{}
	release chan struct{}
	failure *createops.Failure
}

func (e recoveryExecutor) Place(context.Context, createops.Spec, int) (createops.Placement, error) {
	return createops.Placement{Worker: createops.Worker{HostID: "h", RegistryID: "r"}, Release: func([]createops.Outcome) {}}, nil
}
func (e recoveryExecutor) Execute(ctx context.Context, _ createops.Worker, c createops.Command) ([]createops.Outcome, error) {
	if e.started != nil {
		select {
		case e.started <- struct{}{}:
		default:
			{
			}
		}
	}
	if e.release != nil {
		select {
		case <-e.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	out := make([]createops.Outcome, len(c.Members))
	for i, m := range c.Members {
		out[i] = createops.Outcome{ID: m.ID, Sandbox: &registry.Sandbox{ID: "stable", Status: registry.StatusRunning, CreatedAt: time.Now(), Vcpus: 2, MemMIB: 1024}}
		if e.failure != nil {
			out[i] = createops.Outcome{ID: m.ID, Failure: e.failure}
		}
	}
	return out, nil
}

func TestCreateFailureRetainsRequestIDAfterReopen(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "batch"}[batch], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ops.db")
			executor := recoveryExecutor{failure: &createops.Failure{Status: 404, Code: "source_not_found"}}
			route, body := "/v1/sandboxes", `{}`
			if batch {
				route, body = "/v1/sandbox-batches", `{"count":1,"sandbox":{}}`
			}
			for reopen := 0; reopen < 2; reopen++ {
				h, closeHandler := recoveryHandler(t, path, executor)
				func() {
					defer closeHandler()
					h = httpapi.Middleware(h)
					req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(body))
					req.Header.Set("Idempotency-Key", "source-error")
					req.Header.Set(httpapi.RequestIDHeader, "original-request")
					if reopen > 0 {
						req.Header.Set(httpapi.RequestIDHeader, "replay-request")
					}
					w := httptest.NewRecorder()
					h.ServeHTTP(w, req)
					var problem httpapi.Problem
					if batch {
						var op Operation
						if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &op) != nil {
							t.Fatalf("accept: %d %s", w.Code, w.Body.String())
						}
						deadline := time.Now().Add(time.Second)
						for op.CompletedAt == nil && time.Now().Before(deadline) {
							w = httptest.NewRecorder()
							h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/operations/"+op.ID, nil))
							if err := json.Unmarshal(w.Body.Bytes(), &op); err != nil {
								t.Fatal(err)
							}
							if op.CompletedAt == nil {
								time.Sleep(time.Millisecond)
							}
						}
						if len(op.Results) != 1 || op.Results[0].Error == nil {
							t.Fatalf("no batch failure: %+v", op)
						}
						problem = *op.Results[0].Error
					} else if w.Code != 404 || json.Unmarshal(w.Body.Bytes(), &problem) != nil {
						t.Fatalf("failure: %d %s", w.Code, w.Body.String())
					}
					if problem.RequestID != "original-request" {
						t.Fatalf("reopen %d request_id = %q, want original-request", reopen, problem.RequestID)
					}
				}()
			}
		})
	}
}

func recoveryHandler(t *testing.T, path string, e createops.Executor) (http.Handler, func()) {
	t.Helper()
	s, err := createops.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithCreateOperations(http.NotFoundHandler(), s, e)
	mux := http.NewServeMux()
	h.Register(mux)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.RunCreateOperations(ctx); close(done) }()
	return mux, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("dispatcher did not join")
		}
		_ = h.Close()
	}
}

func TestSingleReplayUsesPersistedFinalBytesAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.db")
	h, close1 := recoveryHandler(t, path, recoveryExecutor{})
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", strings.NewReader(`{"name":"a"}`))
	req.Header.Set("Idempotency-Key", "key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 201 || w.Header().Get("Location") != "/v1/sandboxes/stable" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("first %d %q %q", w.Code, w.Header().Get("Location"), w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	close1()
	h, close2 := recoveryHandler(t, path, recoveryExecutor{})
	defer close2()
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes", strings.NewReader(`{"name":"a"}`))
	req.Header.Set("Idempotency-Key", "key")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 201 || w.Body.String() != body || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay %d %q %q", w.Code, w.Body.String(), w.Header().Get("Idempotency-Replayed"))
	}
}

func TestCancelledSingleWaitDoesNotCancelExecution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.db")
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	h, stop := recoveryHandler(t, path, recoveryExecutor{started: started, release: release})
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", strings.NewReader(`{}`)).WithContext(ctx)
	req.Header.Set("Idempotency-Key", "cancel")
	done := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("not executing")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("wait did not cancel")
	}
	close(release)
	time.Sleep(30 * time.Millisecond)
	s, err := createops.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, err := s.List(context.Background(), createops.ListQuery{Limit: 100})
	ops := page.Operations
	if err != nil || len(ops) != 1 {
		t.Fatalf("operations=%+v err=%v", ops, err)
	}
	_, _, ready, err := s.FinalResponse(context.Background(), ops[0].ID)
	if err != nil || !ready {
		t.Fatalf("final ready=%v err=%v", ready, err)
	}
}
