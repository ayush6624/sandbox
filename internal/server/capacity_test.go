package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

// capacityTestServer builds a server whose provisioner works over temp dirs
// (a tiny file stands in for the base rootfs), so handleCreate runs for real
// up to the registry allocation — where pool exhaustion is raised.
func capacityTestServer(t *testing.T) (*Server, *registry.Registry) {
	t.Helper()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	if err := os.WriteFile(base, []byte("rootfs"), 0o644); err != nil {
		t.Fatalf("write base rootfs: %v", err)
	}
	reg, err := registry.Open(filepath.Join(dir, "registry.db"), registry.Pools{
		TapPrefix:  "fc",
		TapMax:     3,
		GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.0.12",
		PortMin:    5200,
		PortMax:    5202,
	})
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })
	s := New(Config{
		Provisioner: &provisioner.Provisioner{
			RootfsBase:  base,
			RootfsDir:   filepath.Join(dir, "rootfs"),
			SnapshotDir: filepath.Join(dir, "snapshots"),
		},
	}, reg)
	return s, reg
}

func TestCreateReturns503OnPoolExhaustion(t *testing.T) {
	s, reg := capacityTestServer(t)
	ctx := context.Background()

	// Exhaust the pool via the registry directly (no VMs involved).
	for _, id := range []string{"a", "b", "c"} {
		if _, err := reg.Create(ctx, id, "", "/tmp/"+id+".ext4", nil, "", 0, 0, 0); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	w := httptest.NewRecorder()
	s.handleCreate(w, httptest.NewRequest("POST", "/sandboxes", strings.NewReader(`{}`)))
	if w.Code != 503 {
		t.Fatalf("create on a full pool: got %d, want 503 (body: %s)", w.Code, w.Body)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("capacity 503 must carry Retry-After")
	}
	if !strings.Contains(w.Body.String(), "pool exhausted") {
		t.Fatalf("error should say which pool is exhausted: %s", w.Body)
	}
}

func TestCreateReturns503WhenMemBudgetExceeded(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	if err := os.WriteFile(base, []byte("rootfs"), 0o644); err != nil {
		t.Fatalf("write base rootfs: %v", err)
	}
	reg, err := registry.Open(filepath.Join(dir, "registry.db"), registry.Pools{
		TapPrefix: "fc", TapMax: 8,
		GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.17",
		PortMin: 5200, PortMax: 5207,
	})
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })
	s := New(Config{
		Provisioner: &provisioner.Provisioner{
			RootfsBase: base,
			RootfsDir:  filepath.Join(dir, "rootfs"),
		},
		VMTemplate:   vm.RunOptions{Vcpus: 2, MemMIB: 1024},
		MemBudgetMIB: 4096,
	}, reg)

	// Seed a big-mem sandbox: 3000 + 156 overhead = 3156 of 4096 committed.
	ctx := context.Background()
	if _, err := reg.Create(ctx, "big", "", "/tmp/big.ext4", nil, "", 0, 0, 3000); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	// A template-sized create (1024+156=1180) would hit 4336 > 4096 → 503.
	w := httptest.NewRecorder()
	s.handleCreate(w, httptest.NewRequest("POST", "/sandboxes", strings.NewReader(`{}`)))
	if w.Code != 503 {
		t.Fatalf("create beyond memory budget: got %d, want 503 (body: %s)", w.Code, w.Body)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("memory-budget 503 must carry Retry-After")
	}
	if !strings.Contains(w.Body.String(), "memory budget") {
		t.Fatalf("error should name the memory budget: %s", w.Body)
	}

	// An override that can NEVER fit (> budget − overhead) 400s up front
	// instead of burning gateway failover attempts.
	w = httptest.NewRecorder()
	s.handleCreate(w, httptest.NewRequest("POST", "/sandboxes", strings.NewReader(`{"mem_mib": 4000}`)))
	if w.Code != 400 {
		t.Fatalf("unfittable mem_mib override: got %d, want 400 (body: %s)", w.Code, w.Body)
	}
}

