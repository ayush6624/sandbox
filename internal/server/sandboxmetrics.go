package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ayush6624/sandbox/internal/agentapi"
	"github.com/ayush6624/sandbox/internal/metricsapi"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

// Per-sandbox utilization (docs/sandbox-metrics-plan.md, phase 1: host-side).
//
// Distinct from the usage ledger, which bills ALLOCATION and is durable. This
// is what a sandbox is actually consuming right now, sampled from the jailer's
// cgroup leaf, its tap counters and its rootfs file — no guest contact, so it
// cannot reset a sandbox's idle-hibernation clock and cannot wake a frozen one.
//
// History lives in a bounded in-memory ring, never in SQLite (25 rows/s of
// churn against the create path's database) and never in Prometheus with a
// sandbox label (cardinality — see handleMetrics). It is lost on restart and
// when a sandbox moves hosts, the same caveat /v1/usage prints as coverage.

const (
	defaultMetricsInterval = 5 * time.Second
	defaultMetricsHistory  = 360 // 30 min at the default interval
	maxMetricsSamples      = 1000
)

// rawSample is one tick's readings before rates are derived.
type rawSample struct {
	vcpus       int64
	cpuUsec     int64
	memBytes    int64
	haveCPU     bool // false when the leaf is unreadable (unjailed dev host, exited VMM)
	rootfsBytes int64
	rxBytes     int64
	txBytes     int64
	guest       *agentapi.Stats // nil when guest stats are off or the agent didn't answer
}

type sandboxStats struct {
	history int
	mu      sync.Mutex
	series  map[string]*statSeries
}

type statSeries struct {
	samples     []metricsapi.Sample
	generation  int64
	prevCPUUsec int64
	prevAt      time.Time
	havePrev    bool
}

func newSandboxStats(history int) *sandboxStats {
	if history <= 0 {
		history = defaultMetricsHistory
	}
	return &sandboxStats{history: history, series: map[string]*statSeries{}}
}

// record appends one tick, deriving the rates that need a predecessor.
func (st *sandboxStats) record(id string, now time.Time, raw rawSample) {
	st.mu.Lock()
	defer st.mu.Unlock()

	e := st.series[id]
	if e == nil {
		e = &statSeries{generation: 1}
		st.series[id] = e
	}
	s := metricsapi.Sample{
		Timestamp:        now,
		CPUCount:         raw.vcpus,
		HostMemBytes:     raw.memBytes,
		RootfsAllocBytes: raw.rootfsBytes,
		NetRxBytes:       raw.txBytes, // tap tx == guest rx
		NetTxBytes:       raw.rxBytes,
	}
	if g := raw.guest; g != nil {
		s.MemTotalBytes, s.MemUsedBytes = g.MemTotalBytes, g.MemTotalBytes-g.MemAvailableBytes
		s.DiskTotalBytes, s.DiskUsedBytes = g.DiskTotalBytes, g.DiskTotalBytes-g.DiskFreeBytes
		s.Load1, s.Processes = &g.Load1, g.Processes
	}
	if raw.haveCPU {
		s.CPUSecondsTotal = float64(raw.cpuUsec) / 1e6
		if e.havePrev && raw.cpuUsec < e.prevCPUUsec {
			// A fresh VMM: the counter restarted. Start a generation rather
			// than reporting a negative rate.
			e.generation++
			e.havePrev = false
		}
		if e.havePrev && raw.vcpus > 0 {
			if elapsed := now.Sub(e.prevAt).Seconds(); elapsed > 0 {
				s.CPUUsedPct = float64(raw.cpuUsec-e.prevCPUUsec) / 1e4 / elapsed / float64(raw.vcpus)
			}
		}
		e.prevCPUUsec, e.prevAt, e.havePrev = raw.cpuUsec, now, true
	}
	s.VMMGeneration = e.generation

	// Shifting a full ring copies at most `history` samples of ~100 B, which is
	// cheaper than the SQLite read that produced this tick.
	if len(e.samples) >= st.history {
		copy(e.samples, e.samples[len(e.samples)-st.history+1:])
		e.samples = e.samples[:st.history-1]
	}
	e.samples = append(e.samples, s)
}

// retain drops the series of sandboxes that no longer exist. Hibernated
// sandboxes are kept: they still exist, and a wake continues the series.
func (st *sandboxStats) retain(ids map[string]bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for id := range st.series {
		if !ids[id] {
			delete(st.series, id)
		}
	}
}

