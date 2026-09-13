package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCreateAsyncRejectsWrongAcceptanceWithoutFallback(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{201, `{"id":"sandbox"}`}, {202, `{"id":"operation","type":"sandbox_batch_create","requested":1}`},
		{202, `{"id":"operation","type":"sandbox_create","status":"pending","requested":1}`},
		{202, `{"id":`}, {404, `{"detail":"unsupported route"}`}, {405, `{"detail":"unsupported method"}`},
		{302, `{}`},
	} {
		t.Run(fmt.Sprint(tc.status, tc.body), func(t *testing.T) {
			var requests atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/v1/sandbox-creations" || r.Header.Get("Idempotency-Key") != "key" {
					t.Errorf("request=%s %v", r.URL.Path, r.Header)
				}
				if tc.status == 302 {
					w.Header().Set("Location", "/sandboxes")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			_, err := NewHTTP(s.URL, "").CreateAsync(context.Background(), "key", CreateRequest{})
			if err == nil || requests.Load() != 1 {
				t.Fatalf("err=%v requests=%d", err, requests.Load())
			}
			var acceptance *CreateAcceptanceError
			var api *APIError
			if tc.status >= 400 {
				if !errors.As(err, &api) || api.StatusCode != tc.status {
					t.Fatalf("err=%v", err)
				}
			} else if !errors.As(err, &acceptance) || acceptance.IdempotencyKey != "key" {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
func TestCreateAsyncContextBoundsAcceptanceBody(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		fmt.Fprint(w, `{"id":`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := NewHTTP(s.URL, "").CreateAsync(ctx, "recover-key", CreateRequest{})
	var acceptance *CreateAcceptanceError
	if !errors.As(err, &acceptance) || acceptance.IdempotencyKey != "recover-key" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}
func TestWaitOperationWorkerSuccessIsNotTerminal(t *testing.T) {
	var gets atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := "succeeded"
		completed := "null"
		if gets.Add(1) == 3 {
			status = "succeeded"
			completed = `"2026-01-01T00:00:01Z"`
		}
		fmt.Fprintf(w, `{"id":"op","type":"sandbox_create","status":%q,"completed_at":%s,"requested":1,"results":[{"index":0,"progress":{"coordination":{"phase":"assigned"},"worker":{"condition":"succeeded"}}}]}`, status, completed)
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	op, err := NewHTTP(s.URL, "").WaitOperation(ctx, "op", time.Millisecond, nil)
	if err != nil || op.Status != "succeeded" || gets.Load() != 3 {
		t.Fatalf("op=%+v err=%v gets=%d", op, err, gets.Load())
	}
}
