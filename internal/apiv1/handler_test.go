package apiv1

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/httpapi"
	"github.com/ayush6624/sandbox/internal/registry"
)

func TestInternalDispatchPreservesNonNilEmptyBody(t *testing.T) {
	legacy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/snapshots/snap" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Fatalf("read empty body: %v", err)
		}
		if len(body) != 0 {
			t.Fatalf("body=%q, want empty", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodDelete, "/v1/snapshots/snap", nil)
	req.Header.Set("Idempotency-Key", "delete-snapshot")
	w := httptest.NewRecorder()
	testHandler(t, legacy).ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

type fakeLegacy struct {
	mu      sync.Mutex
	creates int
	items   map[string]registry.Sandbox
	snaps   map[string]registry.Snapshot
	exposed []registry.PortMapping
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
	// Subresource GETs are matched further down; this case is the id lookup.
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/sandboxes/") &&
		!strings.HasSuffix(r.URL.Path, "/ports"):
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
		var body struct {
			RetentionSeconds int `json:"retention_seconds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		snap := registry.Snapshot{ID: "snap_1", SourceID: id, CreatedAt: time.Now(), Durability: "local"}
		if body.RetentionSeconds > 0 {
			value := time.Now().Add(time.Duration(body.RetentionSeconds) * time.Second)
			snap.ExpiresAt = &value
		}
		f.snaps[snap.ID] = snap
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(snap)
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/snapshots/") && strings.HasSuffix(r.URL.Path, "/public-fields"):
		id := strings.Split(r.URL.Path, "/")[2]
		snap := f.snaps[id]
		var body struct {
			Name      string     `json:"name"`
			ExpiresAt *time.Time `json:"expires_at"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		snap.Name = body.Name
		snap.ExpiresAt = body.ExpiresAt
		f.snaps[id] = snap
		_ = json.NewEncoder(w).Encode(snap)
	case r.Method == "GET" && r.URL.Path == "/snapshots":
		out := make([]registry.Snapshot, 0, len(f.snaps))
		for _, snap := range f.snaps {
			out = append(out, snap)
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/ports"):
		// Mirror a worker with an ingress domain configured: every exposure
		// carries a URL, and host_port decides whether a host port comes too.
		var body struct {
			GuestPort int   `json:"guest_port"`
			HostPort  *bool `json:"host_port"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		pm := registry.PortMapping{
			GuestPort: body.GuestPort, HostPort: 5200, Mode: "both",
			URL: fmt.Sprintf("https://%d-existing.sandboxes.example.com", body.GuestPort),
		}
		if body.HostPort != nil && !*body.HostPort {
			pm.HostPort, pm.Mode = 0, "url"
		}
		f.exposed = append(f.exposed, pm)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(pm)
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/ports"):
		_ = json.NewEncoder(w).Encode(f.exposed)
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

// A URL-only exposure has no host port, and the public ingress URL is the only
// way to reach it — so /v1 must carry `url`/`mode` through and must NOT report
// `host_port: 0`, which is both undialable and below the contract's minimum.
func TestPortForwardsCarryIngressURLAndOmitAbsentHostPort(t *testing.T) {
	h := testHandler(t, newFakeLegacy())
	call := func(body, key string) map[string]any {
		req := httptest.NewRequest("POST", "/v1/sandboxes/existing/port-forwards", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 201 {
			t.Fatalf("create port forward=%d body=%s", w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	both := call(`{"guest_port":8080,"host_port":true}`, "both")
	if both["host_port"] != float64(5200) || both["mode"] != "both" ||
		both["url"] != "https://8080-existing.sandboxes.example.com" {
		t.Fatalf("host+url exposure lost fields: %#v", both)
	}

	urlOnly := call(`{"guest_port":3000,"host_port":false}`, "url-only")
	if _, ok := urlOnly["host_port"]; ok {
		t.Fatalf("URL-only exposure must omit host_port entirely, got %#v", urlOnly)
	}
	if urlOnly["mode"] != "url" || urlOnly["url"] != "https://3000-existing.sandboxes.example.com" {
		t.Fatalf("URL-only exposure lost fields: %#v", urlOnly)
	}

	// The same shape must survive the list path, not just the create response.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/sandboxes/existing/port-forwards", nil))
	if w.Code != 200 {
		t.Fatalf("list=%d body=%s", w.Code, w.Body.String())
	}
	var list struct {
		PortForwards []map[string]any `json:"port_forwards"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.PortForwards) != 2 {
		t.Fatalf("want 2 port forwards, got %#v", list.PortForwards)
	}
	if _, ok := list.PortForwards[1]["host_port"]; ok {
		t.Fatalf("listed URL-only exposure must omit host_port, got %#v", list.PortForwards[1])
	}
	if list.PortForwards[1]["url"] != "https://3000-existing.sandboxes.example.com" {
		t.Fatalf("listed URL-only exposure lost its url: %#v", list.PortForwards[1])
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

func TestBatchOperationConcurrentPollingProducesValidJSON(t *testing.T) {
	fake := newFakeLegacy()
	legacy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/sandboxes" {
			// Keep the operation running long enough for many GETs to overlap
			// result publication.
			time.Sleep(time.Millisecond)
		}
		fake.ServeHTTP(w, r)
	})
	mux := http.NewServeMux()
	New(legacy).Register(mux)
	h := http.Handler(mux)
	req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-batches",
		strings.NewReader(`{"count":100,"sandbox":{},"max_parallelism":32}`))
	req.Header.Set("Idempotency-Key", "concurrent-poll")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch=%d body=%s", w.Code, w.Body.String())
	}
	var started Operation
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}

	const pollers = 16
	errs := make(chan error, pollers)
	var wg sync.WaitGroup
	for range pollers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				get := httptest.NewRequest(http.MethodGet, "/v1/operations/"+started.ID, nil)
				got := httptest.NewRecorder()
				h.ServeHTTP(got, get)
				if got.Code != http.StatusOK {
					errs <- fmt.Errorf("poll status=%d body=%s", got.Code, got.Body.String())
					return
				}
				var op Operation
				if err := json.Unmarshal(got.Body.Bytes(), &op); err != nil {
					errs <- fmt.Errorf("invalid operation JSON: %w: %q", err, got.Body.String())
					return
				}
				if op.Succeeded+op.Failed > op.Requested {
					errs <- fmt.Errorf("impossible operation counts: %+v", op)
					return
				}
				if op.CompletedAt != nil {
					if op.Succeeded != op.Requested || op.Failed != 0 {
						errs <- fmt.Errorf("incomplete terminal operation: %+v", op)
					}
					return
				}
			}
			errs <- fmt.Errorf("operation %s did not complete", started.ID)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
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
