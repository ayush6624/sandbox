package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// liveGateway builds a gateway with the given hosts, all marked seen just now.
func liveGateway(hosts ...*host) *Gateway {
	g := New("tok", 20*time.Second)
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

	rr := httptest.NewRecorder()
	g.handleMetrics(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()

	want := []string{
		"sandbox_hosts_live 2",
		"sandbox_slots_total 48",
		"sandbox_slots_used 15",
		"sandbox_slots_free 33",
		"sandbox_routes 2",
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
