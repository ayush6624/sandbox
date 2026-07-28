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
