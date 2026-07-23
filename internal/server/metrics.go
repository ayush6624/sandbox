package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// handleMetrics serves this host's occupancy + lifecycle counters in Prometheus
// text exposition format (v0.0.4), mirroring the gateway's hand-rolled
// handleMetrics (internal/gateway/metrics.go) — the repo keeps its dependency
// tree minimal, so a handful of series don't justify client_golang.
//
// The gateway aggregates fleet-wide slots from heartbeats; this exposes the
// per-host detail a heartbeat elides: WHICH pool is the binding constraint
// (tap/IP/port), committed vs budgeted memory, and lifecycle rates
// (create/hibernate/wake) an operator needs to see a host misbehaving. Prometheus
// (on the control VM) scrapes it behind the same bearer auth as every other
// endpoint on the TCP listener; over the Unix socket it's auth-free like the
// rest.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	st, err := s.reg.Stats(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Errorf("collect metrics: %w", err))
		return
	}

	var b strings.Builder
	gauge := func(name, help string, val int64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, val)
	}
	counter := func(name, help string, val int64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, val)
	}

	gauge("sandbox_uptime_seconds", "Seconds since this server process started.", int64(time.Since(s.startedAt).Seconds()))

	gauge("sandbox_running", "Sandboxes running on this host (hold a tap, IP, port, and guest memory).", int64(st.Running))
	gauge("sandbox_hibernated", "Sandboxes frozen to disk on this host (hold only their host port).", int64(st.Hibernated))
	gauge("sandbox_slots_free", "Allocatable slots right now: smallest per-pool availability, memory-bounded.", int64(st.SlotsFree))

	// Per-pool used/total so an operator can see which pool binds. The label
	// distinguishes the three pools that a create draws from.
	fmt.Fprintf(&b, "# HELP sandbox_pool_used Resources held per pool (tap/ip: running only; port: running+hibernated+extra).\n# TYPE sandbox_pool_used gauge\n")
	fmt.Fprintf(&b, "sandbox_pool_used{pool=\"tap\"} %d\n", st.TapUsed)
	fmt.Fprintf(&b, "sandbox_pool_used{pool=\"ip\"} %d\n", st.IPUsed)
	fmt.Fprintf(&b, "sandbox_pool_used{pool=\"port\"} %d\n", st.PortUsed)
	fmt.Fprintf(&b, "# HELP sandbox_pool_total Pool capacity.\n# TYPE sandbox_pool_total gauge\n")
	fmt.Fprintf(&b, "sandbox_pool_total{pool=\"tap\"} %d\n", st.TapTotal)
	fmt.Fprintf(&b, "sandbox_pool_total{pool=\"ip\"} %d\n", st.IPTotal)
	fmt.Fprintf(&b, "sandbox_pool_total{pool=\"port\"} %d\n", st.PortTotal)

	gauge("sandbox_committed_mem_mib", "Sum of running sandboxes' effective mem_mib + VMM overhead.", st.CommittedMemMIB)
	gauge("sandbox_mem_budget_mib", "Committed-memory admission ceiling (0 = disabled).", st.MemBudgetMIB)

	var goldenReady int64
	if s.golden.Load() != nil {
		goldenReady = 1
	}
	gauge("sandbox_golden_ready", "1 if the golden snapshot is staged (hot create available), else 0.", goldenReady)
	gauge("sandbox_create_inflight", "Bring-ups currently holding a create-concurrency slot.", int64(len(s.createSem)))
	gauge("sandbox_create_concurrency", "Max concurrent bring-ups (create-concurrency semaphore size).", int64(cap(s.createSem)))

	counter("sandbox_creates_ok_total", "POST /sandboxes that brought a sandbox up (hot clone or cold boot).", s.met.createsOK.Load())
	counter("sandbox_creates_error_total", "POST /sandboxes that failed to bring a sandbox up (after validation).", s.met.createsErr.Load())
	counter("sandbox_hibernations_total", "Sandboxes frozen to disk (idle reaper, manual, or shutdown).", s.met.hibernations.Load())
	counter("sandbox_wakes_total", "Sandboxes successfully thawed from hibernation.", s.met.wakes.Load())
	counter("sandbox_wake_failures_total", "Wake attempts that rolled back to hibernated.", s.met.wakeFailures.Load())

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
