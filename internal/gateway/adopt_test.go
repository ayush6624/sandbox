package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testID returns a canonical-UUID-shaped sandbox id, which is what
// wellFormedSandboxID (and therefore every request-path resolution) demands.
func testID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

// TestResolveViaAdoptSuccess: a route miss adopts onto a live host, records the
// route, and consumes a slot.
func TestResolveViaAdoptSuccess(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	id := testID(1)
	srv, hits := fakeHost(t, 201, `{"id":"`+id+`","status":"running"}`)
	addTestHost(g, "a", strings.TrimPrefix(srv.URL, "http://"), 0, 24)

	hid, outcome := g.resolveViaAdopt(id, nil, requestResolve)
	if outcome != resolveAdopted || hid != "a" {
		t.Fatalf("resolveViaAdopt = (%q, %v), want (a, resolveAdopted)", hid, outcome)
	}
	if hits.Load() != 1 {
		t.Fatalf("host hit %d times, want 1", hits.Load())
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.route[id] != "a" {
		t.Fatalf("route not recorded: %v", g.route)
	}
	if g.hosts["a"].slotsUsed != 1 {
		t.Fatalf("slot not consumed: used=%d", g.hosts["a"].slotsUsed)
	}
}

// TestResolveViaAdopt404NegativeCaches: a definitive not-found is cached, so a
// second lookup for the same dead id does NOT re-dispatch to the host.
func TestResolveViaAdopt404NegativeCaches(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	srv, hits := fakeHost(t, 404, `{"error":"not adoptable"}`)
	addTestHost(g, "a", strings.TrimPrefix(srv.URL, "http://"), 0, 24)

	ghost := testID(2)
	if _, outcome := g.resolveViaAdopt(ghost, nil, requestResolve); outcome != resolveAbsent {
		t.Fatalf("outcome = %v, want resolveAbsent for a host 404", outcome)
	}
	if _, outcome := g.resolveViaAdopt(ghost, nil, requestResolve); outcome != resolveAbsent {
		t.Fatalf("outcome = %v, want resolveAbsent from the negative cache", outcome)
	}
	if hits.Load() != 1 {
		t.Fatalf("host hit %d times, want 1 (second lookup should hit the negative cache)", hits.Load())
	}
	if !g.notFound.has(ghost) {
		t.Fatal("dead id not negative-cached")
	}
}

// TestResolveViaAdoptFailsOver: an adopt that a host rejects with capacity
// pushback fails over to another live host.
func TestResolveViaAdoptFailsOver(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	id := testID(3)
	srvA, hitsA := fakeHost(t, 503, `{"error":"pool exhausted"}`)
	srvB, hitsB := fakeHost(t, 201, `{"id":"`+id+`","status":"running"}`)
	// A fuller (free=1) so bin-pack tries it first; B has more room.
	addTestHost(g, "a", strings.TrimPrefix(srvA.URL, "http://"), 23, 1)
	addTestHost(g, "b", strings.TrimPrefix(srvB.URL, "http://"), 0, 24)

	hid, outcome := g.resolveViaAdopt(id, nil, requestResolve)
	if outcome != resolveAdopted || hid != "b" {
		t.Fatalf("resolveViaAdopt = (%q, %v), want (b, resolveAdopted)", hid, outcome)
	}
	if hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Fatalf("hits A=%d B=%d, want 1 and 1", hitsA.Load(), hitsB.Load())
	}
}

