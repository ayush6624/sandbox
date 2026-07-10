package gateway

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// liveGateway builds a gateway with the given hosts, all marked seen just now.
// Queueing is disabled so placement tests see reserveHost's immediate answer.
func liveGateway(hosts ...*host) *Gateway {
	g := New("tok", 20*time.Second, 0, 0)
	now := time.Now()
	for _, h := range hosts {
		h.lastSeen = now
		g.hosts[h.id] = h
	}
	return g
}

func TestPickHostBinPacks(t *testing.T) {
	// b has fewer free slots (fuller); bin-pack must prefer it so a and c can
	// drain to empty and become removable.
	g := liveGateway(
		&host{id: "a", slotsTotal: 24, slotsUsed: 0},  // free 24
		&host{id: "b", slotsTotal: 24, slotsUsed: 21}, // free 3
		&host{id: "c", slotsTotal: 24, slotsUsed: 10}, // free 14
	)
	if got := g.pickHost(); got == nil || got.id != "b" {
		t.Fatalf("bin-pack should pick fullest host b; got %v", got)
	}
}

func TestPickHostTieBreakByID(t *testing.T) {
	g := liveGateway(
		&host{id: "z", slotsTotal: 24, slotsUsed: 20}, // free 4
		&host{id: "m", slotsTotal: 24, slotsUsed: 20}, // free 4
	)
	if got := g.pickHost(); got == nil || got.id != "m" {
		t.Fatalf("tie should break to smaller id m; got %v", got)
	}
}

func TestPickHostSkipsFullAndStale(t *testing.T) {
	g := liveGateway(
		&host{id: "full", slotsTotal: 24, slotsUsed: 24}, // no capacity
	)
	stale := &host{id: "stale", slotsTotal: 24, slotsUsed: 0}
	stale.lastSeen = time.Now().Add(-time.Hour)
	g.hosts["stale"] = stale

	if got := g.pickHost(); got != nil {
		t.Fatalf("no live host has capacity; want nil, got %v", got)
	}
}

func TestReserveHostCapsAtCapacity(t *testing.T) {
	// Two hosts, 24 slots each = 48 total. Reserving 60 times (a burst larger
	// than capacity) must hand out exactly 48 hosts then nil — no host is
	// over-committed, because reservations count before creates complete.
	g := liveGateway(
		&host{id: "a", slotsTotal: 24, slotsUsed: 0},
		&host{id: "b", slotsTotal: 24, slotsUsed: 0},
	)
	got := 0
	for i := 0; i < 60; i++ {
		if g.reserveHost() != nil {
			got++
		}
	}
	if got != 48 {
		t.Fatalf("reserveHost should cap at 48 (2x24); got %d", got)
	}
	// Every reservation is accounted: both hosts full, zero free.
	for _, h := range g.hosts {
		if h.free() != 0 || h.reserved != 24 {
			t.Fatalf("host %s: reserved=%d free=%d, want reserved=24 free=0", h.id, h.reserved, h.free())
		}
	}
	// A failed create releases its reservation, freeing exactly one slot.
	g.release("a", false)
	if g.hosts["a"].free() != 1 {
		t.Fatalf("after failed release, host a free=%d want 1", g.hosts["a"].free())
	}
	// A landed create moves reserved->used, free stays 0.
	g.release("b", true)
	if h := g.hosts["b"]; h.free() != 0 || h.slotsUsed != 1 || h.reserved != 23 {
		t.Fatalf("after landed release, host b used=%d reserved=%d free=%d", h.slotsUsed, h.reserved, h.free())
	}
}

func TestAwaitHostReturnsWhenCapacityFrees(t *testing.T) {
	g := liveGateway(&host{id: "a", slotsTotal: 1, slotsUsed: 1})
	g.queueWait, g.queueMax = 5*time.Second, 8

	got := make(chan *host, 1)
	go func() { got <- g.awaitHost(context.Background()) }()

	// Free the slot after the waiter has queued; the next poll must pick it up.
	time.Sleep(50 * time.Millisecond)
	g.mu.Lock()
	g.hosts["a"].slotsUsed = 0
	g.mu.Unlock()

	select {
	case h := <-got:
		if h == nil || h.id != "a" {
			t.Fatalf("queued create should land on host a once freed; got %v", h)
		}
		if g.hosts["a"].reserved != 1 {
			t.Fatalf("awaitHost must return a RESERVED host; reserved=%d", g.hosts["a"].reserved)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued create never picked up the freed slot")
	}
	if n := g.queued.Load(); n != 0 {
		t.Fatalf("queue depth should drop back to 0, got %d", n)
	}
}

func TestAwaitHostTimesOutAndRespectsCancel(t *testing.T) {
	g := liveGateway(&host{id: "a", slotsTotal: 1, slotsUsed: 1})
	g.queueWait, g.queueMax = 300*time.Millisecond, 8

	// Full host, nothing frees: the wait must end at the deadline, empty-handed.
	if h := g.awaitHost(context.Background()); h != nil {
		t.Fatalf("expected timeout nil, got %v", h)
	}

	// A cancelled client (disconnect) must not keep occupying the queue.
	g.queueWait = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	if h := g.awaitHost(ctx); h != nil {
		t.Fatalf("expected nil on client cancel, got %v", h)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancel should end the wait immediately, not at the deadline")
	}
	if n := g.queued.Load(); n != 0 {
		t.Fatalf("queue depth should be 0 after exits, got %d", n)
	}
}

func TestAwaitHostQueueBounds(t *testing.T) {
	g := liveGateway(&host{id: "a", slotsTotal: 1, slotsUsed: 1})

	// Disabled queue: immediate nil.
	if h := g.awaitHost(context.Background()); h != nil {
		t.Fatalf("queueing disabled, want nil, got %v", h)
	}

	// Full queue: the waiter beyond queueMax is rejected immediately.
	g.queueWait, g.queueMax = time.Minute, 1
	g.queued.Store(1) // one waiter already queued
	start := time.Now()
	if h := g.awaitHost(context.Background()); h != nil {
		t.Fatalf("queue full, want nil, got %v", h)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("full queue must reject immediately, not wait out the deadline")
	}
	if n := g.queued.Load(); n != 1 {
		t.Fatalf("rejected waiter must not leak depth; got %d want 1", n)
	}
}

func TestMetricsExposition(t *testing.T) {
	g := liveGateway(
		&host{id: "h1", slotsTotal: 24, slotsUsed: 10},
		&host{id: "h2", slotsTotal: 24, slotsUsed: 5},
	)
	// A stale host must not inflate totals.
	stale := &host{id: "dead", slotsTotal: 24, slotsUsed: 24}
	stale.lastSeen = time.Now().Add(-time.Hour)
	g.hosts["dead"] = stale
	g.route["sb1"] = "h1"
	g.route["sb2"] = "h2"
	g.queued.Store(3)

	rr := httptest.NewRecorder()
	g.handleMetrics(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()

	want := []string{
		"sandbox_hosts_live 2",
		"sandbox_slots_total 48",
		"sandbox_slots_used 15",
		"sandbox_slots_free 33",
		"sandbox_routes 2",
		"sandbox_create_queue_depth 3",
		`sandbox_host_slots_used{host="h1"} 10`,
		`sandbox_host_slots_total{host="h2"} 24`,
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("metrics missing %q\n---\n%s", w, body)
		}
	}
	if strings.Contains(body, `host="dead"`) {
		t.Errorf("stale host should be excluded from metrics:\n%s", body)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("wrong content-type %q", ct)
	}
}
