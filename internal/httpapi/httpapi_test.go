package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMiddlewareAndProblemShareRequestID(t *testing.T) {
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteProblem(w, r, http.StatusBadRequest, "bad_widget", "widget is invalid",
			FieldViolation{Field: "widget", Description: "is required"})
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/widgets", nil)
	req.Header.Set(RequestIDHeader, "req-test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get(RequestIDHeader); got != "req-test" {
		t.Fatalf("request ID header = %q", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("content type = %q", got)
	}
	var problem Problem
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.RequestID != "req-test" || problem.Code != "bad_widget" || len(problem.Violations) != 1 {
		t.Fatalf("unexpected problem: %+v", problem)
	}
}

func TestIdempotencyReplayAndConflict(t *testing.T) {
	var calls atomic.Int64
	store := NewStore(time.Hour)
	h := Middleware(store.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "/v1/sandboxes/sb_1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"sb_1"}`))
	})))

	call := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", "create-1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	first := call(`{"name":"one"}`)
	second := call(`{"name":"one"}`)
	conflict := call(`{"name":"two"}`)

	if first.Code != 201 || second.Code != 201 || calls.Load() != 1 {
		t.Fatalf("codes=%d,%d calls=%d", first.Code, second.Code, calls.Load())
	}
	if second.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("replay marker missing")
	}
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d", conflict.Code)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	for _, offset := range []int{0, 1, 999} {
		got, err := ParseCursor(Cursor(offset))
		if err != nil || got != offset {
			t.Fatalf("offset %d round trip = %d, %v", offset, got, err)
		}
	}
	if _, err := ParseCursor("4.not-valid"); err == nil {
		t.Fatal("tampered cursor accepted")
	}
}
