package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

// liveGateway builds a gateway with the given hosts, all marked seen just now.
// Queueing is disabled so placement tests see reserveHost's immediate answer.
// Hosts whose literals don't set slotsFree get the old-binary fallback
// (total-used), mirroring handleRegister.
func liveGateway(hosts ...*host) *Gateway {
	g := New("tok", 20*time.Second, 0, 0)
	now := time.Now()
	for _, h := range hosts {
		h.lastSeen = now
		if h.slotsFree == 0 && h.slotsUsed < h.slotsTotal {
			h.slotsFree = h.slotsTotal - h.slotsUsed
		}
		g.hosts[h.id] = h
	}
	return g
}

// queueDeadline mirrors handleCreate's shared-deadline computation for tests
// that call awaitHost directly.
func (g *Gateway) queueDeadline() time.Time { return time.Now().Add(g.queueWait) }

func TestHostOnlyNormalizesAdvertisedManagementAddress(t *testing.T) {
	tests := map[string]string{
		"10.160.0.62:8080":         "10.160.0.62",
		"http://10.160.0.62:8080":  "10.160.0.62",
		"https://worker.test:8443": "worker.test",
		"[fd00::62]:8080":          "[fd00::62]",
		"https://[fd00::62]:8443":  "[fd00::62]",
		"worker-without-port.test": "worker-without-port.test",
	}
	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			if got := hostOnly(input); got != want {
				t.Fatalf("hostOnly(%q) = %q, want %q", input, got, want)
			}
		})
	}
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
		if g.reserveHost(nil) != nil {
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
	// A landed create releases its reservation, optimistically advances used,
	// and debits advertised free.
	g.release("b", true)
	if h := g.hosts["b"]; h.free() != 0 || h.slotsUsed != 1 || h.reserved != 23 {
		t.Fatalf("after landed release, host b used=%d reserved=%d free=%d", h.slotsUsed, h.reserved, h.free())
	}
}

func TestLandedCreatesCapOptimisticUsedAfterHeartbeatRace(t *testing.T) {
	g := liveGateway(&host{id: "worker", slotsTotal: 48, slotsUsed: 0})

	reservations := make([]*host, 48)
	for i := range reservations {
		reservations[i] = g.reserveHost(nil)
		if reservations[i] == nil {
			t.Fatalf("reservation %d unexpectedly failed", i)
		}
	}

	// Half the creates have committed to the worker registry, but none of their
	// HTTP responses have returned yet. This is the race seen during the
	// autoscaling burst: the heartbeat includes those 24 while all 48 gateway
	// reservations are still outstanding.
	g.mu.Lock()
	g.hosts["worker"].slotsUsed = 24
	g.hosts["worker"].slotsFree = 24
	g.mu.Unlock()

	for i, reserved := range reservations {
		g.landReservation(reserved, fmt.Sprintf("sandbox-%d", i))
	}

	h := g.hosts["worker"]
	if h.slotsUsed != 48 {
		t.Fatalf("optimistic occupancy escaped physical capacity: slotsUsed=%d, want 48", h.slotsUsed)
	}
	if h.reserved != 0 || h.free() != 0 {
		t.Fatalf("after landings reserved=%d free=%d, want 0/0", h.reserved, h.free())
	}

	// The next worker heartbeat supplies the exact occupancy.
	g.mu.Lock()
	h.slotsUsed = 48
	h.slotsFree = 0
	g.mu.Unlock()
	if h.slotsUsed != h.slotsTotal {
		t.Fatalf("settled occupancy used=%d total=%d", h.slotsUsed, h.slotsTotal)
	}
}