// TestDrainExcludesSource: draining a host releases each sandbox on the source
// and adopts it onto a DIFFERENT live host (never back onto the drained source).
func TestDrainExcludesSource(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	src, _ := fakeHost(t, 204, ``)                                    // release ok
	dst, dstHits := fakeHost(t, 201, `{"id":"x","status":"running"}`) // adopt ok
	// Source is FULLER than dst; without the exclude, bin-pack would re-pick it.
	addTestHost(g, "src", strings.TrimPrefix(src.URL, "http://"), 2, 2)
	addTestHost(g, "dst", strings.TrimPrefix(dst.URL, "http://"), 0, 24)
	g.route["sb-a"] = "src"
	g.route["sb-b"] = "src"

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hosts/src/drain", nil)
	req.SetPathValue("host", "src") // mux normally sets this; a raw request must
	g.handleDrain(rr, req)
	if rr.Code != 200 {
		t.Fatalf("drain: got %d: %s", rr.Code, rr.Body.String())
	}
	var res struct{ Total, Moved, Skipped int }
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Total != 2 || res.Moved != 2 || res.Skipped != 0 {
		t.Fatalf("drain result = %+v, want total=2 moved=2 skipped=0", res)
	}
	if dstHits.Load() != 2 {
		t.Fatalf("dst adopted %d, want 2", dstHits.Load())
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.route["sb-a"] != "dst" || g.route["sb-b"] != "dst" {
		t.Fatalf("routes not moved to dst: %v", g.route)
	}
}

// TestAdoptMalformedIDNeverDispatches: an id that can't be a sandbox id (a
// hostname-scan label reaching GET /route/{id} from the public edge) is answered
// 404 without contacting a single host, and without consuming a negative-cache
// slot — that combination is what makes a scan free for the control plane.
func TestAdoptMalformedIDNeverDispatches(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	srv, hits := fakeHost(t, 404, `{"error":"not adoptable"}`)
	addTestHost(g, "a", strings.TrimPrefix(srv.URL, "http://"), 0, 24)

	for _, id := range []string{"myapp", "wp-login", "0000-0000", strings.Repeat("a", 36), testID(4) + "x"} {
		if _, outcome := g.resolveViaAdopt(id, nil, requestResolve); outcome != resolveAbsent {
			t.Fatalf("resolveViaAdopt(%q) = %v, want resolveAbsent", id, outcome)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("host contacted %d times for malformed ids, want 0", hits.Load())
	}
	if n := g.notFound.len(); n != 0 {
		t.Fatalf("negative cache grew to %d on malformed ids, want 0", n)
	}
	if g.adoptSuppressedMalformed.Load() != 5 {
		t.Fatalf("suppressed counter = %d, want 5", g.adoptSuppressedMalformed.Load())
	}
}

// TestRouteHandlerUnknownID404sWithoutFanOut: the edge's resolution endpoint
// answers 404 for an unknown id after contacting at most ONE host — a 404 from
// any host is definitive (every host reads the same GCS record), so there is no
// reason to ask the rest of the fleet.
func TestRouteHandlerUnknownID404sWithoutFanOut(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	var srvs []*atomic.Int64
	for i, name := range []string{"a", "b", "c"} {
		srv, hits := fakeHost(t, 404, `{"error":"not adoptable"}`)
		addTestHost(g, name, strings.TrimPrefix(srv.URL, "http://"), i, 24-i)
		srvs = append(srvs, hits)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/route/"+testID(5), nil)
	req.SetPathValue("id", testID(5))
	g.handleRoute(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("handleRoute = %d, want 404: %s", rr.Code, rr.Body.String())
	}
	var total int64
	for _, h := range srvs {
		total += h.Load()
	}
	if total != 1 {
		t.Fatalf("adopt dispatched to %d hosts, want 1", total)
	}
}

// TestAdoptDispatchIsRateLimitedFleetWide: a flood of DISTINCT well-formed ids
// (which single-flight and the negative cache can't collapse) is bounded by the
// fleet-wide token bucket, so a hostname scan can't make every host do GCS
// lookups per request.
func TestAdoptDispatchIsRateLimitedFleetWide(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	srv, hits := fakeHost(t, 404, `{"error":"not adoptable"}`)
	addTestHost(g, "a", strings.TrimPrefix(srv.URL, "http://"), 0, 24)

	const flood = 200
	for i := 0; i < flood; i++ {
		if _, outcome := g.resolveViaAdopt(testID(1000+i), nil, edgeResolve); outcome == resolveAdopted {
			t.Fatal("unexpected successful adopt")
		}
	}
	// The bucket starts full and refills at adoptEdgeRefill; the loop is
	// sub-second, so allow one second's worth of refill on top of the burst.
	if max := int64(adoptEdgeBurst + adoptEdgeRefill + 1); hits.Load() > max {
		t.Fatalf("dispatched %d adopts for %d ids, want <= %d", hits.Load(), flood, max)
	}
	if g.adoptSuppressedThrottled.Load() == 0 {
		t.Fatal("no dispatches recorded as throttled")
	}
	if n := g.notFound.len(); n > negCacheMax {
		t.Fatalf("negative cache = %d, want <= %d", n, negCacheMax)
	}
}

// TestRequestWaitBoundedAdoptFinishesInBackground: a slow adopt does NOT hold
// the request. The caller gives up after its bounded wait; the adopt keeps
// running on its own budget and a retry JOINS the same flight (one host hit,
// not two) and gets the route.
func TestRequestWaitBoundedAdoptFinishesInBackground(t *testing.T) {
	id := testID(6)
	release := make(chan struct{})
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"` + id + `","status":"running"}`))
	}))
	t.Cleanup(srv.Close)

	g := New("tok", 20*time.Second, 0, 0)
	addTestHost(g, "a", strings.TrimPrefix(srv.URL, "http://"), 0, 24)
	quick := resolvePolicy{wait: 30 * time.Millisecond, screenID: true, limiter: adoptLimitAPI}

	start := time.Now()
	if _, outcome := g.resolveViaAdopt(id, nil, quick); outcome != resolveUnknown {
		t.Fatalf("outcome = %v, want resolveUnknown: a bounded wait over an in-flight adopt must never report absence", outcome)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("bounded wait took %v, want ~30ms", elapsed)
	}
	if g.adoptWaitTimeouts.Load() != 1 {
		t.Fatalf("wait-timeout counter = %d, want 1", g.adoptWaitTimeouts.Load())
	}

	close(release)
	// A retry joins the in-flight adopt instead of dispatching a second one.
	var hid string
	var outcome resolveOutcome
	for i := 0; i < 100; i++ {
		if hid, outcome = g.resolveViaAdopt(id, nil, quick); outcome == resolveAdopted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if outcome != resolveAdopted || hid != "a" {
		t.Fatalf("retry after background adopt = (%q, %v), want (a, resolveAdopted)", hid, outcome)
	}
	if hits.Load() != 1 {
		t.Fatalf("host hit %d times, want 1 (the retry must join the in-flight adopt)", hits.Load())
	}
}

// TestResolvePolicyBudgets pins the asymmetry the audit's L1 fix rests on: an
// inbound request may not wait minutes, while a drain (deliberate, authenticated,
// moving real sandboxes) keeps the full adopt budget.
func TestResolvePolicyBudgets(t *testing.T) {
	if requestResolve.wait <= 0 || requestResolve.wait > 5*time.Second {
		t.Fatalf("requestResolve.wait = %v, want a small positive bound", requestResolve.wait)
	}
	if !requestResolve.screenID || requestResolve.limiter != adoptLimitAPI {
		t.Fatalf("requestResolve must screen ids and rate-limit dispatch: %+v", requestResolve)
	}
	if edgeResolve.wait != requestResolve.wait || !edgeResolve.screenID || edgeResolve.limiter != adoptLimitEdge {
		t.Fatalf("edgeResolve must be bounded, screened, and on its OWN bucket: %+v", edgeResolve)
	}
	if drainResolve.wait != 0 {
		t.Fatalf("drainResolve.wait = %v, want 0 (wait for completion)", drainResolve.wait)
	}
	if drainResolve.limiter != adoptNoLimit || drainResolve.screenID {
		t.Fatalf("drainResolve must not be throttled or screened: %+v", drainResolve)
	}
	if adoptTimeout < 5*time.Minute {
		t.Fatalf("adoptTimeout = %v, want >= 5m (a drain's adopt does real bring-up work)", adoptTimeout)
	}
}

// TestDrainWaitsPastRequestBudget: a drain whose adopt takes longer than an
// inbound request would ever wait still completes and counts the sandbox moved.
func TestDrainWaitsPastRequestBudget(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	src, _ := fakeHost(t, 204, ``)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(requestAdoptWait + 100*time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"sb-slow","status":"running"}`))
	}))
	t.Cleanup(slow.Close)
	addTestHost(g, "src", strings.TrimPrefix(src.URL, "http://"), 2, 2)
	addTestHost(g, "dst", strings.TrimPrefix(slow.URL, "http://"), 0, 24)
	g.route["sb-slow"] = "src"

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/hosts/src/drain", nil)
	req.SetPathValue("host", "src")
	g.handleDrain(rr, req)
	var res struct{ Total, Moved, Skipped int }
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Moved != 1 || res.Skipped != 0 {
		t.Fatalf("drain result = %+v, want moved=1 skipped=0", res)
	}
}
