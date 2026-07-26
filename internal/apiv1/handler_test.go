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
	snaps   map[string]registry.Snapshot
}

func newFakeLegacy() *fakeLegacy {
	return &fakeLegacy{items: map[string]registry.Sandbox{
		"existing": {
			ID: "existing", PID: 99, SocketPath: "/run/private.sock", RootfsPath: "/private/rootfs",
			Status: registry.StatusRunning, CreatedAt: time.Now(), Vcpus: 2, MemMIB: 1024,
		},
	}, snaps: make(map[string]registry.Snapshot)}
}

func (f *fakeLegacy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "GET" && r.URL.Path == "/info":
		_ = json.NewEncoder(w).Encode(map[string]int64{"default_vcpus": 2, "default_mem_mib": 1024})
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
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/sandboxes/"):
		id := strings.Split(r.URL.Path, "/")[2]
		item, ok := f.items[id]
		if !ok {
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "sandbox not found"})
			break
		}
		_ = json.NewEncoder(w).Encode(item)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/hibernate"):
		id := strings.Split(r.URL.Path, "/")[2]
		item := f.items[id]
		item.Status = registry.StatusHibernated
		f.items[id] = item
		_ = json.NewEncoder(w).Encode(item)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/resume"):
		id := strings.Split(r.URL.Path, "/")[2]
		item := f.items[id]
		item.Status = registry.StatusRunning
		f.items[id] = item
		_ = json.NewEncoder(w).Encode(item)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/snapshot"):
		id := strings.Split(r.URL.Path, "/")[2]
		snap := registry.Snapshot{ID: "snap_1", SourceID: id, CreatedAt: time.Now(), Durability: "local"}
		f.snaps[snap.ID] = snap
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(snap)
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/snapshots/") && strings.HasSuffix(r.URL.Path, "/public-fields"):
		id := strings.Split(r.URL.Path, "/")[2]
		snap := f.snaps[id]
		var body struct {
			Name             string `json:"name"`
			RetentionSeconds int    `json:"retention_seconds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		snap.Name = body.Name
		if body.RetentionSeconds > 0 {
			value := time.Now().Add(time.Duration(body.RetentionSeconds) * time.Second)
			snap.ExpiresAt = &value
		}
		f.snaps[id] = snap
		_ = json.NewEncoder(w).Encode(snap)
	case r.Method == "GET" && r.URL.Path == "/snapshots":
		out := make([]registry.Snapshot, 0, len(f.snaps))
		for _, snap := range f.snaps {
			out = append(out, snap)
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/ports"):
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(registry.PortMapping{GuestPort: 8080, HostPort: 5200})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/ports"):
		_ = json.NewEncoder(w).Encode([]registry.PortMapping{{GuestPort: 8080, HostPort: 5200}})
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

func TestLifecycleSnapshotTemplateAndPortResources(t *testing.T) {
	h := testHandler(t, newFakeLegacy())
	call := func(method, path, body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	paused := call("POST", "/v1/sandboxes/existing:pause", "", "pause")
	if paused.Code != 200 || !strings.Contains(paused.Body.String(), `"status":"paused"`) {
		t.Fatalf("pause=%d body=%s", paused.Code, paused.Body.String())
	}
	resumed := call("POST", "/v1/sandboxes/existing:resume", "", "resume")
	if resumed.Code != 200 || !strings.Contains(resumed.Body.String(), `"status":"running"`) {
		t.Fatalf("resume=%d body=%s", resumed.Code, resumed.Body.String())
	}
	snapshot := call("POST", "/v1/sandboxes/existing/snapshots", `{"name":"checkpoint","retention_seconds":60}`, "snapshot")
	if snapshot.Code != 201 || !strings.Contains(snapshot.Body.String(), `"name":"checkpoint"`) ||
		!strings.Contains(snapshot.Body.String(), `"expires_at"`) {
		t.Fatalf("snapshot=%d body=%s", snapshot.Code, snapshot.Body.String())
	}
	if got := call("GET", "/v1/snapshots/snap_1", "", ""); got.Code != 200 {
		t.Fatalf("get snapshot=%d body=%s", got.Code, got.Body.String())
	}
	if got := call("GET", "/v1/templates/default", "", ""); got.Code != 200 ||
		!strings.Contains(got.Body.String(), `"memory_mib":1024`) {
		t.Fatalf("template=%d body=%s", got.Code, got.Body.String())
	}
	port := call("POST", "/v1/sandboxes/existing/port-forwards", `{"guest_port":8080}`, "port")
	if port.Code != 201 || !strings.Contains(port.Body.String(), `"status":"active"`) {
		t.Fatalf("port=%d body=%s", port.Code, port.Body.String())
	}
	if got := call("POST", "/v1/sandboxes/existing/port-forwards", `{"guest_port":0}`, "bad-port"); got.Code != 400 {
		t.Fatalf("bad port=%d body=%s", got.Code, got.Body.String())
	}
}

func TestBatchOperationReturnsEveryIndexedResult(t *testing.T) {
	h := testHandler(t, newFakeLegacy())
	req := httptest.NewRequest("POST", "/v1/sandbox-batches", strings.NewReader(`{"count":3,"sandbox":{},"max_parallelism":2}`))
	req.Header.Set("Idempotency-Key", "batch")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 202 {
		t.Fatalf("batch=%d body=%s", w.Code, w.Body.String())
	}
	var op Operation
	if err := json.Unmarshal(w.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		get := httptest.NewRequest("GET", "/v1/operations/"+op.ID, nil)
		got := httptest.NewRecorder()
		h.ServeHTTP(got, get)
		if err := json.Unmarshal(got.Body.Bytes(), &op); err != nil {
			t.Fatal(err)
		}
		if op.Status == "succeeded" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if op.Status != "succeeded" || op.Succeeded != 3 || len(op.Results) != 3 {
		t.Fatalf("operation=%+v", op)
	}
	for i, result := range op.Results {
		if result.Index != i || result.Sandbox == nil || result.Error != nil {
			t.Fatalf("result[%d]=%+v", i, result)
		}
	}
}

func TestInvalidFiltersReturnProblem(t *testing.T) {
	h := testHandler(t, newFakeLegacy())
	for _, query := range []string{"status=stale", "source_type=clone", "created_after=yesterday"} {
		req := httptest.NewRequest("GET", "/v1/sandboxes?"+query, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"invalid_filter"`) {
			t.Fatalf("%s => %d %s", query, w.Code, w.Body.String())
		}
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