func TestAwaitHostReturnsWhenCapacityFrees(t *testing.T) {
	g := liveGateway(&host{id: "a", slotsTotal: 1, slotsUsed: 1})
	g.queueWait, g.queueMax = 5*time.Second, 8

	got := make(chan *host, 1)
	go func() { got <- g.awaitHost(context.Background(), g.queueDeadline(), nil) }()

	// Free the slot after the waiter has queued (as a heartbeat reporting new
	// capacity would); the next poll must pick it up.
	time.Sleep(50 * time.Millisecond)
	g.mu.Lock()
	g.hosts["a"].slotsUsed = 0
	g.hosts["a"].slotsFree = 1
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

func TestCapacityNotificationWakesAllQueuedCreates(t *testing.T) {
	g := liveGateway(&host{id: "a", slotsTotal: 3, slotsUsed: 3})
	g.queueWait, g.queueMax = 5*time.Second, 8

	got := make(chan *host, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 3; i++ {
		go func() { got <- g.awaitHost(ctx, g.queueDeadline(), nil) }()
	}
	deadline := time.Now().Add(time.Second)
	for g.queued.Load() != 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if depth := g.queued.Load(); depth != 3 {
		t.Fatalf("queued depth = %d, want 3", depth)
	}

	g.mu.Lock()
	g.hosts["a"].slotsUsed = 0
	g.hosts["a"].slotsFree = 3
	g.mu.Unlock()
	g.notifySlotFreed()

	for i := 0; i < 3; i++ {
		select {
		case h := <-got:
			if h == nil || h.id != "a" {
				t.Fatalf("waiter %d got host %v, want a", i, h)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("waiter %d was not woken by capacity broadcast", i)
		}
	}
}

func TestAwaitHostTimesOutAndRespectsCancel(t *testing.T) {
	g := liveGateway(&host{id: "a", slotsTotal: 1, slotsUsed: 1})
	g.queueWait, g.queueMax = 300*time.Millisecond, 8

	// Full host, nothing frees: the wait must end at the deadline, empty-handed.
	if h := g.awaitHost(context.Background(), g.queueDeadline(), nil); h != nil {
		t.Fatalf("expected timeout nil, got %v", h)
	}

	// A cancelled client (disconnect) must not keep occupying the queue.
	g.queueWait = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	if h := g.awaitHost(ctx, g.queueDeadline(), nil); h != nil {
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
	if h := g.awaitHost(context.Background(), g.queueDeadline(), nil); h != nil {
		t.Fatalf("queueing disabled, want nil, got %v", h)
	}

	// Full queue: the waiter beyond queueMax is rejected immediately.
	g.queueWait, g.queueMax = time.Minute, 1
	g.queued.Store(1) // one waiter already queued
	start := time.Now()
	if h := g.awaitHost(context.Background(), g.queueDeadline(), nil); h != nil {
		t.Fatalf("queue full, want nil, got %v", h)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("full queue must reject immediately, not wait out the deadline")
	}
	if n := g.queued.Load(); n != 1 {
		t.Fatalf("rejected waiter must not leak depth; got %d want 1", n)
	}
}

type fakeDirectScaler struct {
	desired chan int
}

func (s *fakeDirectScaler) ScaleOut(_ context.Context, desired int) error {
	s.desired <- desired
	return nil
}

func TestQueueTransitionTriggersOneDirectScaleOutForBurst(t *testing.T) {
	g := liveGateway(
		&host{id: "a", slotsTotal: 2, slotsUsed: 0, slotsFree: 2, reserved: 2},
		&host{id: "b", slotsTotal: 2, slotsUsed: 0, slotsFree: 2, reserved: 2},
	)
	g.queueWait, g.queueMax = time.Minute, 8
	scaler := &fakeDirectScaler{desired: make(chan int, 2)}
	if err := g.ConfigureDirectScaleOut(scaler, 2, 2); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 3; i++ {
		go g.awaitHost(ctx, g.queueDeadline(), nil)
	}

	select {
	case desired := <-scaler.desired:
		// 4 reservations + 3 queued + 2 headroom = 9 slots => 5 workers.
		if desired != 5 {
			t.Fatalf("direct desired workers = %d, want 5", desired)
		}
	case <-time.After(time.Second):
		t.Fatal("queue transition did not trigger direct scale-out")
	}
	select {
	case desired := <-scaler.desired:
		t.Fatalf("one burst triggered a second direct scale-out to %d", desired)
	case <-time.After(2 * directScaleDebounce):
	}
}

// TestFreeUsesSlotsFreeNotTotalMinusUsed is the "hibernation port black hole"
// regression: a host whose hibernated sandboxes hold every spare port reports
// slots_free=0 even though total-used looks roomy. Placement must never pick
// it — before the fix, bin-pack re-picked it forever and every create 502'd.
func TestFreeUsesSlotsFreeNotTotalMinusUsed(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	now := time.Now()
	poisoned := &host{id: "poisoned", slotsTotal: 24, slotsUsed: 4, slotsFree: 0, lastSeen: now}
	healthy := &host{id: "healthy", slotsTotal: 24, slotsUsed: 4, slotsFree: 20, lastSeen: now}
	g.hosts["poisoned"] = poisoned
	g.hosts["healthy"] = healthy

	if f := poisoned.free(); f != 0 {
		t.Fatalf("poisoned host free() = %d, want 0 (slots_free is the truth)", f)
	}
	for i := 0; i < 3; i++ {
		h := g.reserveHost(nil)
		if h == nil || h.id != "healthy" {
			t.Fatalf("pick %d: want healthy, got %v", i, h)
		}
	}
}

func TestRegisterFallsBackWithoutSlotsFree(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)

	post := func(body string) {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
		g.handleRegister(rr, req)
		if rr.Code != 204 {
			t.Fatalf("register: got %d: %s", rr.Code, rr.Body.String())
		}
	}

	// Old host binary: no slots_free field → fall back to total-used.
	post(`{"host_id":"old","addr":"1.2.3.4:8080","slots_total":24,"slots_used":10,"sandbox_ids":[]}`)
	if f := g.hosts["old"].slotsFree; f != 14 {
		t.Fatalf("fallback slotsFree = %d, want 14", f)
	}
	// New binary: explicit slots_free wins, including a genuine zero.
	post(`{"host_id":"new","addr":"1.2.3.5:8080","slots_total":24,"slots_used":10,"slots_free":0,"sandbox_ids":[]}`)
	if f := g.hosts["new"].slotsFree; f != 0 {
		t.Fatalf("explicit slotsFree = %d, want 0", f)
	}
}

func TestRegisterClampsHeartbeatFreeToTotalMinusUsed(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)

	post := func(body string) {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
		g.handleRegister(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("register: got %d: %s", rr.Code, rr.Body.String())
		}
	}

	// Reproduce the live delete storm. The worker's older heartbeat path first
	// listed 7 running rows, then concurrent deletes reduced occupancy to 2
	// before its separate FreeSlots query returned 46. The gateway must retain
	// the route/used snapshot but conservatively clamp its free half to 41.
	post(`{
		"host_id":"worker",
		"addr":"10.160.0.59:8080",
		"slots_total":48,
		"slots_used":48,
		"slots_free":0,
		"sandbox_ids":["before"]
	}`)
	post(`{
		"host_id":"worker",
		"addr":"10.160.0.59:8080",
		"slots_total":48,
		"slots_used":7,
		"slots_free":46,
		"sandbox_ids":["a","b","c","d","e","f","g"]
	}`)

	h := g.hosts["worker"]
	if h.slotsUsed != 7 || h.slotsFree != 41 {
		t.Fatalf("racy heartbeat accounting used/free = %d/%d, want 7/41", h.slotsUsed, h.slotsFree)
	}
	if got := h.slotsUsed + h.free(); got != h.slotsTotal {
		t.Fatalf("used+free = %d, want total %d", got, h.slotsTotal)
	}
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		if got := g.route[id]; got != "worker" {
			t.Fatalf("route %s = %q, want worker", id, got)
		}
	}

	// The following coherent heartbeat restores exact empty capacity.
	post(`{
		"host_id":"worker",
		"addr":"10.160.0.59:8080",
		"slots_total":48,
		"slots_used":0,
		"slots_free":48,
		"sandbox_ids":[]
	}`)
	if h.slotsUsed != 0 || h.slotsFree != 48 {
		t.Fatalf("settled accounting used/free = %d/%d, want 0/48", h.slotsUsed, h.slotsFree)
	}
}

func TestListFailsClosedInsteadOfReturningPartialFleet(t *testing.T) {
	good, _ := fakeHost(t, http.StatusOK, `[{"id":"held-on-good"}]`)
	bad, _ := fakeHost(t, http.StatusInternalServerError, `{"error":"temporarily unavailable"}`)

	g := New("tok", 20*time.Second, 0, 0)
	addTestHost(g, "good-worker", strings.TrimPrefix(good.URL, "http://"), 1, 23)
	addTestHost(g, "bad-worker", strings.TrimPrefix(bad.URL, "http://"), 1, 23)

	rr := httptest.NewRecorder()
	g.handleList(rr, httptest.NewRequest(http.MethodGet, "/sandboxes", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("partial list status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "sandbox list incomplete") ||
		!strings.Contains(rr.Body.String(), "bad-worker") {
		t.Fatalf("partial list error lacks failed host context: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "held-on-good") {
		t.Fatalf("partial sandbox data leaked in error response: %s", rr.Body.String())
	}
}

func TestListSkipsUnreachableEmptyQuarantinedHost(t *testing.T) {
	owner, _ := fakeHost(t, http.StatusOK, `[{"id":"held"}]`)
	quarantined, quarantinedHits := fakeHost(t, http.StatusInternalServerError,
		`{"error":"suspending"}`)

	g := New("tok", 20*time.Second, 0, 0)
	addTestHost(g, "owner", strings.TrimPrefix(owner.URL, "http://"), 1, 23)
	// Fresh placement quarantine: no routes, occupancy, hibernation, or
	// reservations. It is safe and important not to query this host while MIG
	// moves it into SUSPENDED.
	addTestHost(g, "empty-refill", strings.TrimPrefix(quarantined.URL, "http://"), 0, 0)

	rr := httptest.NewRecorder()
	g.handleList(rr, httptest.NewRequest(http.MethodGet, "/sandboxes", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("list with empty quarantined host = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if quarantinedHits.Load() != 0 {
		t.Fatalf("empty quarantined host received %d list calls, want 0", quarantinedHits.Load())
	}
	var got []registry.Sandbox
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(got) != 1 || got[0].ID != "held" {
		t.Fatalf("list = %+v, want held sandbox", got)
	}
}

func TestListStillFailsForUnreachableOwnershipSignals(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Gateway, *host)
	}{
		{
			name: "route",
			setup: func(g *Gateway, _ *host) {
				g.route["held"] = "unreachable"
			},
		},
		{
			name: "occupancy",
			setup: func(_ *Gateway, h *host) {
				h.slotsUsed = 1
			},
		},
		{
			name: "hibernated",
			setup: func(_ *Gateway, h *host) {
				h.hibernated = 1
			},
		},
		{
			name: "reservation",
			setup: func(_ *Gateway, h *host) {
				h.reserved = 1
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unreachable, hits := fakeHost(t, http.StatusInternalServerError,
				`{"error":"unreachable"}`)
			g := New("tok", 20*time.Second, 0, 0)
			h := addTestHost(g, "unreachable", strings.TrimPrefix(unreachable.URL, "http://"), 0, 0)
			tt.setup(g, h)

			rr := httptest.NewRecorder()
			g.handleList(rr, httptest.NewRequest(http.MethodGet, "/sandboxes", nil))
			if rr.Code != http.StatusBadGateway {
				t.Fatalf("list status = %d, want 502; body=%s", rr.Code, rr.Body.String())
			}
			if hits.Load() != 1 {
				t.Fatalf("ownership host received %d list calls, want 1", hits.Load())
			}
		})
	}
}

func TestListAggregatesAllLiveHostsWhenComplete(t *testing.T) {
	first, _ := fakeHost(t, http.StatusOK, `[{"id":"one"}]`)
	second, _ := fakeHost(t, http.StatusOK, `[{"id":"two"}]`)

	g := New("tok", 20*time.Second, 0, 0)
	addTestHost(g, "first", strings.TrimPrefix(first.URL, "http://"), 1, 23)
	addTestHost(g, "second", strings.TrimPrefix(second.URL, "http://"), 1, 23)

	rr := httptest.NewRecorder()
	g.handleList(rr, httptest.NewRequest(http.MethodGet, "/sandboxes", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("complete list status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got []registry.Sandbox
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode complete list: %v", err)
	}
	ids := map[string]bool{}
	for _, sb := range got {
		ids[sb.ID] = true
	}
	if len(got) != 2 || !ids["one"] || !ids["two"] {
		t.Fatalf("complete list = %+v, want one and two", got)
	}
}

func TestWorkerReleaseGatePersistsAndBlocksStaleCapacity(t *testing.T) {
	releaseFile := filepath.Join(t.TempDir(), "worker-release")
	g := New("tok", 20*time.Second, 0, 0)
	if err := g.ConfigureWorkerReleaseFile(releaseFile); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/worker-release", strings.NewReader(`{"release":"release-2"}`))
	g.handleWorkerRelease(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("set release: got %d: %s", rr.Code, rr.Body.String())
	}

	post := func(body string) {
		t.Helper()
		rr := httptest.NewRecorder()
		g.handleRegister(rr, httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body)))
		if rr.Code != http.StatusNoContent {
			t.Fatalf("register: got %d: %s", rr.Code, rr.Body.String())
		}
	}
	post(`{"host_id":"worker","addr":"10.0.0.1:8080","release":"release-1","slots_total":3,"slots_used":0,"slots_free":3,"sandbox_ids":["existing"]}`)
	if free := g.hosts["worker"].free(); free != 0 {
		t.Fatalf("stale worker free = %d, want 0", free)
	}
	if got := g.route["existing"]; got != "worker" {
		t.Fatalf("stale worker route = %q, want worker", got)
	}

	post(`{"host_id":"worker","addr":"10.0.0.1:8080","release":"release-2","slots_total":3,"slots_used":0,"slots_free":3,"sandbox_ids":["existing"]}`)
	if free := g.hosts["worker"].free(); free != 3 {
		t.Fatalf("current worker free = %d, want 3", free)
	}

	restarted := New("tok", 20*time.Second, 0, 0)
	if err := restarted.ConfigureWorkerReleaseFile(releaseFile); err != nil {
		t.Fatal(err)
	}
	if restarted.expectedRelease != "release-2" {
		t.Fatalf("persisted release = %q, want release-2", restarted.expectedRelease)
	}
}

func TestRegisterReplacesHostWithSameAddress(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)

	post := func(body string) {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
		g.handleRegister(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("register: got %d: %s", rr.Code, rr.Body.String())
		}
	}

	// A suspended worker can resume an old serve process that captured the
	// short hostname, then roll to a new allocation that sees the FQDN. Both
	// advertise the same physical API address.
	post(`{
		"host_id":"worker-short",
		"addr":"10.160.0.59:8080",
		"slots_total":48,
		"slots_used":2,
		"slots_free":46,
		"sandbox_ids":["kept","stale"],
		"snapshot_ids":["old-snapshot"]
	}`)
	post(`{
		"host_id":"other-worker",
		"addr":"10.160.0.60:8080",
		"slots_total":48,
		"slots_used":0,
		"slots_free":48,
		"sandbox_ids":[]
	}`)
	// Exercise cleanup of the old reverse-proxy cache too.
	g.proxies.Store("worker-short", &hostProxyEntry{})
	g.hosts["worker-short"].reserved = 1
	inflight := *g.hosts["worker-short"]

	post(`{
		"host_id":"worker.example.internal",
		"addr":"10.160.0.59:8080",
		"slots_total":48,
		"slots_used":2,
		"slots_free":46,
		"sandbox_ids":["kept","new"],
		"snapshot_ids":["new-snapshot"]
	}`)

	if _, ok := g.hosts["worker-short"]; ok {
		t.Fatal("superseded host identity was not removed")
	}
	if len(g.hosts) != 2 {
		t.Fatalf("hosts = %d, want 2 physical addresses", len(g.hosts))
	}
	if _, ok := g.hosts["other-worker"]; !ok {
		t.Fatal("host at a different address was removed")
	}
	if h := g.hosts["worker.example.internal"]; h == nil || h.addr != "10.160.0.59:8080" {
		t.Fatalf("replacement host = %#v", h)
	}
	if got := g.route["kept"]; got != "worker.example.internal" {
		t.Fatalf("kept route = %q, want replacement host", got)
	}
	if got := g.route["new"]; got != "worker.example.internal" {
		t.Fatalf("new route = %q, want replacement host", got)
	}
	if _, ok := g.route["stale"]; ok {
		t.Fatal("route omitted by authoritative replacement heartbeat survived")
	}
	if _, ok := g.snapRoute["old-snapshot"]; ok {
		t.Fatal("superseded snapshot route survived")
	}
	if got := g.snapRoute["new-snapshot"]; got != "worker.example.internal" {
		t.Fatalf("new snapshot route = %q, want replacement host", got)
	}
	if _, ok := g.proxies.Load("worker-short"); ok {
		t.Fatal("superseded reverse proxy cache survived")
	}

	// A create reserved through the old identity may complete after the
	// replacement heartbeat. It must land on the current route, not resurrect
	// the deleted host ID.
	g.landReservation(&inflight, "completed-after-replacement")
	if got := g.route["completed-after-replacement"]; got != "worker.example.internal" {
		t.Fatalf("in-flight create route = %q, want replacement host", got)
	}
	if h := g.hosts["worker.example.internal"]; h.slotsUsed != 3 || h.slotsFree != 45 {
		t.Fatalf("replacement accounting after in-flight create: used=%d free=%d, want 3/45",
			h.slotsUsed, h.slotsFree)
	}
}

// fakeHost is an httptest host answering POST /sandboxes with a fixed status.
func fakeHost(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if status == 503 {
			w.Header().Set("Retry-After", "5")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func addTestHost(g *Gateway, id, addr string, used, free int) *host {
	h := &host{id: id, addr: addr, token: "htok", slotsTotal: 24, slotsUsed: used, slotsFree: free, lastSeen: time.Now()}
	g.hosts[id] = h
	return h
}

// TestCreateClientCancelDoesNotPenalize: a client that disconnects mid-create
// makes the outbound call fail with OUR context's cancellation — that must not
// read as "host down". A wave of client timeouts would otherwise penalize
// every healthy host and blackout placement.
func TestCreateClientCancelDoesNotPenalize(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked // hold the create until the client has gone away
		w.WriteHeader(500)
	}))
	t.Cleanup(func() { close(blocked); srv.Close() })

	g := New("tok", 20*time.Second, 0, 0)
	addTestHost(g, "a", strings.TrimPrefix(srv.URL, "http://"), 0, 24)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/sandboxes", strings.NewReader(`{}`)).WithContext(ctx)
	g.handleCreate(rr, req)

	if rr.Code != 499 {
		t.Fatalf("cancelled create: got %d, want 499 (body: %s)", rr.Code, rr.Body.String())
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	a := g.hosts["a"]
	if time.Now().Before(a.penaltyUntil) {
		t.Fatal("client cancellation must not penalize the host")
	}
	if a.reserved != 0 {
		t.Fatalf("reservation leaked: %d", a.reserved)
	}
}

func TestCreateFailsOverOnCapacityPushback(t *testing.T) {
	// Host A (fuller — bin-pack picks it first) answers 503 "port pool
	// exhausted"; host B answers 201. The create must land on B, and A must be
	// penalized with its free count zeroed.
	srvA, hitsA := fakeHost(t, 503, `{"error":"port pool exhausted: pool exhausted"}`)
	srvB, hitsB := fakeHost(t, 201, `{"id":"sb-1","status":"running"}`)

	g := New("tok", 20*time.Second, 0, 0)
	addTestHost(g, "a", strings.TrimPrefix(srvA.URL, "http://"), 20, 4)
	addTestHost(g, "b", strings.TrimPrefix(srvB.URL, "http://"), 0, 24)

	rr := httptest.NewRecorder()
	g.handleCreate(rr, httptest.NewRequest("POST", "/sandboxes", strings.NewReader(`{}`)))

	if rr.Code != 201 {
		t.Fatalf("create should fail over to b and return 201; got %d: %s", rr.Code, rr.Body.String())
	}
	if hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Fatalf("want exactly one attempt per host; a=%d b=%d", hitsA.Load(), hitsB.Load())
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.route["sb-1"] != "b" {
		t.Fatalf("route[sb-1] = %q, want b", g.route["sb-1"])
	}
	a := g.hosts["a"]
	if !time.Now().Before(a.penaltyUntil) {
		t.Fatal("host a should be penalized after capacity pushback")
	}
	if a.slotsFree != 0 {
		t.Fatalf("host a slotsFree = %d, want 0 (advertised free was stale)", a.slotsFree)
	}
	if a.reserved != 0 || g.hosts["b"].reserved != 0 {
		t.Fatalf("reservations must be fully released; a=%d b=%d", a.reserved, g.hosts["b"].reserved)
	}
}

func TestCreateDoesNotRetryOnHostError(t *testing.T) {
	// A genuine host-side 500 is not a capacity signal: no failover, 502 out.
	srvA, hitsA := fakeHost(t, 500, `{"error":"boom"}`)
	srvB, hitsB := fakeHost(t, 201, `{"id":"sb-2","status":"running"}`)

	g := New("tok", 20*time.Second, 0, 0)
	addTestHost(g, "a", strings.TrimPrefix(srvA.URL, "http://"), 20, 4)
	addTestHost(g, "b", strings.TrimPrefix(srvB.URL, "http://"), 0, 24)

	rr := httptest.NewRecorder()
	g.handleCreate(rr, httptest.NewRequest("POST", "/sandboxes", strings.NewReader(`{}`)))

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("want 502 on host error, got %d: %s", rr.Code, rr.Body.String())
	}
	if hitsA.Load() != 1 || hitsB.Load() != 0 {
		t.Fatalf("host error must not fail over; a=%d b=%d", hitsA.Load(), hitsB.Load())
	}
	if g.hosts["a"].reserved != 0 {
		t.Fatalf("reservation leaked: %d", g.hosts["a"].reserved)
	}
}

// TestCreateClientErrorKeepsStatus: a host-side 4xx is the CLIENT's mistake
// (e.g. an unfittable mem_mib override) — it must reach the client with the
// host's status, not be wrapped into a retryable-looking 502, and must not
// fail over.
func TestCreateClientErrorKeepsStatus(t *testing.T) {
	srvA, hitsA := fakeHost(t, 400, `{"error":"mem_mib 99999 exceeds host limit 28164"}`)
	srvB, hitsB := fakeHost(t, 201, `{"id":"sb-3","status":"running"}`)

	g := New("tok", 20*time.Second, 0, 0)
	addTestHost(g, "a", strings.TrimPrefix(srvA.URL, "http://"), 20, 4)
	addTestHost(g, "b", strings.TrimPrefix(srvB.URL, "http://"), 0, 24)

	rr := httptest.NewRecorder()
	g.handleCreate(rr, httptest.NewRequest("POST", "/sandboxes", strings.NewReader(`{"mem_mib":99999}`)))

	if rr.Code != 400 {
		t.Fatalf("host 400 must pass through: got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if hitsA.Load() != 1 || hitsB.Load() != 0 {
		t.Fatalf("client error must not fail over; a=%d b=%d", hitsA.Load(), hitsB.Load())
	}
}

func TestCreateAttemptsBoundedAndReleased(t *testing.T) {
	// Every host pushes back on capacity: the create ends 503 + Retry-After
	// after at most maxCreateAttempts hosts, with no reservation leaked.
	g := New("tok", 20*time.Second, 0, 0)
	for _, id := range []string{"a", "b", "c", "d"} {
		srv, _ := fakeHost(t, 503, `{"error":"port pool exhausted: pool exhausted"}`)
		addTestHost(g, id, strings.TrimPrefix(srv.URL, "http://"), 0, 24)
	}

	rr := httptest.NewRecorder()
	g.handleCreate(rr, httptest.NewRequest("POST", "/sandboxes", strings.NewReader(`{}`)))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 after bounded attempts, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("503 must carry Retry-After")
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	penalized := 0
	for _, h := range g.hosts {
		if h.reserved != 0 {
			t.Fatalf("host %s leaked a reservation: %d", h.id, h.reserved)
		}
		if time.Now().Before(h.penaltyUntil) {
			penalized++
		}
	}
	if penalized != maxCreateAttempts {
		t.Fatalf("want exactly %d penalized hosts, got %d", maxCreateAttempts, penalized)
	}
}

// TestRejectedCounterCountsCapacity503s: demand beyond the queue is invisible
// to the queue-depth gauge, so every capacity 503 must increment the rejected
// counter — it's the autoscaler's only signal of the overflow.
func TestRejectedCounterCountsCapacity503s(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0) // no hosts, queueing disabled

	for i := 0; i < 3; i++ {
		rr := httptest.NewRecorder()
		g.handleCreate(rr, httptest.NewRequest("POST", "/sandboxes", strings.NewReader(`{}`)))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("want 503 with no hosts, got %d", rr.Code)
		}
	}
	if got := g.rejected.Load(); got != 3 {
		t.Fatalf("rejected counter = %d, want 3", got)
	}

	rr := httptest.NewRecorder()
	g.handleMetrics(rr, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rr.Body.String(), "sandbox_create_rejected_total 3") {
		t.Fatalf("metrics must expose the rejected counter:\n%s", rr.Body.String())
	}
}

func TestPenaltyExpires(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	addTestHost(g, "a", "1.2.3.4:8080", 0, 24)
	g.penalize("a", 30*time.Millisecond, true)

	if h := g.reserveHost(nil); h != nil {
		t.Fatalf("penalized host must not be picked; got %v", h)
	}
	time.Sleep(50 * time.Millisecond)
	// Heartbeat restored the free count; penalty has lapsed.
	g.mu.Lock()
	g.hosts["a"].slotsFree = 24
	g.mu.Unlock()
	if h := g.reserveHost(nil); h == nil || h.id != "a" {
		t.Fatalf("expired penalty must make the host placeable again; got %v", h)
	}
}

func TestMetricsExposition(t *testing.T) {
	t.Setenv("SANDBOX_RELEASE", "abc123")
	g := liveGateway(
		&host{id: "h1", slotsTotal: 24, slotsUsed: 10, reserved: 20},
		&host{id: "h2", slotsTotal: 24, slotsUsed: 5, reserved: 4},
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
		`sandbox_build_info{component="gateway",release="abc123"} 1`,
		"sandbox_hosts_live 2",
		"sandbox_slots_total 48",
		"sandbox_slots_used 15",
		// h1 clamps 10 used + 20 reserved to 24; h2 contributes 5 + 4.
		"sandbox_slots_committed 33",
		"sandbox_slots_free 15",
		"sandbox_routes 2",
		"sandbox_create_queue_depth 3",
		"sandbox_direct_scale_out_total 0",
		"sandbox_direct_scale_out_failed_total 0",
		"sandbox_worker_release_mismatch 0",
		`sandbox_worker_release_info{release=""} 1`,
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

func TestMetricsCommittedSlotsCoverHeldBurstBeforeHeartbeat(t *testing.T) {
	g := liveGateway(
		&host{id: "h1", slotsTotal: 48, slotsUsed: 24, slotsFree: 24, reserved: 24},
		&host{id: "h2", slotsTotal: 48, slotsUsed: 24, slotsFree: 24, reserved: 24},
	)
	g.queued.Store(64)

	rr := httptest.NewRecorder()
	g.handleMetrics(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()

	// 96 committed + 64 queued = 160 slots, which the recording rule maps to
	// ceil(160/48) = 4 workers. slots_used alone exposed only 48 and yielded 3.
	for _, want := range []string{
		"sandbox_slots_used 48",
		"sandbox_slots_committed 96",
		"sandbox_create_queue_depth 64",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n---\n%s", want, body)
		}
	}
}
