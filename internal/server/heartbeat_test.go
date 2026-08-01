package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/cluster"
)

func TestWarmCompletionSendsImmediateHeartbeat(t *testing.T) {
	heartbeats := make(chan cluster.Heartbeat, 3)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var hb cluster.Heartbeat
		if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
			t.Errorf("decode heartbeat: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		heartbeats <- hb
		w.WriteHeader(http.StatusNoContent)
	}))
	defer gateway.Close()

	s, _ := capacityTestServer(t)
	s.cfg.GatewayURL = gateway.URL
	s.cfg.AdvertiseAddr = "worker:8080"
	s.cfg.HostID = "worker-1"
	s.cfg.WorkerRelease = "release-2"
	s.cfg.WarmPoolSize = 1
	s.warmed = make(chan struct{})
	s.readyPoolSettled = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.heartbeat(ctx)

	select {
	case hb := <-heartbeats:
		if hb.Release != "release-2" {
			t.Fatalf("release = %q, want release-2", hb.Release)
		}
		if hb.SlotsFree == nil || *hb.SlotsFree != 0 {
			t.Fatalf("initial slots_free = %v, want 0 while warming", hb.SlotsFree)
		}
	case <-time.After(time.Second):
		t.Fatal("initial heartbeat not received")
	}

	start := time.Now()
	close(s.warmed)
	select {
	case hb := <-heartbeats:
		if hb.SlotsFree == nil || *hb.SlotsFree != 0 {
			t.Fatalf("golden-only slots_free = %v, want 0 until ready pool settles", hb.SlotsFree)
		}
		if elapsed := time.Since(start); elapsed >= heartbeatInterval {
			t.Fatalf("warm heartbeat waited for periodic tick: %s", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("warm completion did not send an immediate heartbeat")
	}

	start = time.Now()
	s.settleReadyPool()
	select {
	case hb := <-heartbeats:
		if hb.SlotsFree == nil || *hb.SlotsFree != 3 {
			t.Fatalf("ready-pool slots_free = %v, want 3", hb.SlotsFree)
		}
		if elapsed := time.Since(start); elapsed >= heartbeatInterval {
			t.Fatalf("ready-pool heartbeat waited for periodic tick: %s", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("ready-pool completion did not send an immediate heartbeat")
	}
}

func TestPlacementDelayUsesLinuxBootAgeAfterWarmGate(t *testing.T) {
	s, _ := capacityTestServer(t)
	s.cfg.PlacementDelay = 210 * time.Second

	// A genuinely fresh refill worker is fully initialized but remains
	// unplaceable until after the MIG's 180s standby initial delay plus 30s
	// safety headroom.
	s.bootAge = func() (time.Duration, error) { return 90 * time.Second, nil }
	if got := s.advertisedFreeSlots(3); got != 0 {
		t.Fatalf("fresh boot advertised %d free slots, want 0", got)
	}

	// Linux /proc/uptime uses boottime, which includes time spent suspended.
	// A resumed suspended standby therefore clears the gate immediately.
	s.bootAge = func() (time.Duration, error) { return 20 * time.Minute, nil }
	if got := s.advertisedFreeSlots(3); got != 3 {
		t.Fatalf("resumed old boot advertised %d free slots, want 3", got)
	}

	// Placement age never bypasses golden warm-up: both gates must be open.
	s.warmed = make(chan struct{})
	if got := s.advertisedFreeSlots(3); got != 0 {
		t.Fatalf("old but unwarmed boot advertised %d free slots, want 0", got)
	}
	close(s.warmed)
	if got := s.advertisedFreeSlots(3); got != 3 {
		t.Fatalf("old warm boot advertised %d free slots, want 3", got)
	}
}

func TestPlacementDelayFailsClosedWhenBootAgeUnavailable(t *testing.T) {
	s, _ := capacityTestServer(t)
	s.cfg.PlacementDelay = time.Second
	s.bootAge = func() (time.Duration, error) {
		return 0, context.DeadlineExceeded
	}
	if got := s.advertisedFreeSlots(3); got != 0 {
		t.Fatalf("boot-age error advertised %d free slots, want 0", got)
	}
}

func TestReadyPoolGateHoldsCapacityUntilInitialPoolSettles(t *testing.T) {
	s, _ := capacityTestServer(t)
	s.cfg.WarmPoolSize = 2
	s.readyPoolSettled = make(chan struct{})

	if got := s.advertisedFreeSlots(3); got != 0 {
		t.Fatalf("unsettled ready pool advertised %d free slots, want 0", got)
	}
	s.settleReadyPool()
	if got := s.advertisedFreeSlots(3); got != 3 {
		t.Fatalf("settled ready pool advertised %d free slots, want 3", got)
	}
}

// TestHeartbeatSamplesPublicRoutesInOneQuery pins the O(1) shape of the
// heartbeat's route sampling. It used to call Ports() once per routed sandbox,
// so a host's control-plane cost grew with its inventory: N registry round
// trips every 5 s, on the same database creates depend on.
func TestHeartbeatSamplesPublicRoutesInOneQuery(t *testing.T) {
	s, reg := capacityTestServer(t)
	ctx := context.Background()

	// Three routed sandboxes with a public port each, one of them hibernated
	// (hibernated sandboxes stay routable so this host can wake them), plus a
	// mapping with no public port that must not be advertised.
	want := map[int]cluster.RawPortRoute{}
	for i, id := range []string{"a", "b", "c"} {
		if _, err := reg.Create(ctx, id, "", "/tmp/"+id+".ext4", nil, "", 0, 0, 0); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		guestPort := 3000 + i
		if _, err := reg.AddURLPort(ctx, id, guestPort); err != nil {
			t.Fatalf("expose %s: %v", id, err)
		}
		public := 20000 + i
		if err := reg.SetPublicPort(ctx, id, guestPort, public); err != nil {
			t.Fatalf("set public port %s: %v", id, err)
		}
		want[public] = cluster.RawPortRoute{PublicPort: public, SandboxID: id, GuestPort: guestPort}
	}
	if _, err := reg.AddURLPort(ctx, "a", 4000); err != nil {
		t.Fatalf("expose unrouted port: %v", err)
	}
	if err := reg.Hibernate(ctx, "c"); err != nil {
		t.Fatalf("hibernate c: %v", err)
	}

	got := make(chan cluster.Heartbeat, 1)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var hb cluster.Heartbeat
		if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
			t.Errorf("decode heartbeat: %v", err)
		}
		got <- hb
		w.WriteHeader(http.StatusNoContent)
	}))
	defer gateway.Close()

	beforeRoutes, beforePorts := reg.PortReadCounts()
	s.sendHeartbeat(ctx, &http.Client{Timeout: 5 * time.Second}, gateway.URL, "worker-1", "worker:8080")
	afterRoutes, afterPorts := reg.PortReadCounts()

	if n := afterRoutes - beforeRoutes; n != 1 {
		t.Fatalf("heartbeat issued %d public-route queries, want exactly 1", n)
	}
	if n := afterPorts - beforePorts; n != 0 {
		t.Fatalf("heartbeat issued %d per-sandbox Ports queries, want 0 (that loop is O(N))", n)
	}

	select {
	case hb := <-got:
		if len(hb.RawRoutes) != len(want) {
			t.Fatalf("advertised %d routes, want %d: %+v", len(hb.RawRoutes), len(want), hb.RawRoutes)
		}
		for _, r := range hb.RawRoutes {
			if w, ok := want[r.PublicPort]; !ok || w != r {
				t.Fatalf("unexpected route %+v", r)
			}
		}
		// Every advertised route must belong to an advertised sandbox, or the
		// gateway keeps a route it cannot resolve to a host.
		ids := map[string]bool{}
		for _, id := range hb.SandboxIDs {
			ids[id] = true
		}
		for _, r := range hb.RawRoutes {
			if !ids[r.SandboxID] {
				t.Fatalf("route %+v advertised for a sandbox absent from SandboxIDs %v", r, hb.SandboxIDs)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat not received")
	}
}