// TestStatusForCapacityWake pins the wake-rejection surface: ensureRunning
// errors wrapping ErrPoolExhausted (a memory- or pool-rejected wake) must map
// to 503 so agent-bound requests read as capacity, not server failure.
func TestStatusForCapacityWake(t *testing.T) {
	err := fmt.Errorf("wake sandbox x: %w", registry.ErrMemExhausted)
	if got := statusFor(err); got != 503 {
		t.Fatalf("statusFor(rejected wake) = %d, want 503", got)
	}
	if got := statusFor(fmt.Errorf("boom")); got != 500 {
		t.Fatalf("statusFor(generic) = %d, want 500", got)
	}
}

// TestFanoutRejectsCountAboveCap: fanout sizes its own work (a registry TX, a
// rootfs clone, a VMM and a 30 s agent wait per clone, under the snapshot lock),
// so the count is capped at the same 100 the v1 batch endpoint enforces.
func TestFanoutRejectsCountAboveCap(t *testing.T) {
	s, _ := capacityTestServer(t)
	for _, body := range []string{`{"count": 101}`, `{"count": 100000}`, `{"count": 0}`} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/snapshots/snap-1/fanout", strings.NewReader(body))
		r.SetPathValue("id", "snap-1")
		s.handleFanout(w, r)
		if w.Code != 400 {
			t.Fatalf("fanout %s: got %d, want 400 (body: %s)", body, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "between 1 and 100") {
			t.Fatalf("fanout %s: error should name the cap: %s", body, w.Body)
		}
	}
}

