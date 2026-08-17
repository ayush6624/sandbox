package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/agentapi"
)

// A busy sandbox's utilization is the cgroup delta over elapsed wall time,
// scaled by its allocated vCPUs — so one fully-consumed vCPU of two reads 50%,
// not 100%.
func TestRecordDerivesCPUPercent(t *testing.T) {
	st := newSandboxStats(10)
	t0 := time.Unix(1700000000, 0)
	st.record("sb", t0, rawSample{vcpus: 2, cpuUsec: 1_000_000, haveCPU: true})
	st.record("sb", t0.Add(10*time.Second), rawSample{vcpus: 2, cpuUsec: 11_000_000, haveCPU: true})

	got := st.query("sb", time.Time{}, time.Time{}, 0)
	if len(got) != 2 {
		t.Fatalf("samples = %d, want 2", len(got))
	}
	if got[0].CPUUsedPct != 0 {
		t.Errorf("first sample pct = %v, want 0: it has no predecessor to difference against", got[0].CPUUsedPct)
	}
	if got[1].CPUUsedPct != 50 {
		t.Errorf("pct = %v, want 50 (10 CPU-seconds over 10s on 2 vcpus)", got[1].CPUUsedPct)
	}
	if got[1].CPUSecondsTotal != 11 {
		t.Errorf("cpu_seconds_total = %v, want 11", got[1].CPUSecondsTotal)
	}
}

// A wake or restore replaces the VMM and restarts its counters. That must
// start a new generation, never produce a negative rate.
func TestRecordCounterResetStartsGeneration(t *testing.T) {
	st := newSandboxStats(10)
	t0 := time.Unix(1700000000, 0)
	st.record("sb", t0, rawSample{vcpus: 1, cpuUsec: 50_000_000, haveCPU: true})
	st.record("sb", t0.Add(5*time.Second), rawSample{vcpus: 1, cpuUsec: 1_000_000, haveCPU: true})
	st.record("sb", t0.Add(10*time.Second), rawSample{vcpus: 1, cpuUsec: 3_000_000, haveCPU: true})

	got := st.query("sb", time.Time{}, time.Time{}, 0)
	if got[0].VMMGeneration != 1 {
		t.Errorf("generation = %d, want 1", got[0].VMMGeneration)
	}
	if got[1].VMMGeneration != 2 {
		t.Errorf("generation after reset = %d, want 2", got[1].VMMGeneration)
	}
	if got[1].CPUUsedPct != 0 {
		t.Errorf("pct across a reset = %v, want 0 (not a negative rate)", got[1].CPUUsedPct)
	}
	if got[2].CPUUsedPct != 40 {
		t.Errorf("pct after reset = %v, want 40 (2 CPU-seconds over 5s on 1 vcpu)", got[2].CPUUsedPct)
	}
}

// The tap is the HOST's end of the guest NIC, so its counters are the guest's
// inverted. Getting this backwards silently reports egress as ingress.
func TestRecordInvertsTapCounters(t *testing.T) {
	st := newSandboxStats(4)
	st.record("sb", time.Unix(1700000000, 0), rawSample{rxBytes: 100, txBytes: 700})
	got := st.query("sb", time.Time{}, time.Time{}, 0)[0]
	if got.NetRxBytes != 700 || got.NetTxBytes != 100 {
		t.Errorf("guest rx/tx = %d/%d, want 700/100", got.NetRxBytes, got.NetTxBytes)
	}
}

func TestRingEvictsOldestAndLimitKeepsNewest(t *testing.T) {
	st := newSandboxStats(3)
	t0 := time.Unix(1700000000, 0)
	for i := 0; i < 6; i++ {
		st.record("sb", t0.Add(time.Duration(i)*time.Second), rawSample{vcpus: 1, rootfsBytes: int64(i)})
	}
	got := st.query("sb", time.Time{}, time.Time{}, 0)
	if len(got) != 3 {
		t.Fatalf("retained = %d, want 3", len(got))
	}
	if got[0].RootfsAllocBytes != 3 || got[2].RootfsAllocBytes != 5 {
		t.Errorf("retained %v..%v, want the newest three (3..5)", got[0].RootfsAllocBytes, got[2].RootfsAllocBytes)
	}
	if one := st.query("sb", time.Time{}, time.Time{}, 1); len(one) != 1 || one[0].RootfsAllocBytes != 5 {
		t.Errorf("limit=1 returned %v, want the newest sample", one)
	}
}

