package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/apiv1"
	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
)

func TestSingleAsyncLostAcceptanceReopensWithoutAnotherWarmClaim(t *testing.T) {
	s := createRequestServer(t)
	if _, err := s.reg.CreateWarmForTemplate(context.Background(), "warm-single", "/tmp/unused-single-disk", "golden", "golden", 1, 128); err != nil {
		t.Fatal(err)
	}
	if err := s.reg.MarkWarmReady(context.Background(), "warm-single"); err != nil {
		t.Fatal(err)
	}
	s.golden.Store(&registry.Snapshot{ID: "golden", Golden: true})
	store, path := deliveryStore(t)
	h := apiv1.NewWithCreateOperations(http.NotFoundHandler(), store, s)
	mux := http.NewServeMux()
	h.Register(mux)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.RunCreateOperations(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	receipts := make(chan []byte, 1)
	lost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorded := httptest.NewRecorder()
		mux.ServeHTTP(recorded, r)
		if recorded.Code != 202 {
			t.Errorf("acceptance: %d %s", recorded.Code, recorded.Body.String())
		}
		receipts <- bytes.Clone(recorded.Body.Bytes())
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer lost.Close()
	post := func(url string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodPost, url+"/v1/sandbox-creations", strings.NewReader(`{"name":"once"}`))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Idempotency-Key", "lost-single-key")
		return lost.Client().Do(req)
	}
	response, err := post(lost.URL)
	if err == nil {
		response.Body.Close()
		t.Fatal("lost acceptance unexpectedly returned a response")
	}
	original := <-receipts
	var receipt apiv1.Operation
	if err := json.Unmarshal(original, &receipt); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var completed createops.Operation
	for time.Now().Before(deadline) {
		completed, err = store.Get(context.Background(), receipt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if completed.CompletedAt != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if completed.CompletedAt == nil || len(completed.Members) != 1 || completed.Members[0].Outcome == nil || completed.Members[0].Outcome.Sandbox == nil || completed.Members[0].Outcome.Sandbox.ID != "warm-single" {
		t.Fatalf("warm completion: %+v", completed)
	}
	cancel()
	<-done
	lost.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := createops.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recoveredHandler := apiv1.NewWithCreateOperations(http.NotFoundHandler(), reopened, s)
	recoveredMux := http.NewServeMux()
	recoveredHandler.Register(recoveredMux)
	recovered := httptest.NewServer(recoveredMux)
	defer recovered.Close()
	for deleted := 0; deleted < 2; deleted++ {
		response, err := post(recovered.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 202 || !bytes.Equal(body, original) {
			t.Fatalf("replayed receipt: %d %s %v", response.StatusCode, body, err)
		}
		rows, err := s.reg.All(context.Background())
		if err != nil || len(rows) != 1-deleted {
			t.Fatalf("replay allocated again: %+v %v", rows, err)
		}
		if deleted == 0 {
			if rows[0].ID != "warm-single" || rows[0].Status != registry.StatusRunning {
				t.Fatalf("wrong warm claim: %+v", rows)
			}
			if err := s.reg.Destroy(context.Background(), "warm-single"); err != nil {
				t.Fatal(err)
			}
		}
	}
}