// TestFanoutFailsFastBeyondFreeCapacity: a batch the host cannot hold must be
// refused as capacity (503 + Retry-After, so the gateway/SDK back off) instead
// of booting and tearing down clones a wave at a time while holding the
// snapshot lock against restores and deletes.
func TestFanoutFailsFastBeyondFreeCapacity(t *testing.T) {
	s, reg := capacityTestServer(t)
	ctx := context.Background()
	if err := reg.CreateSnapshot(ctx, registry.Snapshot{ID: "snap-1", SourceID: "gone", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	// Pools hold 3; ask for 4.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/snapshots/snap-1/fanout", strings.NewReader(`{"count": 4}`))
	r.SetPathValue("id", "snap-1")
	s.handleFanout(w, r)
	if w.Code != 503 {
		t.Fatalf("fanout beyond free capacity: got %d, want 503 (body: %s)", w.Code, w.Body)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("capacity 503 must carry Retry-After")
	}
	if len(s.createSem) != 0 {
		t.Fatalf("fail-fast must not retain create permits, held %d", len(s.createSem))
	}
}

// Public restore is a new sandbox, not a claim on the source sandbox's old
// host identity. Pool resources are intentionally reused after destroy, so a
// snapshot that insists on its baked tap/IP becomes randomly unrestorable as
// soon as ordinary traffic takes either resource.
func TestRestoreDoesNotReclaimBakedNetworkIdentity(t *testing.T) {
	s, reg := capacityTestServer(t)
	ctx := context.Background()
	dir := t.TempDir()
	sourceRootfs := filepath.Join(dir, "source.ext4")
	if err := os.WriteFile(sourceRootfs, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := reg.Create(ctx, "source", "", sourceRootfs, nil, "", 0, 1, 512)
	if err != nil {
		t.Fatalf("create source identity: %v", err)
	}
	mem, state, frozen, err := s.cfg.Provisioner.SnapshotPaths("snap-identity")
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{mem: "memory", state: "state", frozen: "frozen-rootfs"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.CreateSnapshot(ctx, registry.Snapshot{
		ID: "snap-identity", SourceID: source.ID, TapDevice: source.TapDevice,
		GuestIP: source.GuestIP, MemPath: mem, StatePath: state,
		RootfsPath: frozen, SourceRootfsPath: sourceRootfs, CreatedAt: time.Now(),
		Format: registry.FormatFull, Vcpus: 1, MemMIB: 512,
	}); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/snapshots/snap-identity/restore", strings.NewReader(`{}`))
	r.SetPathValue("id", "snap-identity")
	s.handleRestore(w, r)
	// The fake test host cannot launch Firecracker, so the request eventually
	// fails during tap/VM bring-up. It must get past identity allocation: the old
	// implementation returned 409 here before doing any bring-up work.
	if w.Code == 409 || strings.Contains(w.Body.String(), "source sandbox still running") {
		t.Fatalf("restore tried to reclaim baked identity: status=%d body=%s", w.Code, w.Body)
	}
}

// TestFanoutWaitsForCreateBudget: fanout bring-ups run on the same host-wide
// create budget as handleCreate and handleRestore — it used to bypass createSem
// entirely, so one call could boot-storm a host already at its ceiling. The
// permit is taken before any snapshot work, so a client that disconnects while
// queued costs nothing.
func TestFanoutWaitsForCreateBudget(t *testing.T) {
	s, reg := capacityTestServer(t)
	if err := reg.CreateSnapshot(context.Background(), registry.Snapshot{ID: "snap-1", SourceID: "gone", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	s.createSem = make(chan struct{}, 1)
	if err := s.acquireCreate(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/snapshots/snap-1/fanout", strings.NewReader(`{"count": 1}`)).WithContext(ctx)
	r.SetPathValue("id", "snap-1")
	start := time.Now()
	s.handleFanout(w, r)
	if time.Since(start) < 25*time.Millisecond {
		t.Fatal("fanout should have blocked on the create semaphore until cancel")
	}
	if w.Code != 499 {
		t.Fatalf("cancelled queued fanout: got %d, want 499 (body: %s)", w.Code, w.Body)
	}
}

// TestFanoutPhaseTwoIsBounded: phase 2 (announce wait, bridge, 30 s agent wait
// per clone) used to spawn one unbounded goroutine per clone, undoing the pacing
// phase 1 applied. Both phases now share the batch's permit count.
func TestFanoutPhaseTwoIsBounded(t *testing.T) {
	s, _ := capacityTestServer(t)
	const limit = 3
	var mu sync.Mutex
	inFlight, peak := 0, 0
	s.finishCloneFn = func(context.Context, *clone) error {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond) // hold the slot like a real agent wait
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	}

	clones := make([]*clone, 24)
	for i := range clones {
		clones[i] = &clone{sb: registry.Sandbox{ID: fmt.Sprintf("clone-%d", i)}}
	}
	clones[7] = nil                                        // never allocated
	clones[9] = &clone{err: fmt.Errorf("bring-up failed")} // failed phase 1

	live := s.finishClones(context.Background(), "snap-1", clones, limit)
	if len(live) != len(clones)-2 {
		t.Fatalf("finished %d clones, want %d", len(live), len(clones)-2)
	}
	if peak > limit {
		t.Fatalf("phase 2 ran %d clones concurrently, limit is %d", peak, limit)
	}
	if peak < 2 {
		t.Fatalf("phase 2 ran serially (peak %d) — the bound must not remove parallelism", peak)
	}
}

// TestRunBoundedRespectsLimit exercises the shared fan-out helper directly,
// including the degenerate limit both phases would hit if the permit count were
// ever computed as zero.
func TestRunBoundedRespectsLimit(t *testing.T) {
	for _, limit := range []int{0, 1, 4} {
		var mu sync.Mutex
		inFlight, peak, done := 0, 0, 0
		runBounded(limit, 40, func(int) {
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			inFlight--
			done++
			mu.Unlock()
		})
		want := limit
		if want < 1 {
			want = 1
		}
		if peak > want {
			t.Fatalf("limit %d: peak concurrency %d", limit, peak)
		}
		if done != 40 {
			t.Fatalf("limit %d: ran %d of 40", limit, done)
		}
	}
}

func TestCreateSemaphoreQueuesAndRespectsCancel(t *testing.T) {
	s, _ := capacityTestServer(t)
	s.createSem = make(chan struct{}, 1)

	// Occupy the only bring-up slot.
	if err := s.acquireCreate(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// A second create must block in the queue, then fail out when its client
	// disconnects — without ever starting disk/registry work.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/sandboxes", strings.NewReader(`{}`)).WithContext(ctx)
	start := time.Now()
	s.handleCreate(w, r)
	if time.Since(start) < 25*time.Millisecond {
		t.Fatal("create should have blocked on the semaphore until cancel")
	}
	if w.Code != 499 {
		t.Fatalf("cancelled queued create: got %d, want 499 (body: %s)", w.Code, w.Body)
	}

	// Freeing the slot lets the next create proceed (to pool allocation).
	s.releaseCreate()
	if err := s.acquireCreate(context.Background()); err != nil {
		t.Fatalf("semaphore should be free again: %v", err)
	}
	s.releaseCreate()
}
