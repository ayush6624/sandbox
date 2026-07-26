// Package httpapi contains transport-level behavior shared by the public
// worker and fleet gateway APIs.
package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const RequestIDHeader = "X-Request-Id"

type contextKey struct{}

// RequestID returns the identifier assigned by Middleware.
func RequestID(r *http.Request) string {
	if id, ok := r.Context().Value(contextKey{}).(string); ok {
		return id
	}
	return ""
}

// Middleware assigns a request ID, adds it to every response, and emits a
// concise completion log without logging credentials or query strings.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(RequestIDHeader))
		if id == "" || len(id) > 128 || strings.ContainsAny(id, "\r\n") {
			id = uuid.NewString()
		}
		ctx := context.WithValue(r.Context(), contextKey{}, id)
		r = r.WithContext(ctx)
		w.Header().Set(RequestIDHeader, id)
		rec := &statusWriter{ResponseWriter: w}
		started := time.Now()
		next.ServeHTTP(rec, r)
		log.Printf("http_request request_id=%s method=%s path=%s status=%d duration_ms=%d",
			id, r.Method, r.URL.Path, rec.statusCode(), time.Since(started).Milliseconds())
	})
}

// Problem is the RFC 9457 response shape with stable machine-readable
// extensions.
type Problem struct {
	Type       string           `json:"type"`
	Title      string           `json:"title"`
	Status     int              `json:"status"`
	Detail     string           `json:"detail,omitempty"`
	Instance   string           `json:"instance,omitempty"`
	Code       string           `json:"code"`
	RequestID  string           `json:"request_id"`
	Violations []FieldViolation `json:"violations,omitempty"`
}

type FieldViolation struct {
	Field       string `json:"field"`
	Description string `json:"description"`
}

// WriteProblem writes an application/problem+json error.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string, violations ...FieldViolation) {
	if code == "" {
		code = defaultCode(status)
	}
	p := Problem{
		Type:       "https://sandbox.dev/problems/" + code,
		Title:      http.StatusText(status),
		Status:     status,
		Detail:     detail,
		Instance:   r.URL.Path,
		Code:       code,
		RequestID:  RequestID(r),
		Violations: violations,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

func defaultCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusServiceUnavailable:
		return "capacity_unavailable"
	default:
		if status >= 500 {
			return "internal_error"
		}
		return "request_failed"
	}
}

// Store provides process-local idempotency for public mutations. A key is
// scoped to method and path. Reuse with a different body is rejected; an
// identical completed request replays status, headers, and body.
type Store struct {
	mu      sync.Mutex
	entries map[string]*entry
	ttl     time.Duration
}

type entry struct {
	hash    [32]byte
	done    chan struct{}
	status  int
	headers http.Header
	body    []byte
	at      time.Time
}

func NewStore(ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Store{entries: make(map[string]*entry), ttl: ttl}
}

// Wrap enforces Idempotency-Key on a mutation handler.
func (s *Store) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			WriteProblem(w, r, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key is required")
			return
		}
		if len(key) > 255 || strings.ContainsAny(key, "\r\n") {
			WriteProblem(w, r, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must be at most 255 characters")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "could not read request body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		hash := sha256.Sum256(body)
		scope := r.Method + " " + r.URL.Path + " " + key

		e, owner := s.acquire(scope, hash)
		if !owner {
			if e.hash != hash {
				WriteProblem(w, r, http.StatusConflict, "idempotency_key_reused", "Idempotency-Key was already used with a different request")
				return
			}
			select {
			case <-e.done:
				copyHeaders(w.Header(), e.headers)
				w.Header().Set("Idempotency-Replayed", "true")
				w.WriteHeader(e.status)
				_, _ = w.Write(e.body)
			case <-r.Context().Done():
				WriteProblem(w, r, 499, "request_cancelled", "request cancelled while awaiting the original operation")
			}
			return
		}

		rec := newCapture()
		next.ServeHTTP(rec, r)
		s.complete(scope, e, rec)
		copyHeaders(w.Header(), rec.header)
		w.WriteHeader(rec.statusCode())
		_, _ = w.Write(rec.body.Bytes())
	})
}

func (s *Store) acquire(scope string, hash [32]byte) (*entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for key, old := range s.entries {
		if now.Sub(old.at) > s.ttl {
			delete(s.entries, key)
		}
	}
	if existing := s.entries[scope]; existing != nil {
		return existing, false
	}
	e := &entry{hash: hash, done: make(chan struct{}), at: now}
	s.entries[scope] = e
	return e, true
}

func (s *Store) complete(scope string, e *entry, rec *captureWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.status = rec.statusCode()
	e.headers = rec.header.Clone()
	e.body = append([]byte(nil), rec.body.Bytes()...)
	e.at = time.Now()
	close(e.done)
	if e.status >= 500 {
		delete(s.entries, scope)
	}
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
}

// Cursor encodes an opaque list offset. It is intentionally not a database
// key so the representation can change without becoming API surface.
func Cursor(offset int) string {
	if offset <= 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("sandbox-cursor:%d", offset)))
	return fmt.Sprintf("%d.%s", offset, hex.EncodeToString(sum[:6]))
}

func ParseCursor(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	var offset int
	var signature string
	if _, err := fmt.Sscanf(value, "%d.%s", &offset, &signature); err != nil || offset < 0 {
		return 0, fmt.Errorf("invalid cursor")
	}
	if Cursor(offset) != value {
		return 0, fmt.Errorf("invalid cursor")
	}
	return offset, nil
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}
func (w *statusWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// Unwrap lets http.ResponseController reach optional interfaces such as
// Flusher and Hijacker on the real writer. This keeps streaming exec and
// WebSocket shell upgrades working through request instrumentation.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type captureWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newCapture() *captureWriter             { return &captureWriter{header: make(http.Header)} }
func (w *captureWriter) Header() http.Header { return w.header }
func (w *captureWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *captureWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.body.Write(p)
}
func (w *captureWriter) statusCode() int {
	if w.status == 0 {
		return 200
	}
	return w.status
}
