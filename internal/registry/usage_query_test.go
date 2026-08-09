package registry

import (
	"context"
	"math"
	"testing"
	"time"
)

// Totals are aggregated in SQL so a truncated page still reports the true
// amount owed, while every other consumer (the gateway fold, the v1 adapter)
// uses the Go accessors. Two implementations of one billing formula can drift
// silently, and the drift would show up as money. This pins them together.
func TestUsageTotalsMatchGoAccessors(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	seed := []struct {
		id                  string
		vcpus, mem, cpuUsec int64
		ageSec              int64
		closeAfterSec       int64 // 0 = leave open
	}{
		{"sb1", 2, 1024, 3_500_000, 600, 300},
		{"sb2", 4, 4096, 12_000_000, 400, 120},
		{"sb3", 1, 512, 900_000, 200, 0},
	}
	for _, s := range seed {
		if _, err := r.OpenUsageInterval(ctx, s.id, "host-a", "vm-"+s.id, s.vcpus, s.mem, 0, nil); err != nil {
			t.Fatalf("open %s: %v", s.id, err)
		}
		started := time.Now().UTC().Add(-time.Duration(s.ageSec) * time.Second).Unix()
		if _, err := r.db.ExecContext(ctx,
			`UPDATE usage_intervals SET started_at=?, last_seen_at=?, cpu_usec=? WHERE sandbox_id=?`,
			started, started+s.ageSec/2, s.cpuUsec, s.id); err != nil {
			t.Fatalf("age %s: %v", s.id, err)
		}
		if s.closeAfterSec > 0 {
			if _, err := r.db.ExecContext(ctx,
				`UPDATE usage_intervals SET ended_at=?, end_reason=? WHERE sandbox_id=?`,
				started+s.closeAfterSec, EndDestroy, s.id); err != nil {
				t.Fatalf("close %s: %v", s.id, err)
			}
		}
	}

	rows, _, err := r.QueryUsage(ctx, UsageQuery{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	sql, err := r.UsageTotalsFor(ctx, UsageQuery{})
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	assertTotalsEqual(t, "sql vs go", sql, SumUsage(rows))
	if sql.Intervals != 3 || sql.OpenIntervals != 1 {
		t.Fatalf("counts wrong: %+v", sql)
	}
}

// A limit bounds the rows, never the money. A caller that pages through a busy
// host must still see the full total on the first page.
func TestUsageTotalsAreExactWhenRowsAreTruncated(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if _, err := r.OpenUsageInterval(ctx, id, "host-a", "vm-"+id, 2, 1024, 0, nil); err != nil {
			t.Fatalf("open %s: %v", id, err)
		}
		if _, _, err := r.CloseUsageInterval(ctx, id, EndDestroy, 1_000_000); err != nil {
			t.Fatalf("close %s: %v", id, err)
		}
	}

	rows, truncated, err := r.QueryUsage(ctx, UsageQuery{Limit: 2})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 || !truncated {
		t.Fatalf("rows=%d truncated=%v, want 2 and true", len(rows), truncated)
	}
	totals, err := r.UsageTotalsFor(ctx, UsageQuery{Limit: 2})
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.Intervals != 5 {
		t.Fatalf("limit leaked into totals: %+v", totals)
	}
	if totals.CPUSeconds != 5 {
		t.Fatalf("cpu seconds=%v, want 5", totals.CPUSeconds)
	}
}

// The window selects by overlap: a sandbox that has been running since before
// the window is exactly the one whose usage the window is asking about.
func TestQueryUsageSelectsByOverlap(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	now := time.Now().UTC()

	seed := map[string][2]int64{
		// sandbox: {startedOffsetSec, endedOffsetSec} relative to now; end 0 = open
		"before":    {-7200, -5400}, // entirely before the window
		"overlaps":  {-7200, -1200}, // started before, ended inside
		"inside":    {-1800, -600},
		"stillopen": {-9000, 0},
		"after":     {-60, 0}, // starts inside the window, still open
	}
	for id, span := range seed {
		if _, err := r.OpenUsageInterval(ctx, id, "host-a", "vm-"+id, 1, 512, 0, nil); err != nil {
			t.Fatalf("open %s: %v", id, err)
		}
		started := now.Unix() + span[0]
		if span[1] == 0 {
			if _, err := r.db.ExecContext(ctx,
				`UPDATE usage_intervals SET started_at=?, last_seen_at=? WHERE sandbox_id=?`,
				started, now.Unix(), id); err != nil {
				t.Fatalf("age %s: %v", id, err)
			}
			continue
		}
		if _, err := r.db.ExecContext(ctx,
			`UPDATE usage_intervals SET started_at=?, last_seen_at=?, ended_at=? WHERE sandbox_id=?`,
			started, now.Unix()+span[1], now.Unix()+span[1], id); err != nil {
			t.Fatalf("age %s: %v", id, err)
		}
	}

	rows, _, err := r.QueryUsage(ctx, UsageQuery{From: now.Add(-time.Hour), To: now})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	got := map[string]bool{}
	for _, iv := range rows {
		got[iv.SandboxID] = true
	}
	for _, want := range []string{"overlaps", "inside", "stillopen", "after"} {
		if !got[want] {
			t.Fatalf("%s missing from an overlapping window: %v", want, got)
		}
	}
	if got["before"] {
		t.Fatalf("interval that ended before the window was selected: %v", got)
	}
}

// A sandbox_id filter must reach intervals whose sandbox no longer exists —
// that is most of them, and all of the ones an invoice is made of.
func TestQueryUsageBySandboxSurvivesTheSandbox(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	if _, err := r.OpenUsageInterval(ctx, "gone", "host-a", "vm-1", 2, 1024, 0, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := r.CloseUsageInterval(ctx, "gone", EndDestroy, 2_000_000); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := r.OpenUsageInterval(ctx, "other", "host-a", "vm-2", 2, 1024, 0, nil); err != nil {
		t.Fatalf("open other: %v", err)
	}

	rows, _, err := r.QueryUsage(ctx, UsageQuery{SandboxID: "gone"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || rows[0].SandboxID != "gone" {
		t.Fatalf("want only the destroyed sandbox's interval, got %+v", rows)
	}
}

// The closed row is what the host's billable counters are credited from, so a
// close has to report exactly what it closed — and report closing nothing.
func TestCloseUsageIntervalReturnsTheClosedRow(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	if _, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-1", 4, 2048, 0, nil); err != nil {
		t.Fatalf("open: %v", err)
	}

	closed, ok, err := r.CloseUsageInterval(ctx, "sb1", EndHibernate, 7_000_000)
	if err != nil || !ok {
		t.Fatalf("close: ok=%v err=%v", ok, err)
	}
	if closed.ID != "host-a:sb1:1" || closed.Vcpus != 4 || closed.MemMIB != 2048 {
		t.Fatalf("closed row is not the interval that was open: %+v", closed)
	}
	if closed.EndedAt == nil || closed.EndReason != EndHibernate || closed.CPUUsec != 7_000_000 {
		t.Fatalf("closed row missing the values the close wrote: %+v", closed)
	}

	// Every teardown path calls close and several can race, so a second close
	// is not an error — but it must not claim to have closed anything, or the
	// counters would be credited twice for one VM.
	again, ok, err := r.CloseUsageInterval(ctx, "sb1", EndDestroy, 9_000_000)
	if err != nil {
		t.Fatalf("second close: %v", err)
	}
	if ok || again.ID != "" {
		t.Fatalf("second close reported closing %+v", again)
	}
	if _, ok, err := r.CloseUsageInterval(ctx, "never-existed", EndDestroy, 1); err != nil || ok {
		t.Fatalf("closing an absent sandbox: ok=%v err=%v", ok, err)
	}
}

func assertTotalsEqual(t *testing.T, what string, a, b UsageTotals) {
	t.Helper()
	close := func(x, y float64) bool { return math.Abs(x-y) < 1e-6 }
	if a.Intervals != b.Intervals || a.OpenIntervals != b.OpenIntervals ||
		!close(a.DurationSeconds, b.DurationSeconds) || !close(a.VcpuSeconds, b.VcpuSeconds) ||
		!close(a.MemMIBSeconds, b.MemMIBSeconds) || !close(a.CPUSeconds, b.CPUSeconds) {
		t.Fatalf("%s disagree:\n  %+v\n  %+v", what, a, b)
	}
}

// A ready-pool VM is launched minutes before anyone claims it, so its cgroup
// leaf already holds the boot and idle CPU that produced it. Reporting that
// absolute counter made consumed CPU exceed what the interval could physically
// have used — 18.15 CPU-seconds for a 5-second interval on a 2-vCPU guest,
// measured on the fleet. Consumed CPU is therefore relative to a baseline
// taken when the interval opens.
func TestConsumedCPUExcludesPreClaimRuntime(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	// The VM burned 18s of CPU sitting in the ready pool before this claim.
	const poolCPU = 18_000_000
	if _, err := r.OpenUsageInterval(ctx, "claimed", "host-a", "vm-1", 2, 1024, poolCPU, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	// The customer then used 2s of CPU during a 5s interval.
	if err := r.TouchUsageInterval(ctx, "claimed", poolCPU+1_000_000); err != nil {
		t.Fatalf("touch: %v", err)
	}
	closed, ok, err := r.CloseUsageInterval(ctx, "claimed", EndDestroy, poolCPU+2_000_000)
	if err != nil || !ok {
		t.Fatalf("close: ok=%v err=%v", ok, err)
	}
	if closed.CPUUsec != 2_000_000 {
		t.Fatalf("cpu_usec=%d, want 2000000 — the pool's CPU leaked into the customer's interval", closed.CPUUsec)
	}
	if closed.CPUUsecBase != poolCPU {
		t.Fatalf("baseline not recorded (%d); a row must be auditable back to both readings", closed.CPUUsecBase)
	}

	// The ceiling that made this visible: consumed CPU cannot exceed
	// vcpus x duration. Duration here is sub-second, so any inherited CPU at
	// all would break it.
	if closed.CPUSeconds() > float64(closed.Vcpus)*closed.Duration().Seconds()+2 {
		t.Fatalf("consumed %0.2f CPU-s exceeds the %d-vCPU ceiling for a %s interval",
			closed.CPUSeconds(), closed.Vcpus, closed.Duration())
	}
}

// An unreadable cgroup must not be recorded as a baseline of zero-with-meaning
// or as a negative one: it degrades to the old absolute reading rather than
// inventing a correction.
func TestUnreadableBaselineDegradesToAbsolute(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	if _, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-1", 2, 1024, -1, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	closed, _, err := r.CloseUsageInterval(ctx, "sb1", EndDestroy, 5_000_000)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.CPUUsecBase != 0 || closed.CPUUsec != 5_000_000 {
		t.Fatalf("base=%d cpu=%d, want 0 and 5000000", closed.CPUUsecBase, closed.CPUUsec)
	}
}

// A sample below the baseline (a leaf that was recreated, a counter that
// cannot be trusted) must floor at zero rather than wrap into a negative bill.
func TestConsumedCPUNeverGoesNegative(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	if _, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-1", 2, 1024, 9_000_000, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	closed, _, err := r.CloseUsageInterval(ctx, "sb1", EndDestroy, 1_000_000)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.CPUUsec != 0 {
		t.Fatalf("cpu_usec=%d, want 0", closed.CPUUsec)
	}
}