// A hibernated sandbox stops producing samples but keeps the ones it has: a
// destroyed one is forgotten. retain() is the only thing that frees a series,
// so a wrong `known` set is either a leak or silent data loss.
func TestRetainKeepsHibernatedDropsDestroyed(t *testing.T) {
	st := newSandboxStats(4)
	now := time.Unix(1700000000, 0)
	st.record("frozen", now, rawSample{vcpus: 1})
	st.record("gone", now, rawSample{vcpus: 1})

	st.retain(map[string]bool{"frozen": true})

	if len(st.query("frozen", time.Time{}, time.Time{}, 0)) != 1 {
		t.Error("hibernated sandbox lost its samples")
	}
	if len(st.query("gone", time.Time{}, time.Time{}, 0)) != 0 {
		t.Error("destroyed sandbox kept its samples")
	}
}

func TestQueryWindowFilters(t *testing.T) {
	st := newSandboxStats(10)
	t0 := time.Unix(1700000000, 0)
	for i := 0; i < 5; i++ {
		st.record("sb", t0.Add(time.Duration(i)*time.Minute), rawSample{vcpus: 1})
	}
	got := st.query("sb", t0.Add(time.Minute), t0.Add(3*time.Minute), 0)
	if len(got) != 3 {
		t.Fatalf("windowed = %d, want 3", len(got))
	}
	if !got[0].Timestamp.Equal(t0.Add(time.Minute)) {
		t.Errorf("first = %v, want %v", got[0].Timestamp, t0.Add(time.Minute))
	}
}

