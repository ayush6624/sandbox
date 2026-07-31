package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/gcsblob"
)

func TestHandleRawRouteReturnsDialableWorkerAddress(t *testing.T) {
	g := liveGateway(&host{
		id: "worker-a", addr: "http://10.0.0.2:8080", token: "worker-token",
		slotsTotal: 2, lastSeen: time.Now(),
	})
	g.route["sandbox-a"] = "worker-a"
	g.raw = &rawAllocator{
		index: rawIndex{Version: 1, Leases: map[string]rawLease{
			"20000": {
				PublicPort: 20000,
				SandboxID:  "sandbox-a",
				GuestPort:  22,
				State:      "active",
			},
		}},
		loaded: true,
	}
	req := httptest.NewRequest("GET", "/raw-route/20000", nil)
	req.SetPathValue("port", "20000")
	w := httptest.NewRecorder()
	g.handleRawRoute(w, req)
	if w.Code != 200 {
		t.Fatalf("raw route: %d %s", w.Code, w.Body)
	}
	var got struct {
		SandboxID string `json:"sandbox_id"`
		GuestPort int    `json:"guest_port"`
		HostAddr  string `json:"host_addr"`
		Token     string `json:"token"`
		TTL       int    `json:"ttl"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SandboxID != "sandbox-a" || got.GuestPort != 22 ||
		got.HostAddr != "10.0.0.2:8080" || got.Token != "worker-token" || got.TTL != 5 {
		t.Fatalf("raw route = %+v", got)
	}
}

type memoryRawStore struct {
	mu   sync.Mutex
	data []byte
	gen  int64
}

func (s *memoryRawStore) GetBytesGen(context.Context, string) ([]byte, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen == 0 {
		return nil, 0, gcsblob.ErrNotExist
	}
	return append([]byte(nil), s.data...), s.gen, nil
}

func (s *memoryRawStore) PutBytesIfGenerationMatch(_ context.Context, _ string, data []byte, gen int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return 0, gcsblob.ErrPreconditionFailed
	}
	s.gen++
	s.data = append([]byte(nil), data...)
	return s.gen, nil
}

func testRawAllocator(t *testing.T, store rawStore) *rawAllocator {
	t.Helper()
	a := &rawAllocator{
		cfg:   RawConfig{PublicHost: "tcp.example.com", PortMin: 20000, PortMax: 20001},
		store: store,
	}
	if err := a.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRawAllocatorPersistsAndIsIdempotent(t *testing.T) {
	store := &memoryRawStore{}
	a := testRawAllocator(t, store)
	assigns := 0
	first, err := a.allocate(context.Background(), "sandbox-a", 22, func(port int) error {
		assigns++
		if port != 20000 {
			t.Fatalf("first port=%d", port)
		}
		return nil
	})
	if err != nil || first.State != "active" {
		t.Fatalf("allocate=%+v err=%v", first, err)
	}
	second, err := a.allocate(context.Background(), "sandbox-a", 22, func(int) error {
		assigns++
		return nil
	})
	if err != nil || second.LeaseID != first.LeaseID || assigns != 1 {
		t.Fatalf("idempotent=%+v err=%v assigns=%d", second, err, assigns)
	}

	reloaded := testRawAllocator(t, store)
	got, ok := reloaded.route(20000)
	if !ok || got.SandboxID != "sandbox-a" || got.GuestPort != 22 {
		t.Fatalf("reloaded route=%+v ok=%v", got, ok)
	}
}

func TestRawAllocatorRollsBackFailedAssignment(t *testing.T) {
	a := testRawAllocator(t, &memoryRawStore{})
	want := errors.New("worker failed")
	if _, err := a.allocate(context.Background(), "sandbox-a", 22, func(int) error {
		return want
	}); !errors.Is(err, want) {
		t.Fatalf("allocate error=%v", err)
	}
	if _, ok := a.route(20000); ok {
		t.Fatal("failed assignment left an active route")
	}
	lease, err := a.allocate(context.Background(), "sandbox-b", 22, func(int) error { return nil })
	if err != nil || lease.PublicPort != 20000 {
		t.Fatalf("released port was not reusable: %+v %v", lease, err)
	}
}

func TestRawAllocatorExhaustion(t *testing.T) {
	a := testRawAllocator(t, &memoryRawStore{})
	for _, id := range []string{"a", "b"} {
		if _, err := a.allocate(context.Background(), id, 22, func(int) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.allocate(context.Background(), "c", 22, func(int) error { return nil }); err == nil {
		t.Fatal("expected raw port exhaustion")
	}
}

func TestRawAllocatorCASRaceAssignsDistinctPorts(t *testing.T) {
	store := &memoryRawStore{}
	a := testRawAllocator(t, store)
	b := testRawAllocator(t, store)
	start := make(chan struct{})
	type result struct {
		lease rawLease
		err   error
	}
	results := make(chan result, 2)
	for i, allocator := range []*rawAllocator{a, b} {
		id := string(rune('a' + i))
		go func() {
			<-start
			lease, err := allocator.allocate(context.Background(), id, 22, func(int) error { return nil })
			results <- result{lease, err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("allocations failed: %v, %v", first.err, second.err)
	}
	if first.lease.PublicPort == second.lease.PublicPort {
		t.Fatalf("CAS race reused public port %d", first.lease.PublicPort)
	}
}

func TestRawAllocatorReleaseIsLeaseSafe(t *testing.T) {
	a := testRawAllocator(t, &memoryRawStore{})
	lease, err := a.allocate(context.Background(), "sandbox-a", 22, func(int) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	releasing, ok, err := a.beginRemove(context.Background(), "sandbox-a", 22)
	if err != nil || !ok {
		t.Fatalf("begin remove: ok=%v err=%v", ok, err)
	}
	stale := releasing
	stale.LeaseID = "different-lease"
	if err := a.finishRemove(context.Background(), []rawLease{stale}); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.index.Leases["20000"]; !ok {
		t.Fatal("stale cleanup removed a different lease")
	}
	if err := a.finishRemove(context.Background(), []rawLease{lease}); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.index.Leases["20000"]; ok {
		t.Fatal("matching lease was not removed")
	}
}
