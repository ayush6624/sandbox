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
		id                string
		total, used, free int
	}
	var (
		liveHosts             int
		totalSlots, usedSlots int
		freeSlots             int
		hibernated            int
		routes                int
		perHost               []hostMetric
	)

	g.mu.RLock()
	for _, h := range g.hosts {
		if time.Since(h.lastSeen) > g.ttl {
			continue
		}
		liveHosts++
		totalSlots += h.slotsTotal
		usedSlots += h.slotsUsed
		freeSlots += h.free()
		hibernated += h.hibernated
		perHost = append(perHost, hostMetric{id: h.id, total: h.slotsTotal, used: h.slotsUsed, free: h.free()})
	}
	routes = len(g.route)
	g.mu.RUnlock()

	sort.Slice(perHost, func(i, j int) bool { return perHost[i].id < perHost[j].id })

	var b strings.Builder
	gauge := func(name, help string, val int) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, val)
	}
	gauge("sandbox_hosts_live", "Number of hosts seen within the heartbeat TTL.", liveHosts)
	gauge("sandbox_slots_total", "Total sandbox slots across live hosts.", totalSlots)
	gauge("sandbox_slots_used", "Used sandbox slots across live hosts.", usedSlots)
	// slots_free is host-reported allocatable capacity (minus in-flight
	// reservations) — NOT total-used. Hibernated sandboxes hold their host
	// port without holding a slot, so total-used overstates what the fleet can
	// actually place; the autoscaler's recording rule uses
	// (slots_total - slots_free) as effective occupancy for the same reason.
	gauge("sandbox_slots_free", "Allocatable sandbox slots across live hosts (host-reported).", freeSlots)
	gauge("sandbox_routes", "Number of sandbox-id -> host routes the gateway holds.", routes)
	gauge("sandbox_hibernated", "Idle sandboxes frozen to disk across live hosts (hold no slot).", hibernated)
	// Queued creates are demand without a slot — the recording rule adds this
	// to slots_used so a burst pulls scale-up before any create lands.
	gauge("sandbox_create_queue_depth", "Creates waiting in the gateway's bounded queue for a free slot.", int(g.queued.Load()))

	// Per-host series share one HELP/TYPE header block each.
	fmt.Fprintf(&b, "# HELP sandbox_host_slots_total Total slots on a live host.\n# TYPE sandbox_host_slots_total gauge\n")
	for _, h := range perHost {
		fmt.Fprintf(&b, "sandbox_host_slots_total{host=%q} %d\n", h.id, h.total)
	}
	fmt.Fprintf(&b, "# HELP sandbox_host_slots_used Used slots on a live host.\n# TYPE sandbox_host_slots_used gauge\n")
	for _, h := range perHost {
		fmt.Fprintf(&b, "sandbox_host_slots_used{host=%q} %d\n", h.id, h.used)
	}
	fmt.Fprintf(&b, "# HELP sandbox_host_slots_free Allocatable slots on a live host (host-reported, minus reservations).\n# TYPE sandbox_host_slots_free gauge\n")
	for _, h := range perHost {
		fmt.Fprintf(&b, "sandbox_host_slots_free{host=%q} %d\n", h.id, h.free)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
