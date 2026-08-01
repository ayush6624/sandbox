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
		id                      string
		total, used, free, warm int
	}
	var (
		liveHosts             int
		totalSlots, usedSlots int
		committedSlots        int
		freeSlots             int
		warmReady             int
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
		warmReady += h.warmFree()
		hibernated += h.hibernated
		if expectedRelease != "" && h.release != expectedRelease {
			releaseMismatches++
		}
		perHost = append(perHost, hostMetric{id: h.id, total: h.slotsTotal, used: h.slotsUsed, free: h.free(), warm: h.warmFree()})
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
	gauge("sandbox_warm_ready", "Fully initialized ready VMs available across live hosts.", warmReady)
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
	// The grow-only watermark the gateway has already asked the MIG to reach.
	// Diagnostic only — it is NOT the scale-in ceiling. It re-baselines to the
	// live host count whenever the queue empties, so it can read lower than the
	// fleet actually is.
	gauge("sandbox_scale_out_requested", "Largest worker count the gateway has requested (grow-only watermark).", int(g.directRequested.Load()))
	// The provider's OWN target worker count, and the authority the autoscaler's
	// scale-in ceiling is built from. Capping on sandbox_hosts_live instead let
	// the autoscaler scale out past its cap (from=5 to=6, measured 2026-07-28):
	// heartbeats also count resumed standby workers that sit outside the target,
	// so hosts_live read 8 against a target of 5 and a latched max_over_time
	// peak of 6 was a legal scale-up. Emitted only once a poll has succeeded, so
	// a provider error publishes nothing rather than a zero that would collapse
	// the ceiling and trigger a spurious scale-in.
	if g.migTargetKnown.Load() {
		gauge("sandbox_mig_target_size", "Provider target worker count (authority for the autoscaler scale-in ceiling).", int(g.migTarget.Load()))
	}
	// Queued creates are demand without a slot — the recording rule adds this
	// to slots_used so a burst pulls scale-up before any create lands.
	gauge("sandbox_create_queue_depth", "Creates waiting in the gateway's bounded queue for a free slot.", int(g.queued.Load()))
	// Cross-host adopt on a route miss. suppressed{reason} is the load a
	// hostname scan is NOT allowed to put on the workers: malformed = rejected
	// on shape alone, cached = known-absent, throttled = fleet-wide dispatch
	// limit. A rising throttled rate with a low dispatch rate means something is
	// probing ids; a rising wait_timeout means real adopts are slower than
	// requestAdoptWait and clients are relying on the retry-joins-flight path.
	g.adoptMu.Lock()
	adoptInflightNow := len(g.adopts)
	g.adoptMu.Unlock()
	gauge("sandbox_adopt_inflight", "Cross-host adopts running in the background.", adoptInflightNow)
	gauge("sandbox_adopt_negative_cache_entries", "Ids cached as having no durable record anywhere (bounded).", g.notFound.len())
	fmt.Fprintf(&b, "# HELP sandbox_adopt_dispatched_total Cross-host adopts dispatched to a worker.\n# TYPE sandbox_adopt_dispatched_total counter\nsandbox_adopt_dispatched_total %d\n", g.adoptDispatched.Load())
	fmt.Fprintf(&b, "# HELP sandbox_adopt_wait_timeout_total Requests that stopped waiting on an in-flight adopt and 404'd.\n# TYPE sandbox_adopt_wait_timeout_total counter\nsandbox_adopt_wait_timeout_total %d\n", g.adoptWaitTimeouts.Load())
	fmt.Fprintf(&b, "# HELP sandbox_adopt_suppressed_total Route misses answered without dispatching an adopt.\n# TYPE sandbox_adopt_suppressed_total counter\n")
	fmt.Fprintf(&b, "sandbox_adopt_suppressed_total{reason=\"malformed_id\"} %d\n", g.adoptSuppressedMalformed.Load())
	fmt.Fprintf(&b, "sandbox_adopt_suppressed_total{reason=\"cached\"} %d\n", g.adoptSuppressedCached.Load())
	fmt.Fprintf(&b, "sandbox_adopt_suppressed_total{reason=\"throttled\"} %d\n", g.adoptSuppressedThrottled.Load())
	if g.raw != nil {
		pending, active, releasing, generation := g.raw.stats()
		fmt.Fprintf(&b, "# HELP sandbox_raw_leases Durable public TCP leases by state.\n# TYPE sandbox_raw_leases gauge\n")
		fmt.Fprintf(&b, "sandbox_raw_leases{state=\"pending\"} %d\n", pending)
		fmt.Fprintf(&b, "sandbox_raw_leases{state=\"active\"} %d\n", active)
		fmt.Fprintf(&b, "sandbox_raw_leases{state=\"releasing\"} %d\n", releasing)
		fmt.Fprintf(&b, "# HELP sandbox_raw_allocations_total Raw TCP allocations by result.\n# TYPE sandbox_raw_allocations_total counter\n")
		fmt.Fprintf(&b, "sandbox_raw_allocations_total{result=\"ok\"} %d\n", g.raw.allocOK.Load())
		fmt.Fprintf(&b, "sandbox_raw_allocations_total{result=\"error\"} %d\n", g.raw.allocError.Load())
		fmt.Fprintf(&b, "# HELP sandbox_raw_reconcile_total Pending raw lease reconciliations by result.\n# TYPE sandbox_raw_reconcile_total counter\n")
		fmt.Fprintf(&b, "sandbox_raw_reconcile_total{result=\"ok\"} %d\n", g.raw.reconcileOK.Load())
		fmt.Fprintf(&b, "sandbox_raw_reconcile_total{result=\"error\"} %d\n", g.raw.reconcileError.Load())
		fmt.Fprintf(&b, "# HELP sandbox_raw_route_conflicts_total Worker heartbeat mappings that conflict with the durable raw index.\n# TYPE sandbox_raw_route_conflicts_total counter\nsandbox_raw_route_conflicts_total %d\n", g.raw.conflicts.Load())
		fmt.Fprintf(&b, "# HELP sandbox_raw_index_generation Current GCS generation of the raw lease index.\n# TYPE sandbox_raw_index_generation gauge\nsandbox_raw_index_generation %d\n", generation)
	}

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
	fmt.Fprintf(&b, "# HELP sandbox_host_warm_ready Ready VMs available on a live host, minus warm reservations.\n# TYPE sandbox_host_warm_ready gauge\n")
	for _, h := range perHost {
		fmt.Fprintf(&b, "sandbox_host_warm_ready{host=%q} %d\n", h.id, h.warm)
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
