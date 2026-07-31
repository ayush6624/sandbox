package gateway

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// slowRawStore models the real store: every commit is a GCS round trip.
type slowRawStore struct {
	inner memoryRawStore
	delay time.Duration
}

func (s *slowRawStore) GetBytesGen(ctx context.Context, name string) ([]byte, int64, error) {
	time.Sleep(s.delay)
	return s.inner.GetBytesGen(ctx, name)
}

func (s *slowRawStore) PutBytesIfGenerationMatch(ctx context.Context, name string, b []byte, gen int64) (int64, error) {
	time.Sleep(s.delay)
	return s.inner.PutBytesIfGenerationMatch(ctx, name, b, gen)
}

// rawAllocator.allocate holds the single allocator mutex across GCS
// commits, the worker assign() call, and its jittered CAS backoff. route() —
// which runs for EVERY inbound public raw TCP connection — takes that same
// mutex, so an in-flight exposure stalls the data plane.
func TestRawRouteDoesNotBlockBehindAllocation(t *testing.T) {
	a := &rawAllocator{
		cfg:    RawConfig{PortMin: 20000, PortMax: 20010, PublicHost: "h", Bucket: "b"},
		store:  &slowRawStore{delay: 150 * time.Millisecond},
		loaded: true,
		index:  rawIndex{Version: 1, Leases: map[string]rawLease{}},
	}
	// Pre-seed an active lease so route() has something real to answer with.
	a.index.Leases[strconv.Itoa(20010)] = rawLease{
		PublicPort: 20010, SandboxID: "sb-live", GuestPort: 22, State: "active",
	}
	a.publishLocked() // production publishes at construction/load

	// Baseline: uncontended route latency.
	t0 := time.Now()
	if _, ok := a.route(20010); !ok {
		t.Fatal("seeded lease should route")
	}
	baseline := time.Since(t0)

	started := make(chan struct{})
	go func() {
		close(started)
		// assign() stands in for the HTTP call to the owning worker.
		_, _ = a.allocate(context.Background(), "sb-new", 8080, func(int) error {
			time.Sleep(200 * time.Millisecond)
			return nil
		})
	}()
	<-started
	time.Sleep(20 * time.Millisecond) // let allocate() take the mutex

	t1 := time.Now()
	a.route(20010)
	blocked := time.Since(t1)

	t.Logf("route latency: baseline=%v, during allocation=%v", baseline, blocked)
	if blocked > 50*time.Millisecond {
		t.Fatalf("DATA PLANE STALL CONFIRMED: route() blocked %v behind an in-flight allocation (baseline %v)", blocked, baseline)
	}
}