// query returns a copy of a sandbox's samples within the window, most recent
// last. limit keeps the NEWEST samples: a caller asking for one wants the
// latest, not the oldest.
func (st *sandboxStats) query(id string, from, to time.Time, limit int) []metricsapi.Sample {
	st.mu.Lock()
	defer st.mu.Unlock()
	e := st.series[id]
	if e == nil {
		return []metricsapi.Sample{}
	}
	out := make([]metricsapi.Sample, 0, len(e.samples))
	for _, s := range e.samples {
		if !from.IsZero() && s.Timestamp.Before(from) {
			continue
		}
		if !to.IsZero() && s.Timestamp.After(to) {
			continue
		}
		out = append(out, s)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// latest returns the most recent sample of every sandbox still being tracked,
// for the host aggregates on /metrics.
func (st *sandboxStats) latest() []metricsapi.Sample {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]metricsapi.Sample, 0, len(st.series))
	for _, e := range st.series {
		if n := len(e.samples); n > 0 {
			out = append(out, e.samples[n-1])
		}
	}
	return out
}

// sampleLoop ticks the collector. Guest stats run on every guestStatsEvery'th
// tick: polling the agent costs the tenant's own CPU and therefore shows up in
// the very number being measured, so the observer effect is halved by default.
func (s *Server) sampleLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for n := int64(0); ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sampleAll(ctx, s.cfg.MetricsGuestStats && n%guestStatsEvery == 0)
		}
	}
}

const (
	guestStatsEvery       = 2
	guestStatsConcurrency = 8
	guestStatsTimeout     = time.Second
)

func (s *Server) sampleAll(ctx context.Context, withGuest bool) {
	rows, err := s.reg.ListRouted(ctx)
	if err != nil {
		return // a transient read failure costs one tick, never a log storm
	}
	now := time.Now()
	known := make(map[string]bool, len(rows))
	type target struct {
		sb registry.Sandbox
		m  *vm.Machine
	}
	live := make([]target, 0, len(rows))
	for _, sb := range rows {
		known[sb.ID] = true
		m, ok := s.machines.Load(sb.ID)
		if !ok {
			continue // hibernated or mid-teardown: no VMM to sample, and we must not wake it
		}
		live = append(live, target{sb, m.(*vm.Machine)})
	}

	// Guest polls are network calls, so they fan out; the host-side reads below
	// are file reads and stay serial. All of them are stamped with the tick's
	// `now`, not with whenever the slowest guest answered.
	guest := make([]*agentapi.Stats, len(live))
	if withGuest {
		sem := make(chan struct{}, guestStatsConcurrency)
		var wg sync.WaitGroup
		for i, t := range live {
			if t.sb.GuestIP == "" {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				guest[i] = fetchGuestStats(ctx, t.sb.ID, t.sb.GuestIP)
			}()
		}
		wg.Wait()
	}

	for i, t := range live {
		eff := s.effectiveResources(t.sb)
		raw := rawSample{
			vcpus:       eff.Vcpus,
			rootfsBytes: allocatedBytes(t.sb.RootfsPath),
			guest:       guest[i],
		}
		if u, err := vm.SampleUsage(t.m); err == nil {
			raw.cpuUsec, raw.memBytes, raw.haveCPU = u.CPUUsec, u.MemBytes, true
		}
		raw.rxBytes, raw.txBytes = tapCounters(t.sb.TapDevice)
		s.stats.record(t.sb.ID, now, raw)
	}
	s.stats.retain(known)
}

// fetchGuestStats polls one guest's GET /stats.
//
// It deliberately does NOT go through handleAgentProxy. That path calls
// act.begin, which resets the idle-hibernation clock and pins the sandbox
// running — a sampler on it would stop every sandbox on the fleet from ever
// going idle. It also skips ensureRunning: the caller only reaches here for a
// sandbox with a live VMM, and a sampler must never wake a frozen one.
//
// The authority carries the sandbox id so a recycled guest IP can't hand this
// request a keep-alive connection to the dead VM that held the address before
// (see agentAuthority). Every failure — timeout, connection error, an old agent
// answering 404 — yields nil, and the tick records host-side fields alone.
func fetchGuestStats(ctx context.Context, id, guestIP string) *agentapi.Stats {
	ctx, cancel := context.WithTimeout(ctx, guestStatsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+agentAuthority(id, guestIP)+"/stats", nil)
	if err != nil {
		return nil
	}
	req.Host = agentHostPort(guestIP) // the guest sees a plain ip:port
	resp, err := agentClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil
	}
	var st agentapi.Stats
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&st); err != nil {
		return nil
	}
	return &st
}

// tapCounters reads the host side of a sandbox's virtual NIC. Absent or
// unreadable (destroyed tap, non-Linux) reads as zero, which a consumer sees as
// a counter that stopped advancing rather than as an error.
func tapCounters(tap string) (rx, tx int64) {
	if tap == "" {
		return 0, 0
	}
	base := "/sys/class/net/" + tap + "/statistics/"
	return readInt64File(base + "rx_bytes"), readInt64File(base + "tx_bytes")
}

