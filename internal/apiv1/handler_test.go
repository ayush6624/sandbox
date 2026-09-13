package apiv1

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/createops"
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

func TestCLIPathsAreAdaptedToWorkerDataPlane(t *testing.T) {
	seen := make(chan string, 2)
	legacy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	handler := testHandler(t, legacy)

	keyReq := httptest.NewRequest(http.MethodPut, "/v1/sandboxes/abc/ssh-access", strings.NewReader(`{"public_key":"ssh-ed25519 AAAA"}`))
	keyReq.Header.Set("Idempotency-Key", "authorize-key")
	keyOut := httptest.NewRecorder()
	handler.ServeHTTP(keyOut, keyReq)
	if keyOut.Code != http.StatusNoContent {
		t.Fatalf("key status=%d body=%s", keyOut.Code, keyOut.Body.String())
	}

	connectReq := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/abc/connect/22", nil)
	connectOut := httptest.NewRecorder()
	handler.ServeHTTP(connectOut, connectReq)
	if connectOut.Code != http.StatusNoContent {
		t.Fatalf("connect status=%d body=%s", connectOut.Code, connectOut.Body.String())
	}

	if got := <-seen; got != "PUT /sandboxes/abc/ssh-key" {
		t.Fatalf("key dispatch = %q", got)
	}
	if got := <-seen; got != "GET /sandboxes/abc/connect/22" {
		t.Fatalf("connect dispatch = %q", got)
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
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/snapshots/") && strings.HasSuffix(r.URL.Path, "/warm-target"):
		id := strings.Split(r.URL.Path, "/")[2]
		snap := f.snaps[id]
		var body struct {
			WarmTarget int `json:"warm_target"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		snap.WarmTarget = body.WarmTarget
		f.snaps[id] = snap
		_ = json.NewEncoder(w).Encode(snap)
	case r.Method == "GET" && r.URL.Path == "/snapshots":
		out := make([]registry.Snapshot, 0, len(f.snaps))
		for _, snap := range f.snaps {
			out = append(out, snap)
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/raw-ports"):
		var body struct {
			GuestPort int `json:"guest_port"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		pm := registry.PortMapping{
			GuestPort: body.GuestPort, PublicHost: "tcp.sandboxes.example.com",
			PublicPort: 20000, Mode: "raw",
		}
		f.exposed = append(f.exposed, pm)
		_ = json.NewEncoder(w).Encode(registry.RawPortMapping{
			GuestPort: body.GuestPort, Mode: "raw",
			PublicHost: pm.PublicHost, PublicPort: pm.PublicPort,
		})
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
	store, err := createops.Open(t.TempDir() + "/operations.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mux := http.NewServeMux()
	h := NewWithCreateOperations(legacy, store, legacyCreateExecutor{legacy: legacy})
	h.Register(mux)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.RunCreateOperations(ctx)
	return httpapi.Middleware(mux)
}

type legacyCreateExecutor struct{ legacy http.Handler }

func (legacyCreateExecutor) Place(context.Context, createops.Spec, int) (createops.Placement, error) {
	return createops.Placement{Worker: createops.Worker{HostID: "test", RegistryID: "test"}, Release: func([]createops.Outcome) {}}, nil
}
func (e legacyCreateExecutor) Execute(ctx context.Context, _ createops.Worker, cmd createops.Command) ([]createops.Outcome, error) {
	if len(cmd.Members) > 1 {
		source, snap := normalizeSource(Source{Type: cmd.Members[0].Spec.Source.Type, ID: cmd.Members[0].Spec.Source.ID})
		if snap {
			data, _ := json.Marshal(map[string]any{"count": len(cmd.Members)})
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/snapshots/"+source.ID+"/fanout", strings.NewReader(string(data)))
			w := httptest.NewRecorder()
			e.legacy.ServeHTTP(w, req)
			var raws []registry.Sandbox
			if w.Code < 200 || w.Code >= 300 || json.Unmarshal(w.Body.Bytes(), &raws) != nil {
				return nil, nil
			}
			out := make([]createops.Outcome, 0, len(cmd.Members))
			for i, m := range cmd.Members {
				if i < len(raws) {
					raw := raws[i]
					out = append(out, createops.Outcome{ID: m.ID, Sandbox: &raw})
				} else {
					out = append(out, createops.Outcome{ID: m.ID, Failure: &createops.Failure{Status: 503, Code: "short_response"}})
				}
			}
			return out, nil
		}
	}
	out := make([]createops.Outcome, 0, len(cmd.Members))
	for _, m := range cmd.Members {
		body := map[string]any{"name": m.Spec.Name, "timeout_sec": m.Spec.Lifecycle.TTLSeconds, "hibernate_after_sec": m.Spec.Lifecycle.IdleTimeoutSeconds}
		if m.Spec.Resources != nil {
			body["vcpus"] = m.Spec.Resources.VCPU
			body["mem_mib"] = m.Spec.Resources.MemoryMIB
		}
		source, from := normalizeSource(Source{Type: m.Spec.Source.Type, ID: m.Spec.Source.ID})
		path := "/sandboxes"
		if from {
			path = "/snapshots/" + source.ID + "/fanout"
			body["count"] = 1
		}
		data, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(string(data)))
		w := httptest.NewRecorder()
		e.legacy.ServeHTTP(w, req)
		if w.Code < 200 || w.Code >= 300 {
			out = append(out, createops.Outcome{ID: m.ID, Failure: &createops.Failure{Status: w.Code, Code: "batch_item_failed", Detail: w.Body.String()}})
			continue
		}
		var raw registry.Sandbox
		if from {
			var xs []registry.Sandbox
			if json.Unmarshal(w.Body.Bytes(), &xs) != nil || len(xs) != 1 {
				out = append(out, createops.Outcome{ID: m.ID, Failure: &createops.Failure{Status: 502, Code: "invalid_upstream_response", Detail: "invalid fanout"}})
				continue
			}
			raw = xs[0]
		} else if json.Unmarshal(w.Body.Bytes(), &raw) != nil {
			out = append(out, createops.Outcome{ID: m.ID, Failure: &createops.Failure{Status: 502, Code: "invalid_upstream_response", Detail: "invalid create"}})
			continue
		}
		fields, _ := json.Marshal(map[string]any{"source_type": source.Type, "source_id": source.ID, "metadata": m.Spec.Metadata, "name": m.Spec.Name})
		patch, _ := http.NewRequestWithContext(ctx, http.MethodPatch, "/sandboxes/"+raw.ID+"/public-fields", strings.NewReader(string(fields)))
		pw := httptest.NewRecorder()
		e.legacy.ServeHTTP(pw, patch)
		if pw.Code >= 200 && pw.Code < 300 {
			_ = json.Unmarshal(pw.Body.Bytes(), &raw)
		}
		out = append(out, createops.Outcome{ID: m.ID, Sandbox: &raw})
	}
	return out, nil
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
	legacy := newFakeLegacy()
	legacy.snaps["template_py"] = registry.Snapshot{
		ID: "template_py", Role: registry.SnapshotRoleTemplate, Vcpus: 4, MemMIB: 2048, CreatedAt: time.Now(),
	}
	h := testHandler(t, legacy)
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
	if got := call("GET", "/v1/templates/template_py", "", ""); got.Code != 200 ||
		!strings.Contains(got.Body.String(), `"memory_mib":2048`) {
		t.Fatalf("custom template=%d body=%s", got.Code, got.Body.String())
	}
	if got := call("PATCH", "/v1/templates/template_py", `{"warm_target":2}`, "warm"); got.Code != 200 ||
		!strings.Contains(got.Body.String(), `"warm_target":2`) {
		t.Fatalf("warm template=%d body=%s", got.Code, got.Body.String())
	}
	if got := call("PATCH", "/v1/templates/template_py", `{}`, "warm-missing"); got.Code != 400 ||
		!strings.Contains(got.Body.String(), "warm_target is required") {
		t.Fatalf("missing warm target=%d body=%s", got.Code, got.Body.String())
	}
	if got := call("PATCH", "/v1/templates/template_py", `{"warm_target":2,"extra":true}`, "warm-extra"); got.Code != 400 {
		t.Fatalf("unknown warm field=%d body=%s", got.Code, got.Body.String())
	}
	port := call("POST", "/v1/sandboxes/existing/port-forwards", `{"guest_port":8080}`, "port")
	if port.Code != 201 || !strings.Contains(port.Body.String(), `"status":"active"`) {
		t.Fatalf("port=%d body=%s", port.Code, port.Body.String())
	}
	if got := call("POST", "/v1/sandboxes/existing/port-forwards", `{"guest_port":0}`, "bad-port"); got.Code != 400 {
		t.Fatalf("bad port=%d body=%s", got.Code, got.Body.String())
	}
}

func TestSnapshotUploadProjectionAndBaseFiltering(t *testing.T) {
	legacy := newFakeLegacy()
	next := time.Now().Add(time.Minute).Round(time.Second)
	legacy.snaps["captured"] = registry.Snapshot{
		ID: "captured", SourceID: "sandbox", Durability: "local", CreatedAt: time.Now(),
		Upload: &registry.SnapshotUpload{State: "retrying", Attempts: 2, NextAttemptAt: &next, Error: "temporary storage outage"},
	}
	legacy.snaps["cached"] = registry.Snapshot{ID: "cached", SourceID: "sandbox", Durability: "local", CreatedAt: time.Now()}
	legacy.snaps["base"] = registry.Snapshot{ID: "base", SourceID: "sandbox", Role: registry.SnapshotRoleBase, CreatedAt: time.Now()}
	legacy.snaps["durable"] = registry.Snapshot{
		ID: "durable", SourceID: "sandbox", Durability: "durable", CreatedAt: time.Now(),
		Upload: &registry.SnapshotUpload{State: "retrying", Attempts: 9},
	}
	h := testHandler(t, legacy)

	list := httptest.NewRecorder()
	h.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/v1/snapshots", nil))
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), `"id":"base"`) {
		t.Fatalf("list=%d body=%s", list.Code, list.Body.String())
	}
	var page struct {
		Snapshots []Snapshot `json:"snapshots"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Snapshot, len(page.Snapshots))
	for _, snapshot := range page.Snapshots {
		byID[snapshot.ID] = snapshot
	}
	if got := byID["captured"].Upload; got == nil || got.State != "retrying" || got.Attempts != 2 || got.NextAttemptAt == nil || got.Error != "temporary storage outage" {
		t.Fatalf("captured upload=%+v", got)
	}
	if got := byID["cached"].Upload; got != nil {
		t.Fatalf("cache upload=%+v, want nil", got)
	}
	if got := byID["durable"].Upload; got != nil {
		t.Fatalf("durable upload=%+v, want nil", got)
	}

	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/snapshots/base", nil))
	if get.Code != http.StatusNotFound {
		t.Fatalf("base get=%d body=%s", get.Code, get.Body.String())
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

	raw := call(`{"guest_port":22,"mode":"raw"}`, "raw")
	if raw["mode"] != "raw" || raw["public_host"] != "tcp.sandboxes.example.com" ||
		raw["public_port"] != float64(20000) {
		t.Fatalf("raw exposure lost its public address: %#v", raw)
	}
	if _, ok := raw["host_port"]; ok {
		t.Fatalf("raw exposure must not report a worker host port: %#v", raw)
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
	if len(list.PortForwards) != 3 {
		t.Fatalf("want 3 port forwards, got %#v", list.PortForwards)
	}
	if _, ok := list.PortForwards[1]["host_port"]; ok {
		t.Fatalf("listed URL-only exposure must omit host_port, got %#v", list.PortForwards[1])
	}
	if list.PortForwards[1]["url"] != "https://3000-existing.sandboxes.example.com" {
		t.Fatalf("listed URL-only exposure lost its url: %#v", list.PortForwards[1])
	}
	if list.PortForwards[2]["public_host"] != "tcp.sandboxes.example.com" ||
		list.PortForwards[2]["public_port"] != float64(20000) {
		t.Fatalf("listed raw exposure lost its public address: %#v", list.PortForwards[2])
	}
}

func TestRawPortForwardRejectsConflictingAddressMode(t *testing.T) {
	h := testHandler(t, newFakeLegacy())
	for _, body := range []string{
		`{"guest_port":22,"mode":"raw","host_port":true}`,
		`{"guest_port":22,"mode":"private"}`,
	} {
		req := httptest.NewRequest("POST", "/v1/sandboxes/existing/port-forwards", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", body)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status=%d response=%s", body, w.Code, w.Body.String())
		}
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
	h := testHandler(t, legacy)
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

// A template built from a container image IS a snapshot, so `source.templateId`
// has to reach the clone path with that id — the spelling customers use for a
// template must not silently create a default sandbox instead.
func TestTemplateSourceClonesTheNamedTemplate(t *testing.T) {
	var seen string
	legacy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/public-fields") {
			seen = r.URL.Path
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/fanout"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `[{"id":"sb_1","status":"running"}]`)
		case strings.HasSuffix(r.URL.Path, "/public-fields"):
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"id":"sb_1","status":"running"}`)
		default:
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"sb_1","status":"running"}`)
		}
	})

	create := func(body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", key)
		out := httptest.NewRecorder()
		testHandler(t, legacy).ServeHTTP(out, req)
		return out
	}

	if got := create(`{"source":{"type":"template","id":"tmpl_abc"}}`, "tmpl"); got.Code != http.StatusCreated {
		t.Fatalf("template create=%d body=%s", got.Code, got.Body.String())
	}
	if seen != "/snapshots/tmpl_abc/fanout" {
		t.Fatalf("worker path=%q, want the template cloned", seen)
	}

	// "default" stays reserved for the host's built-in image.
	if got := create(`{"source":{"type":"template","id":"default"}}`, "default"); got.Code != http.StatusCreated {
		t.Fatalf("default create=%d body=%s", got.Code, got.Body.String())
	}
	if seen != "/sandboxes" {
		t.Fatalf("worker path=%q, want an ordinary create", seen)
	}

	if got := create(`{"source":{"type":"template"}}`, "empty"); got.Code != http.StatusBadRequest {
		t.Fatalf("empty template id=%d, want 400", got.Code)
	}
}

// A snapshot-sourced batch must reach the worker as fanout calls carrying a
// COUNT, not as `count` separate fanouts of one. Each single-clone fanout takes
// the worker's per-snapshot lock for its whole bring-up, so N of them run
// strictly one at a time — measured dead-linear at 756 ms per sandbox, i.e.
// ~11.3 s for a 15-sandbox batch, with max_parallelism having no effect at all.
func TestSnapshotBatchIssuesCountedFanouts(t *testing.T) {
	var mu sync.Mutex
	var counts []int
	legacy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/fanout"):
			var body struct {
				Count int `json:"count"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			counts = append(counts, body.Count)
			mu.Unlock()
			list := make([]string, body.Count)
			for i := range list {
				list[i] = fmt.Sprintf(`{"id":"sb_%d_%d","status":"running"}`, len(counts), i)
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, "[%s]", strings.Join(list, ","))
		case strings.HasSuffix(r.URL.Path, "/public-fields"):
			id := strings.Split(r.URL.Path, "/")[2]
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"id":%q,"status":"running"}`, id)
		default:
			t.Errorf("unexpected worker call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	h := testHandler(t, legacy)
	req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-batches",
		strings.NewReader(`{"count":15,"sandbox":{"source":{"type":"snapshot","id":"snap_1"}}}`))
	req.Header.Set("Idempotency-Key", "snap-batch")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch=%d body=%s", w.Code, w.Body.String())
	}
	var op Operation
	if err := json.Unmarshal(w.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := httptest.NewRecorder()
		h.ServeHTTP(got, httptest.NewRequest("GET", "/v1/operations/"+op.ID, nil))
		if err := json.Unmarshal(got.Body.Bytes(), &op); err != nil {
			t.Fatal(err)
		}
		if op.CompletedAt != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if op.Status != "succeeded" || op.Succeeded != 15 {
		t.Fatalf("operation=%+v", op)
	}
	mu.Lock()
	defer mu.Unlock()
	// 15 clones at the default max_parallelism of 8 → chunks of 8 and 7, never
	// fifteen calls of one.
	if len(counts) != 2 {
		t.Fatalf("worker saw %d fanout calls (%v), want 2 chunked calls", len(counts), counts)
	}
	total := 0
	for _, n := range counts {
		if n < 2 {
			t.Fatalf("fanout call asked for %d clones (%v) — the batch is being serialized", n, counts)
		}
		total += n
	}
	if total != 15 {
		t.Fatalf("fanout calls requested %d clones (%v), want 15", total, counts)
	}
	// Every index must be filled exactly once across chunks.
	for i, result := range op.Results {
		if result.Index != i || result.Sandbox == nil || result.Error != nil {
			t.Fatalf("result[%d]=%+v", i, result)
		}
	}
}

// A single create keeps sending count:1 — the non-batch path is unchanged.
func TestSingleSnapshotCreateStillSendsCountOne(t *testing.T) {
	got := -1
	legacy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/fanout") {
			var body struct {
				Count int `json:"count"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			got = body.Count
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `[{"id":"sb_1","status":"running"}]`)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"sb_1","status":"running"}`)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes",
		strings.NewReader(`{"source":{"type":"snapshot","id":"snap_1"}}`))
	req.Header.Set("Idempotency-Key", "one")
	w := httptest.NewRecorder()
	testHandler(t, legacy).ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create=%d body=%s", w.Code, w.Body.String())
	}
	if got != 1 {
		t.Fatalf("single create sent count=%d, want 1", got)
	}
}