// The sampler reads the host's view of a sandbox and never the guest's. This
// pins the two file readers that make that possible.
func TestHostReaders(t *testing.T) {
	if rx, tx := tapCounters(""); rx != 0 || tx != 0 {
		t.Errorf("no tap = %d/%d, want 0/0", rx, tx)
	}
	if rx, tx := tapCounters("tap-does-not-exist"); rx != 0 || tx != 0 {
		t.Errorf("absent tap = %d/%d, want 0/0: a destroyed tap must read as a stalled counter, not an error", rx, tx)
	}
	if got := allocatedBytes("/no/such/rootfs"); got != 0 {
		t.Errorf("absent rootfs = %d, want 0", got)
	}
	f := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(f, make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := allocatedBytes(f); got <= 0 {
		t.Errorf("allocated bytes = %d, want > 0", got)
	}
}

// The guest reports what it has and what is free; a caller wants what is USED.
// The conversion happens once, here, so every consumer agrees.
func TestRecordMergesGuestStats(t *testing.T) {
	st := newSandboxStats(4)
	st.record("sb", time.Unix(1700000000, 0), rawSample{vcpus: 1, guest: &agentapi.Stats{
		MemTotalBytes: 1000, MemAvailableBytes: 250,
		DiskTotalBytes: 8000, DiskFreeBytes: 3000,
		Load1: 1.5, Processes: 42,
	}})
	got := st.query("sb", time.Time{}, time.Time{}, 0)[0]
	if got.MemUsedBytes != 750 || got.MemTotalBytes != 1000 {
		t.Errorf("mem used/total = %d/%d, want 750/1000", got.MemUsedBytes, got.MemTotalBytes)
	}
	if got.DiskUsedBytes != 5000 || got.DiskTotalBytes != 8000 {
		t.Errorf("disk used/total = %d/%d, want 5000/8000", got.DiskUsedBytes, got.DiskTotalBytes)
	}
	if got.Load1 == nil || *got.Load1 != 1.5 || got.Processes != 42 {
		t.Errorf("load1/processes = %v/%d, want 1.5/42", got.Load1, got.Processes)
	}
}

// An agent predating GET /stats 404s it, and guest stats can be off entirely.
// Either way the tick still records host-side fields, and the guest fields stay
// ABSENT rather than reporting a zero that reads as "no memory in use".
func TestRecordWithoutGuestStatsOmitsGuestFields(t *testing.T) {
	st := newSandboxStats(4)
	st.record("sb", time.Unix(1700000000, 0), rawSample{vcpus: 1, memBytes: 512, guest: nil})
	got := st.query("sb", time.Time{}, time.Time{}, 0)[0]
	if got.HostMemBytes != 512 {
		t.Errorf("host mem = %d, want 512: host-side fields survive a silent agent", got.HostMemBytes)
	}
	if got.MemTotalBytes != 0 || got.Load1 != nil {
		t.Errorf("guest fields = %d/%v, want absent", got.MemTotalBytes, got.Load1)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "mem_used_bytes") || strings.Contains(string(body), "load1") {
		t.Errorf("body = %s, want guest fields omitted entirely", body)
	}
}

// The /metrics aggregates carry no sandbox label — that is the whole reason
// the per-sandbox series lives in the ring instead — and the histogram buckets
// must be cumulative or Prometheus reads the distribution backwards.
func TestSandboxStatMetricsAggregate(t *testing.T) {
	s := testMeteringServer(t)
	now := time.Unix(1700000000, 0)
	for i, pct := range []float64{5, 5, 80} {
		s.stats.record(string(rune('a'+i)), now, rawSample{vcpus: 1, haveCPU: true, memBytes: 100, rootfsBytes: 10, rxBytes: 1, txBytes: 2})
		// Second tick at a rate that yields the wanted utilization.
		s.stats.record(string(rune('a'+i)), now.Add(time.Second),
			rawSample{vcpus: 1, cpuUsec: int64(pct * 1e4), haveCPU: true, memBytes: 100, rootfsBytes: 10, rxBytes: 1, txBytes: 2})
	}

	var b strings.Builder
	s.writeSandboxStatMetrics(&b)
	body := b.String()
	if strings.Contains(body, "sandbox_id") {
		t.Error("per-sandbox label leaked into /metrics: cardinality bomb")
	}
	got := parseMetrics(t, body)
	if got["sandbox_host_mem_bytes"] != 300 {
		t.Errorf("host mem sum = %d, want 300", got["sandbox_host_mem_bytes"])
	}
	if got[`sandbox_net_bytes_total{dir="rx"}`] != 6 {
		t.Errorf("guest rx sum = %d, want 6 (tap tx of 2, three sandboxes)", got[`sandbox_net_bytes_total{dir="rx"}`])
	}
	if got["sandbox_cpu_utilization_count"] != 3 {
		t.Errorf("histogram count = %d, want 3", got["sandbox_cpu_utilization_count"])
	}
	// Cumulative: two idle sandboxes at or below 10%, all three by 100%.
	if got[`sandbox_cpu_utilization_bucket{le="10"}`] != 2 {
		t.Errorf("le=10 bucket = %d, want 2", got[`sandbox_cpu_utilization_bucket{le="10"}`])
	}
	if got[`sandbox_cpu_utilization_bucket{le="100"}`] != 3 {
		t.Errorf("le=100 bucket = %d, want 3 (buckets must be cumulative)", got[`sandbox_cpu_utilization_bucket{le="100"}`])
	}
}

// Guest sums must cover only the sandboxes that actually reported, and the
// reporting count must ship with them: a sandbox whose baked agent 404s
// GET /stats contributes zeros, so folding it into the denominator would make a
// fleet-wide "guest memory used" ratio read low exactly when coverage is worst.
func TestSandboxStatMetricsGuestAggregate(t *testing.T) {
	s := testMeteringServer(t)
	now := time.Unix(1700000000, 0)
	s.stats.record("a", now, rawSample{vcpus: 1, guest: &agentapi.Stats{
		MemTotalBytes: 1000, MemAvailableBytes: 250, DiskTotalBytes: 8000, DiskFreeBytes: 3000, Processes: 40,
	}})
	s.stats.record("b", now, rawSample{vcpus: 1, guest: &agentapi.Stats{
		MemTotalBytes: 1000, MemAvailableBytes: 750, DiskTotalBytes: 8000, DiskFreeBytes: 6000, Processes: 2,
	}})
	s.stats.record("c", now, rawSample{vcpus: 1, memBytes: 512}) // old agent: no guest data

	var b strings.Builder
	s.writeSandboxStatMetrics(&b)
	got := parseMetrics(t, b.String())
	if got["sandbox_guest_stats_reporting"] != 2 {
		t.Errorf("reporting = %d, want 2 (the silent agent is not a denominator)", got["sandbox_guest_stats_reporting"])
	}
	if got[`sandbox_guest_mem_bytes{state="used"}`] != 1000 || got[`sandbox_guest_mem_bytes{state="total"}`] != 2000 {
		t.Errorf("guest mem used/total = %d/%d, want 1000/2000",
			got[`sandbox_guest_mem_bytes{state="used"}`], got[`sandbox_guest_mem_bytes{state="total"}`])
	}
	if got[`sandbox_guest_disk_bytes{state="used"}`] != 7000 {
		t.Errorf("guest disk used = %d, want 7000", got[`sandbox_guest_disk_bytes{state="used"}`])
	}
	if got["sandbox_guest_processes"] != 42 {
		t.Errorf("processes = %d, want 42", got["sandbox_guest_processes"])
	}
}

// THE regression test for this feature. Every host→guest path bumps the
// activity tracker, which resets the idle-hibernation clock and pins the
// sandbox running. A sampler that ran through one of those paths would freeze
// idle hibernation fleet-wide — blowing mem_budget_mib and every bill — and it
// would do it silently, because nothing else asserts a sandbox still goes idle.
// Sampling must therefore leave no activity record at all.
func TestSamplingIsNotActivity(t *testing.T) {
	s := testMeteringServer(t)
	ctx := context.Background()
	if _, err := s.reg.Create(ctx, "sb1", "", "/tmp/sb1.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}

	for i := 0; i < 3; i++ {
		s.sampleAll(ctx, false)
	}

	if _, _, ok := s.act.idleFor("sb1"); ok {
		t.Error("sampling created an activity record: idle hibernation would never fire again")
	}
}

// A sandbox with no samples yet answers an empty array rather than blocking
// for a tick — what a client sees in the first seconds of a sandbox's life.
func TestHandleSandboxMetricsEmptySeries(t *testing.T) {
	s := testMeteringServer(t)
	if _, err := s.reg.Create(context.Background(), "sb1", "", "/tmp/sb1.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sandboxes/sb1/metrics", nil)
	req.SetPathValue("id", "sb1")
	s.handleSandboxMetrics(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"samples":[]`) {
		t.Errorf("body = %s, want an empty samples array", rec.Body)
	}
}

// A malformed window is rejected before the sandbox is even resolved: a
// silently dropped filter is a wrong number that looks right.
func TestHandleSandboxMetricsRejectsBadWindow(t *testing.T) {
	s := testMeteringServer(t)
	for _, q := range []string{"?from=yesterday", "?to=nope", "?limit=-1", "?limit=abc"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/sandboxes/sb1/metrics"+q, nil)
		req.SetPathValue("id", "sb1")
		s.handleSandboxMetrics(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, rec.Code)
		}
	}
}

func TestHandleSandboxMetricsUnknownSandbox(t *testing.T) {
	s := testMeteringServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sandboxes/nope/metrics", nil)
	req.SetPathValue("id", "nope")
	s.handleSandboxMetrics(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
