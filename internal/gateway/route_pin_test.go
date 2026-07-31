package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// handleRegister rebuilds a host's whole route table from the
// heartbeat's sandbox list. A heartbeat sampled BEFORE a create committed
// (routine: heartbeats are 5 s apart) therefore ERASES the route that
// landReservation just wrote, so the freshly created sandbox 404s until the
// next heartbeat — or gets sent down the expensive cross-host adopt path.
func TestHeartbeatKeepsFreshlyLandedRoute(t *testing.T) {
	h := &host{id: "worker-a", addr: "http://10.0.0.2:8080", token: "t", slotsTotal: 4}
	g := liveGateway(h)

	// A create lands and records its route, exactly as handleCreate does.
	reserved := g.reserveHost(nil)
	if reserved == nil {
		t.Fatal("expected a reservable host")
	}
	g.landReservation(reserved, "sandbox-fresh")
	g.mu.RLock()
	routed := g.route["sandbox-fresh"]
	g.mu.RUnlock()
	if routed != "worker-a" {
		t.Fatalf("route after landing = %q, want worker-a", routed)
	}

	// An in-flight heartbeat, sampled before that create committed, arrives.
	body := `{"host_id":"worker-a","addr":"http://10.0.0.2:8080","control_token":"t",` +
		`"slots_total":4,"slots_used":1,"sandbox_ids":[]}`
	req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	g.handleRegister(w, req)
	if w.Code != 204 {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}

	g.mu.RLock()
	after, ok := g.route["sandbox-fresh"]
	g.mu.RUnlock()
	if !ok {
		t.Fatalf("ROUTE LOSS CONFIRMED: a heartbeat that predates the create erased the route for sandbox-fresh (now unroutable → 404)")
	}
	_ = after
}
