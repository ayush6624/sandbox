package httpapi

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMiddlewareRedactsCredentialsAndQueryStringsFromLogs(t *testing.T) {
	var logs bytes.Buffer
	originalWriter := log.Writer()
	originalFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(originalWriter)
		log.SetFlags(originalFlags)
	})

	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes?access_token=query-secret&token=other-secret", nil)
	req.Header.Set("Authorization", "Bearer header-secret")
	req.Header.Set("Cookie", "session=cookie-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	got := logs.String()
	for _, secret := range []string{"query-secret", "other-secret", "header-secret", "cookie-secret", "access_token", "Authorization"} {
		if strings.Contains(got, secret) {
			t.Fatalf("request log leaked %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "method=GET path=/v1/sandboxes status=204") {
		t.Fatalf("request log missing safe fields: %s", got)
	}
}

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
