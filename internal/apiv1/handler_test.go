package apiv1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/httpapi"
	"github.com/ayush6624/sandbox/internal/registry"
)

type fakeLegacy struct {
	mu      sync.Mutex
	creates int
	items   map[string]registry.Sandbox
}

func newFakeLegacy() *fakeLegacy {
	return &fakeLegacy{items: map[string]registry.Sandbox{
		"existing": {
			ID: "existing", PID: 99, SocketPath: "/run/private.sock", RootfsPath: "/private/rootfs",
			Status: registry.StatusRunning, CreatedAt: time.Now(), Vcpus: 2, MemMIB: 1024,
		},
	}}
}

func (f *fakeLegacy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "GET" && r.URL.Path == "/sandboxes":
		out := make([]registry.Sandbox, 0, len(f.items))
		for _, item := range f.items {
			out = append(out, item)
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == "POST" && r.URL.Path == "/sandboxes":
		f.creates++
		id := "created"
		f.items[id] = registry.Sandbox{ID: id, Status: registry.StatusRunning, CreatedAt: time.Now(), Vcpus: 2, MemMIB: 1024}
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(f.items[id])
	case r.Method == "PATCH" && strings.HasSuffix(r.URL.Path, "/public-fields"):
		id := strings.Split(r.URL.Path, "/")[2]
		item := f.items[id]
		var body struct {
			Name       string            `json:"name"`
			SourceType string            `json:"source_type"`
			SourceID   string            `json:"source_id"`
			Metadata   map[string]string `json:"metadata"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		item.Name, item.SourceType, item.SourceID, item.Metadata = body.Name, body.SourceType, body.SourceID, body.Metadata
		f.items[id] = item
		_ = json.NewEncoder(w).Encode(item)
	default:
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	}
}

func testHandler(t *testing.T, legacy http.Handler) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	New(legacy).Register(mux)
	return httpapi.Middleware(mux)
}

func TestListSanitizesInternalsAndPaginates(t *testing.T) {
	h := testHandler(t, newFakeLegacy())
	req := httptest.NewRequest("GET", "/v1/sandboxes?page_size=1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "socket_path") || strings.Contains(w.Body.String(), "rootfs_path") || strings.Contains(w.Body.String(), `"pid"`) {
		t.Fatalf("host internals leaked: %s", w.Body.String())
	}
	var body struct {
		Sandboxes []Sandbox `json:"sandboxes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Sandboxes) != 1 {
		t.Fatalf("response=%s err=%v", w.Body.String(), err)
	}
}

func TestCreateRequiresAndReplaysIdempotencyKey(t *testing.T) {
	legacy := newFakeLegacy()
	h := testHandler(t, legacy)
	call := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/sandboxes", strings.NewReader(`{"name":"ci","metadata":{"run":"1"}}`))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if got := call("").Code; got != 400 {
		t.Fatalf("missing key status=%d", got)
	}
	first, replay := call("create-1"), call("create-1")
	if first.Code != 201 || replay.Code != 201 || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("first=%d replay=%d marker=%q", first.Code, replay.Code, replay.Header().Get("Idempotency-Replayed"))
	}
	if legacy.creates != 1 {
		t.Fatalf("legacy creates=%d", legacy.creates)
	}
	var sb Sandbox
	if err := json.Unmarshal(first.Body.Bytes(), &sb); err != nil || sb.Metadata["run"] != "1" {
		t.Fatalf("body=%s err=%v", first.Body.String(), err)
	}
}

func TestUnknownFieldsUseProblemDetails(t *testing.T) {
	h := testHandler(t, newFakeLegacy())
	req := httptest.NewRequest("POST", "/v1/sandboxes", strings.NewReader(`{"fanout":32}`))
	req.Header.Set("Idempotency-Key", "bad")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 400 || w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("status=%d content-type=%q body=%s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"invalid_request"`) {
		t.Fatalf("problem missing stable code: %s", w.Body.String())
	}
}

func TestListRejectsInvalidFilters(t *testing.T) {
	h := testHandler(t, newFakeLegacy())
	req := httptest.NewRequest("GET", "/v1/sandboxes?status=hibernated", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"invalid_filter"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
