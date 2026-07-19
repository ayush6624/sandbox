package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
