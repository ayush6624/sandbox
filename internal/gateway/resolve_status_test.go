package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The audit's L1 fix bounds how long an inbound request waits on a cross-host
// adopt. That bound makes "we don't know yet" a ROUTINE outcome for a real,
// cross-host-hibernated sandbox, so the status it maps to is load-bearing: a 404
// tells the SDK to raise NotFoundError and the client stops retrying, which
// turns a latency guard into apparent data loss. These tests pin the mapping —
// 404 only for a proven absence, 503 + Retry-After for everything else.
//
// They exercise handleRoute because it is the shortest path to the shared
// writeResolveFailure, and it is the one the unauthenticated public edge drives.

// slowAdoptHost answers POST /sandboxes/{id}/adopt after delay, so a bounded
// caller gives up while the adopt is genuinely still running.
func slowAdoptHost(t *testing.T, delay time.Duration, id string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"` + id + `","status":"running"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func routeStatus(t *testing.T, g *Gateway, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/route/"+id, nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	g.handleRoute(rec, req)
	return rec
}

// A host that reads the shared durable store and reports no record is the one
// verdict no retry can change, so it is the only 404.
func TestResolveFailureAbsentIs404(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	id := testID(41)
	srv, hits := fakeHost(t, 404, `{"error":"no durable record"}`)
	addTestHost(g, "a", strings.TrimPrefix(srv.URL, "http://"), 0, 24)

	rec := routeStatus(t, g, id)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a definitive absence", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want none: retrying cannot change a definitive absence", got)
	}
	if hits.Load() != 1 {
		t.Fatalf("host hit %d times, want 1 (a 404 is definitive — no fan-out)", hits.Load())
	}
}

// A malformed id cannot be a sandbox id, so it is absent without contacting
// anything — but it must still be a 404, not a 503.
func TestResolveFailureMalformedIs404WithoutContact(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	srv, hits := fakeHost(t, 201, `{"id":"x","status":"running"}`)
	addTestHost(g, "a", strings.TrimPrefix(srv.URL, "http://"), 0, 24)

	rec := routeStatus(t, g, "wp-login")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a malformed id", rec.Code)
	}
	if hits.Load() != 0 {
		t.Fatalf("host hit %d times, want 0: a hostname scan must not reach a worker", hits.Load())
	}
}

// The regression this whole change exists to prevent: a real sandbox whose adopt
// outruns the bounded wait must NOT be reported as not-found.
func TestResolveFailureInFlightAdoptIs503NotFound(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	id := testID(42)
	// The delay must exceed requestAdoptWait, or the adopt lands inside the
	// wait and the request simply succeeds — which is the happy path, not the
	// case under test.
	srv, hits := slowAdoptHost(t, requestAdoptWait+2*time.Second, id)
	addTestHost(g, "a", strings.TrimPrefix(srv.URL, "http://"), 0, 24)

	rec := routeStatus(t, g, id)
	if rec.Code == http.StatusNotFound {
		t.Fatal("status = 404 while the adopt was still in flight: the SDK maps that to NotFoundError and the client gives up on a sandbox that exists")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for an indeterminate resolve", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("503 without Retry-After: the client is being told to retry with no idea when")
	}
	if hits.Load() != 1 {
		t.Fatalf("host hit %d times, want 1", hits.Load())
	}

	// The adopt keeps running detached, so a retry joins it and succeeds —
	// which is what makes answering 503 rather than blocking acceptable.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if rec := routeStatus(t, g, id); rec.Code == http.StatusOK {
			if hits.Load() != 1 {
				t.Fatalf("host hit %d times, want 1: the retry must join the in-flight adopt", hits.Load())
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the background adopt never became visible to a retry")
}

// Nothing was asked — no host had capacity, or dispatch was throttled — so
// absence is not knowable and must not be reported. This is the cheapest way to
// exercise that arm: with no host registered, every resolve is indeterminate.
func TestResolveFailureNothingAskedIs503(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)

	for i := 0; i < 50; i++ {
		rec := routeStatus(t, g, testID(3000+i))
		if rec.Code == http.StatusNotFound {
			t.Fatalf("id #%d got 404 with no host consulted: absence is not knowable, so this must be 503", i)
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("id #%d status = %d, want 503", i, rec.Code)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Fatalf("id #%d: 503 without Retry-After", i)
		}
	}
	// And an indeterminate answer must never be remembered as a negative —
	// otherwise a transient capacity dip would 404 a live sandbox for the whole
	// cache TTL.
	if n := g.notFound.len(); n != 0 {
		t.Fatalf("negative cache holds %d entries, want 0: only a definitive absence may be cached", n)
	}
}

// DELETE must agree with the rest of the API about what "gone" means. A worker's
// handleDestroy answers 404 for a row it does not have, and every other
// id-scoped gateway route already 404s a deleted sandbox through this same
// resolveAbsent path — so a 204 here made the fleet contradict both a worker and
// itself, and left a caller unable to tell "I deleted it" from "never existed".
func TestGatewayDestroyAbsentIs404(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	id := testID(61)
	srv, _ := fakeHost(t, 404, `{"error":"no durable record"}`)
	addTestHost(g, "a", strings.TrimPrefix(srv.URL, "http://"), 0, 24)

	req := httptest.NewRequest(http.MethodDelete, "/sandboxes/"+id, nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	g.handleGatewayDestroy(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a provably absent sandbox", rec.Code)
	}
}

// An indeterminate answer must NOT be reported as a completed delete: that
// would tell a client its sandbox is gone while it is merely unreachable.
func TestGatewayDestroyUnknownIs503NotDeleted(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	id := testID(62)
	srv, _ := fakeHost(t, 503, `{"error":"no capacity"}`)
	addTestHost(g, "a", strings.TrimPrefix(srv.URL, "http://"), 0, 24)

	req := httptest.NewRequest(http.MethodDelete, "/sandboxes/"+id, nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	g.handleGatewayDestroy(rec, req)

	if rec.Code == http.StatusNoContent || rec.Code == http.StatusNotFound {
		t.Fatalf("status = %d, want an indeterminate status (not 204/404)", rec.Code)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
