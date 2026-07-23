package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestResolveViaAdoptSuccess: a route miss adopts onto a live host, records the
// route, and consumes a slot.
func TestResolveViaAdoptSuccess(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	srv, hits := fakeHost(t, 201, `{"id":"sb-1","status":"running"}`)
	addTestHost(g, "a", strings.TrimPrefix(srv.URL, "http://"), 0, 24)

	hid, ok := g.resolveViaAdopt("sb-1", nil)
	if !ok || hid != "a" {
		t.Fatalf("resolveViaAdopt = (%q, %v), want (a, true)", hid, ok)
	}
	if hits.Load() != 1 {
		t.Fatalf("host hit %d times, want 1", hits.Load())
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.route["sb-1"] != "a" {
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

	if _, ok := g.resolveViaAdopt("ghost", nil); ok {
		t.Fatal("expected ok=false for a 404")
	}
	if _, ok := g.resolveViaAdopt("ghost", nil); ok {
		t.Fatal("expected ok=false on the cached second lookup")
	}
	if hits.Load() != 1 {
		t.Fatalf("host hit %d times, want 1 (second lookup should hit the negative cache)", hits.Load())
	}
	if _, cached := g.notFound.Load("ghost"); !cached {
		t.Fatal("dead id not negative-cached")
	}
}

// TestResolveViaAdoptFailsOver: an adopt that a host rejects with capacity
// pushback fails over to another live host.
func TestResolveViaAdoptFailsOver(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	srvA, hitsA := fakeHost(t, 503, `{"error":"pool exhausted"}`)
	srvB, hitsB := fakeHost(t, 201, `{"id":"sb-2","status":"running"}`)
	// A fuller (free=1) so bin-pack tries it first; B has more room.
	addTestHost(g, "a", strings.TrimPrefix(srvA.URL, "http://"), 23, 1)
	addTestHost(g, "b", strings.TrimPrefix(srvB.URL, "http://"), 0, 24)

	hid, ok := g.resolveViaAdopt("sb-2", nil)
	if !ok || hid != "b" {
		t.Fatalf("resolveViaAdopt = (%q, %v), want (b, true)", hid, ok)
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
