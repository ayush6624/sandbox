package gateway

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// handleMetrics serves the gateway's fleet state in Prometheus text exposition
// format (v0.0.4). It's hand-rolled rather than pulling in client_golang — the
// repo keeps its dependency tree minimal, and a handful of gauges don't justify
// the library. Prometheus (on the control VM) scrapes this behind the same
// bearer auth as every other endpoint; its scrape config carries the token.
//
// The autoscaler's scaling signal is derived downstream from these:
// workers_desired = ceil((sandbox_slots_used + headroom) / slots_per_host).
// Only live hosts (seen within ttl) are counted, so a dead host's capacity
// doesn't mask real demand.
func (g *Gateway) handleMetrics(w http.ResponseWriter, r *http.Request) {
	type hostMetric struct {
		id          string
		total, used int
	}
	var (
		liveHosts               int
		totalSlots, usedSlots   int
		routes                  int
		perHost                 []hostMetric
	)

	g.mu.RLock()
	for _, h := range g.hosts {
		if time.Since(h.lastSeen) > g.ttl {
			continue
		}
		liveHosts++
		totalSlots += h.slotsTotal
		usedSlots += h.slotsUsed
		perHost = append(perHost, hostMetric{id: h.id, total: h.slotsTotal, used: h.slotsUsed})
	}
	routes = len(g.route)
	g.mu.RUnlock()

	freeSlots := totalSlots - usedSlots
	if freeSlots < 0 {
		freeSlots = 0
	}
	sort.Slice(perHost, func(i, j int) bool { return perHost[i].id < perHost[j].id })

	var b strings.Builder
	gauge := func(name, help string, val int) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, val)
	}
	gauge("sandbox_hosts_live", "Number of hosts seen within the heartbeat TTL.", liveHosts)
	gauge("sandbox_slots_total", "Total sandbox slots across live hosts.", totalSlots)
	gauge("sandbox_slots_used", "Used sandbox slots across live hosts.", usedSlots)
	gauge("sandbox_slots_free", "Free sandbox slots across live hosts.", freeSlots)
	gauge("sandbox_routes", "Number of sandbox-id -> host routes the gateway holds.", routes)

	// Per-host series share one HELP/TYPE header block each.
	fmt.Fprintf(&b, "# HELP sandbox_host_slots_total Total slots on a live host.\n# TYPE sandbox_host_slots_total gauge\n")
	for _, h := range perHost {
		fmt.Fprintf(&b, "sandbox_host_slots_total{host=%q} %d\n", h.id, h.total)
	}
	fmt.Fprintf(&b, "# HELP sandbox_host_slots_used Used slots on a live host.\n# TYPE sandbox_host_slots_used gauge\n")
	for _, h := range perHost {
		fmt.Fprintf(&b, "sandbox_host_slots_used{host=%q} %d\n", h.id, h.used)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
