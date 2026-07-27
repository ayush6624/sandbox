package gateway

import (
	"fmt"
	"net/http"
	"os"
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
// workers_desired = ceil((sandbox_slots_committed + headroom) / slots_per_host).
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
		committedSlots        int
		freeSlots             int
		hibernated            int
		routes                int
		releaseMismatches     int
		expectedRelease       string
		perHost               []hostMetric
	)

	g.mu.RLock()
	expectedRelease = g.expectedRelease
	for _, h := range g.hosts {
		if time.Since(h.lastSeen) > g.ttl {
			continue
		}
		liveHosts++
		totalSlots += h.slotsTotal
		usedSlots += h.slotsUsed
		committedSlots += h.committed()
		freeSlots += h.free()
		hibernated += h.hibernated
		if expectedRelease != "" && h.release != expectedRelease {
			releaseMismatches++
		}
		perHost = append(perHost, hostMetric{id: h.id, total: h.slotsTotal, used: h.slotsUsed, free: h.free()})
	}
	routes = len(g.route)
	g.mu.RUnlock()

	sort.Slice(perHost, func(i, j int) bool { return perHost[i].id < perHost[j].id })

	var b strings.Builder
	gauge := func(name, help string, val int) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, val)
	}
	release := metricRelease(os.Getenv("SANDBOX_RELEASE"))
	fmt.Fprintf(&b, "# HELP sandbox_build_info Release identity for rollout tracking.\n# TYPE sandbox_build_info gauge\nsandbox_build_info{component=\"gateway\",release=%q} 1\n", release)
	gauge("sandbox_hosts_live", "Number of hosts seen within the heartbeat TTL.", liveHosts)
	gauge("sandbox_slots_total", "Total sandbox slots across live hosts.", totalSlots)
	gauge("sandbox_slots_used", "Used sandbox slots across live hosts.", usedSlots)
	gauge("sandbox_slots_committed", "Running and in-flight reserved sandbox slots across live hosts, clamped per host.", committedSlots)
	// slots_free is host-reported allocatable capacity (minus in-flight
	// reservations) — the truth to PLACE against. NB the autoscaler's recording
	// rule does NOT use (slots_total - slots_free) for occupancy: a still-warming
	// host reports slots_free=0 as a placement gate while running zero sandboxes,
	// which total-free would misread as fully occupied and over-scale. The rule
	// uses (slots_committed + hibernated) instead. slots_free is for placement
	// only. slots_committed closes the scrape-time gap before worker heartbeats
	// report creates already assigned by the gateway.
	gauge("sandbox_slots_free", "Allocatable sandbox slots across live hosts (host-reported).", freeSlots)
	gauge("sandbox_routes", "Number of sandbox-id -> host routes the gateway holds.", routes)
	gauge("sandbox_hibernated", "Idle sandboxes frozen to disk across live hosts (hold no slot).", hibernated)
	gauge("sandbox_worker_release_mismatch", "Live workers gated from placement because their release is not current.", releaseMismatches)
	fmt.Fprintf(&b, "# HELP sandbox_worker_release_info Expected worker release used for placement gating.\n# TYPE sandbox_worker_release_info gauge\nsandbox_worker_release_info{release=%q} 1\n", expectedRelease)
	fmt.Fprintf(&b, "# HELP sandbox_create_rejected_total Creates 503'd for capacity (queue full or queue-wait expired). Retrying clients re-increment every Retry-After, so the rate approximates demand the saturated queue can no longer express.\n# TYPE sandbox_create_rejected_total counter\nsandbox_create_rejected_total %d\n", g.rejected.Load())
	// Gateway-side aggregate of successful creates. Scraped at 10s (gateway
	// /metrics), so rate() feeds the autoscaler's lead term at 10s resolution —
	// far fresher than the 30s-federated per-host sandbox_creates_ok_total.
	fmt.Fprintf(&b, "# HELP sandbox_creates_total Sandboxes the gateway successfully brought up.\n# TYPE sandbox_creates_total counter\nsandbox_creates_total %d\n", g.createsOK.Load())
	fmt.Fprintf(&b, "# HELP sandbox_direct_scale_out_total Queue-triggered direct scale-out requests submitted to the scaler.\n# TYPE sandbox_direct_scale_out_total counter\nsandbox_direct_scale_out_total %d\n", g.directScaleStarted.Load())
	fmt.Fprintf(&b, "# HELP sandbox_direct_scale_out_failed_total Queue-triggered direct scale-out requests that failed.\n# TYPE sandbox_direct_scale_out_failed_total counter\nsandbox_direct_scale_out_failed_total %d\n", g.directScaleFailed.Load())
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

func metricRelease(v string) string {
	if v == "" {
		return "unknown"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, v)
}
