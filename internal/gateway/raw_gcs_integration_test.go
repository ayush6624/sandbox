package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/registry"
)

// These exercise the durable raw allocator against REAL Google Cloud Storage.
// The unit tests use an in-memory CAS fake, which cannot prove that gcsblob's
// 404→ErrNotExist and 412→ErrPreconditionFailed mapping matches what GCS
// actually returns for this access pattern — and that mapping is the whole
// basis of the allocator's crash-safety.
//
// Run with:
//
//	SANDBOX_GCS_INGRESS_BUCKET=<bucket> go test ./internal/gateway/ -run GCS -v
func realStore(t *testing.T) (*gcsblob.Client, string) {
	t.Helper()
	bucket := os.Getenv("SANDBOX_GCS_INGRESS_BUCKET")
	if bucket == "" {
		t.Skip("set SANDBOX_GCS_INGRESS_BUCKET to run real-GCS ingress tests")
	}
	// gcsblob takes its credential from the GCE metadata server. Off-GCE, stand
	// up a local endpoint in that shape serving a real gcloud token, so the
	// requests below hit production GCS with the caller's own credentials.
	if os.Getenv("GCE_METADATA_HOST") == "" {
		out, err := exec.Command("gcloud", "auth", "print-access-token").Output()
		if err != nil {
			t.Skipf("gcloud auth print-access-token failed (run `gcloud auth login`): %v", err)
		}
		token := strings.TrimSpace(string(out))
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"access_token":%q,"expires_in":3000,"token_type":"Bearer"}`, token)
		}))
		t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(srv.URL, "http://"))
		t.Cleanup(srv.Close)
	}
	c := gcsblob.New(bucket)
	// Start from a clean index so assertions are absolute, not relative.
	if err := c.Delete(context.Background(), rawIndexObject); err != nil {
		t.Fatalf("clear index: %v", err)
	}
	return c, bucket
}

func newRealAllocator(t *testing.T, store rawStore, min, max int) *rawAllocator {
	t.Helper()
	a := &rawAllocator{
		cfg:   RawConfig{Bucket: "test", PublicHost: "tcp.example.com", PortMin: min, PortMax: max},
		store: store,
	}
	if err := a.load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	return a
}

// A missing index object must read as an empty, ready allocator rather than an
// error — that is the first-boot path on a fresh bucket.
func TestGCSAbsentIndexLoadsEmpty(t *testing.T) {
	store, _ := realStore(t)
	a := newRealAllocator(t, store, 20000, 20009)
	pending, active, releasing, gen := a.stats()
	if pending != 0 || active != 0 || releasing != 0 || gen != 0 {
		t.Fatalf("fresh index: pending=%d active=%d releasing=%d gen=%d", pending, active, releasing, gen)
	}
}

func TestGCSAllocateIsDurableAndIdempotent(t *testing.T) {
	store, _ := realStore(t)
	ctx := context.Background()
	a := newRealAllocator(t, store, 20000, 20009)

	lease, err := a.allocate(ctx, "sb-1", 22, func(int) error { return nil })
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if lease.State != "active" || lease.PublicPort < 20000 || lease.PublicPort > 20009 {
		t.Fatalf("lease = %+v", lease)
	}

	// Same (sandbox, guest port) must return the identical lease, and must not
	// call assign again.
	again, err := a.allocate(ctx, "sb-1", 22, func(int) error {
		t.Fatal("assign called on an already-active lease")
		return nil
	})
	if err != nil {
		t.Fatalf("re-allocate: %v", err)
	}
	if again.PublicPort != lease.PublicPort || again.LeaseID != lease.LeaseID {
		t.Fatalf("not idempotent: %+v vs %+v", again, lease)
	}

	// A second gateway process reading the bucket must see the committed lease.
	fresh := newRealAllocator(t, store, 20000, 20009)
	got, ok := fresh.route(lease.PublicPort)
	if !ok || got.SandboxID != "sb-1" || got.GuestPort != 22 {
		t.Fatalf("durable read-back: lease=%+v ok=%v", got, ok)
	}
}

// A failed worker commit must leave no lease behind, or the public port leaks.
func TestGCSAllocateRollsBackFailedAssign(t *testing.T) {
	store, _ := realStore(t)
	ctx := context.Background()
	a := newRealAllocator(t, store, 20000, 20009)

	assignErr := errors.New("worker rejected the mapping")
	if _, err := a.allocate(ctx, "sb-1", 22, func(int) error { return assignErr }); !errors.Is(err, assignErr) {
		t.Fatalf("allocate error = %v, want %v", err, assignErr)
	}
	if pending, active, releasing, _ := a.stats(); pending != 0 || active != 0 || releasing != 0 {
		t.Fatalf("rollback left state: pending=%d active=%d releasing=%d", pending, active, releasing)
	}
	fresh := newRealAllocator(t, store, 20000, 20009)
	if pending, active, releasing, _ := fresh.stats(); pending != 0 || active != 0 || releasing != 0 {
		t.Fatalf("rollback not durable: pending=%d active=%d releasing=%d", pending, active, releasing)
	}
}

// Two independent allocators sharing one bucket must never hand out the same
// public port. This is the real 412 retry path.
func TestGCSConcurrentAllocatorsNeverDoubleAssign(t *testing.T) {
	store, _ := realStore(t)
	ctx := context.Background()
	const n = 8
	allocators := make([]*rawAllocator, 3)
	for i := range allocators {
		allocators[i] = newRealAllocator(t, store, 20000, 20000+n-1)
	}

	var mu sync.Mutex
	ports := map[int]string{}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("sb-%d", i)
			lease, err := allocators[i%len(allocators)].allocate(ctx, id, 22, func(int) error { return nil })
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if prev, dup := ports[lease.PublicPort]; dup {
				errs <- fmt.Errorf("port %d assigned to both %s and %s", lease.PublicPort, prev, id)
				return
			}
			ports[lease.PublicPort] = id
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent allocate: %v", err)
	}
	if len(ports) != n {
		t.Fatalf("allocated %d distinct ports, want %d", len(ports), n)
	}

	// And the durable index agrees with what callers were told.
	fresh := newRealAllocator(t, store, 20000, 20000+n-1)
	if _, active, _, _ := fresh.stats(); active != n {
		t.Fatalf("durable active leases = %d, want %d", active, n)
	}
	for port, id := range ports {
		got, ok := fresh.route(port)
		if !ok || got.SandboxID != id {
			t.Fatalf("port %d routes to %+v, want sandbox %s", port, got, id)
		}
	}
}

// Exhaustion must be reported as capacity, not as a generic failure.
func TestGCSAllocateReportsPoolExhaustion(t *testing.T) {
	store, _ := realStore(t)
	ctx := context.Background()
	a := newRealAllocator(t, store, 20000, 20001)
	for i, id := range []string{"sb-1", "sb-2"} {
		if _, err := a.allocate(ctx, id, 22, func(int) error { return nil }); err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
	}
	_, err := a.allocate(ctx, "sb-3", 22, func(int) error { return nil })
	if !errors.Is(err, registry.ErrPoolExhausted) {
		t.Fatalf("exhaustion error = %v, want registry.ErrPoolExhausted", err)
	}
}

// The full release path must free the port for reuse and be crash-safe in
// between (releasing is observable, then gone).
func TestGCSReleaseFreesPortForReuse(t *testing.T) {
	store, _ := realStore(t)
	ctx := context.Background()
	a := newRealAllocator(t, store, 20000, 20000) // exactly one port

	first, err := a.allocate(ctx, "sb-1", 22, func(int) error { return nil })
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	leases, ok, err := a.beginRemove(ctx, "sb-1", 22)
	if err != nil || !ok {
		t.Fatalf("beginRemove: ok=%v err=%v", ok, err)
	}
	if _, _, releasing, _ := a.stats(); releasing != 1 {
		t.Fatalf("releasing lease not observable")
	}
	// A releasing lease must not be routable.
	if _, routable := a.route(first.PublicPort); routable {
		t.Fatal("releasing lease is still routable")
	}
	if err := a.finishRemove(ctx, []rawLease{leases}); err != nil {
		t.Fatalf("finishRemove: %v", err)
	}

	reused, err := a.allocate(ctx, "sb-2", 22, func(int) error { return nil })
	if err != nil {
		t.Fatalf("reallocate after release: %v", err)
	}
	if reused.PublicPort != first.PublicPort {
		t.Fatalf("port not reused: %d then %d", first.PublicPort, reused.PublicPort)
	}
	if reused.LeaseID == first.LeaseID {
		t.Fatal("reused port kept the old lease id; stale cleanup could release the new tenant")
	}
}
