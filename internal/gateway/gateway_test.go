package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