func readInt64File(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := strconv.ParseInt(string(bytes.TrimSpace(b)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// allocatedBytes is what the rootfs actually occupies, not its apparent size:
// every per-VM rootfs is a sparse reflink clone of the same ~2 GB base, so
// apparent size is identical for every sandbox and tells an operator nothing.
func allocatedBytes(path string) int64 {
	if path == "" {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return int64(st.Blocks) * 512
}

func (s *Server) handleSandboxMetrics(w http.ResponseWriter, r *http.Request) {
	// Validate before resolving: a malformed window is a bad request whatever
	// the id turns out to be, and a silently dropped filter is a wrong number
	// that looks right.
	q := r.URL.Query()
	from, err := parseTimeParam(q.Get("from"))
	if err != nil {
		httpError(w, http.StatusBadRequest, fmt.Errorf("from: %w", err))
		return
	}
	to, err := parseTimeParam(q.Get("to"))
	if err != nil {
		httpError(w, http.StatusBadRequest, fmt.Errorf("to: %w", err))
		return
	}
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 0 {
			httpError(w, http.StatusBadRequest, fmt.Errorf("limit must be a non-negative integer"))
			return
		}
	}
	if limit == 0 || limit > maxMetricsSamples {
		limit = maxMetricsSamples
	}
	id := r.PathValue("id")
	sb, err := s.reg.Get(r.Context(), id)
	if err != nil {
		httpError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, metricsapi.SandboxMetrics{
		Samples:         s.stats.query(id, from, to, limit),
		State:           sb.Status,
		IntervalSeconds: s.metricsInterval().Seconds(),
	})
}

func parseTimeParam(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, raw)
}

func (s *Server) metricsInterval() time.Duration {
	if s.cfg.MetricsInterval > 0 {
		return s.cfg.MetricsInterval
	}
	return defaultMetricsInterval
}

// cpuUtilBuckets are the upper bounds of the per-sandbox CPU-utilization
// histogram on /metrics. Nothing else on this host measures whether the
// deliberate CPU oversubscription is safe, or how many resident sandboxes are
// doing nothing at all.
var cpuUtilBuckets = []float64{1, 10, 25, 50, 75, 90, 100}

// writeSandboxStatMetrics exports host AGGREGATES of the per-sandbox series.
// No sandbox label, for the same cardinality reason as the billing counters.
func (s *Server) writeSandboxStatMetrics(b *strings.Builder) {
	samples := s.stats.latest()
	var hostMem, rootfs, rx, tx int64
	counts := make([]int64, len(cpuUtilBuckets))
	var sum float64
	for _, sm := range samples {
		hostMem += sm.HostMemBytes
		rootfs += sm.RootfsAllocBytes
		rx += sm.NetRxBytes
		tx += sm.NetTxBytes
		sum += sm.CPUUsedPct
		for i, bound := range cpuUtilBuckets {
			if sm.CPUUsedPct <= bound {
				counts[i]++
			}
		}
	}
	fmt.Fprintf(b, "# HELP sandbox_host_mem_bytes Sum of running sandboxes' VMM cgroup memory charge (guest pages touched).\n# TYPE sandbox_host_mem_bytes gauge\nsandbox_host_mem_bytes %d\n", hostMem)
	fmt.Fprintf(b, "# HELP sandbox_rootfs_alloc_bytes Sum of per-VM rootfs blocks allocated on the data disk.\n# TYPE sandbox_rootfs_alloc_bytes gauge\nsandbox_rootfs_alloc_bytes %d\n", rootfs)
	fmt.Fprintf(b, "# HELP sandbox_net_bytes_total Guest network bytes since each sandbox's current VMM started.\n# TYPE sandbox_net_bytes_total gauge\nsandbox_net_bytes_total{dir=\"rx\"} %d\nsandbox_net_bytes_total{dir=\"tx\"} %d\n", rx, tx)
	fmt.Fprintf(b, "# HELP sandbox_cpu_utilization Per-sandbox CPU used as a percentage of its allocated vCPUs.\n# TYPE sandbox_cpu_utilization histogram\n")
	for i, bound := range cpuUtilBuckets {
		fmt.Fprintf(b, "sandbox_cpu_utilization_bucket{le=\"%g\"} %d\n", bound, counts[i])
	}
	fmt.Fprintf(b, "sandbox_cpu_utilization_bucket{le=\"+Inf\"} %d\nsandbox_cpu_utilization_sum %.4f\nsandbox_cpu_utilization_count %d\n",
		len(samples), sum, len(samples))
}
