package registry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUsageIntervalOpenCloseRoundTrip(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	u, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-1", 2, 1024, map[string]string{"team": "core"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if u.Seq != 1 {
		t.Fatalf("first interval must be seq 1, got %d", u.Seq)
	}
	if u.ID != "host-a:sb1:1" {
		t.Fatalf("id must be deterministic for at-least-once dedup, got %q", u.ID)
	}

	if _, _, err := r.CloseUsageInterval(ctx, "sb1", EndDestroy, 3_500_000); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := r.UsageForSandbox(ctx, "sb1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 interval, got %d", len(got))
	}
	iv := got[0]
	if iv.EndedAt == nil {
		t.Fatal("closed interval must have ended_at")
	}
	if iv.EndReason != EndDestroy {
		t.Fatalf("end_reason = %q, want %q", iv.EndReason, EndDestroy)
	}
	if iv.CPUUsec != 3_500_000 {
		t.Fatalf("cpu_usec = %d, want 3500000", iv.CPUUsec)
	}
	if iv.CPUSeconds() != 3.5 {
		t.Fatalf("cpu_seconds = %v, want 3.5", iv.CPUSeconds())
	}
	if iv.Vcpus != 2 || iv.MemMIB != 1024 {
		t.Fatalf("effective resources not snapshotted: %+v", iv)
	}
	if iv.Metadata["team"] != "core" {
		t.Fatalf("metadata snapshot lost: %+v", iv.Metadata)
	}
}

// The ledger must outlive the sandbox: Destroy deletes the sandboxes row, and
// a ledger that vanished with it would lose the usage of every sandbox that
// ever terminated.
func TestUsageIntervalsSurviveSandboxDestroy(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	if _, err := r.Create(ctx, "sb1", "", "/tmp/sb1.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-1", 2, 1024, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := r.CloseUsageInterval(ctx, "sb1", EndDestroy, 1_000_000); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := r.Destroy(ctx, "sb1"); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	got, err := r.UsageForSandbox(ctx, "sb1")
	if err != nil {
		t.Fatalf("read after destroy: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("usage must survive destroy, got %d intervals", len(got))
	}
}

// A hibernate/wake cycle is two VMMs, so it is two intervals — and the frozen
// span between them is billed to nobody.
func TestUsageHibernateWakeOpensNextSeq(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	if _, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-1", 2, 1024, nil); err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if _, _, err := r.CloseUsageInterval(ctx, "sb1", EndHibernate, 500_000); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	second, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-2", 2, 1024, nil)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	if second.Seq != 2 {
		t.Fatalf("wake must open seq 2, got %d", second.Seq)
	}
	if second.ID == "host-a:sb1:1" {
		t.Fatal("second interval reused the first interval's id")
	}

	got, err := r.UsageForSandbox(ctx, "sb1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 intervals, got %d", len(got))
	}
	if got[0].EndReason != EndHibernate {
		t.Fatalf("first interval end_reason = %q", got[0].EndReason)
	}
	if got[1].EndedAt != nil {
		t.Fatal("second interval should still be open")
	}
}

// Two open intervals for one sandbox would charge one VM twice, so the ledger
// refuses it at the database rather than letting it surface on an invoice.
func TestUsageDoubleOpenRefused(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	if _, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-1", 2, 1024, nil); err != nil {
		t.Fatalf("open 1: %v", err)
	}
	_, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-2", 2, 1024, nil)
	if !errors.Is(err, ErrUsageOpen) {
		t.Fatalf("second open must fail with ErrUsageOpen, got %v", err)
	}

	open, err := r.OpenUsageIntervals(ctx)
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("want exactly 1 open interval, got %d", len(open))
	}
}

// Every teardown path closes intervals and several can race for one sandbox;
// a duplicate close must not turn into a failed destroy.
func TestUsageCloseIsIdempotentAndTolerantOfAbsence(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	if _, _, err := r.CloseUsageInterval(ctx, "never-existed", EndDestroy, 100); err != nil {
		t.Fatalf("closing an absent interval must not error: %v", err)
	}

	if _, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-1", 2, 1024, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := r.CloseUsageInterval(ctx, "sb1", EndDestroy, 2_000_000); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if _, _, err := r.CloseUsageInterval(ctx, "sb1", EndVMExit, 9_999_999); err != nil {
		t.Fatalf("close 2: %v", err)
	}

	got, _ := r.UsageForSandbox(ctx, "sb1")
	if len(got) != 1 {
		t.Fatalf("want 1 interval, got %d", len(got))
	}
	if got[0].EndReason != EndDestroy {
		t.Fatalf("a second close overwrote the first: end_reason = %q", got[0].EndReason)
	}
	if got[0].CPUUsec != 2_000_000 {
		t.Fatalf("a second close overwrote the CPU total: %d", got[0].CPUUsec)
	}
}

// The crash rule: an interval left open by a dead server is closed at
// last_seen_at, never at now. Billing the span a host spent DOWN is the one
// error here a customer would certainly notice.
func TestUsageCrashRecoveryTruncatesAtLastSeen(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	if _, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-1", 2, 1024, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	// Backdate: the sandbox started an hour ago and was last sampled 50 minutes
	// ago, i.e. the host died ~50 minutes ago.
	started := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	lastSeen := started.Add(10 * time.Minute)
	if _, err := r.db.Exec(`UPDATE usage_intervals SET started_at=?, last_seen_at=? WHERE sandbox_id=?`,
		started.Unix(), lastSeen.Unix(), "sb1"); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := r.CloseAbandonedUsageIntervals(ctx)
	if err != nil {
		t.Fatalf("close abandoned: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 interval closed, got %d", n)
	}

	got, _ := r.UsageForSandbox(ctx, "sb1")
	iv := got[0]
	if iv.EndReason != EndCrash {
		t.Fatalf("end_reason = %q, want %q", iv.EndReason, EndCrash)
	}
	if !iv.EndedAt.Equal(lastSeen) {
		t.Fatalf("ended_at = %v, want last_seen_at %v (billing the downtime)", iv.EndedAt, lastSeen)
	}
	if got := iv.Duration(); got != 10*time.Minute {
		t.Fatalf("duration = %v, want 10m: the ~50m the host was down must not bill", got)
	}
	if got := iv.VcpuSeconds(); got != 1200 {
		t.Fatalf("vcpu_seconds = %v, want 1200 (2 vcpu × 600s)", got)
	}
	if got := iv.MemMIBSeconds(); got != 614400 {
		t.Fatalf("mem_mib_seconds = %v, want 614400 (1024 MiB × 600s)", got)
	}
}

// An OPEN interval is measured to its heartbeat, not to wall-clock now, so the
// same rule that protects a crashed host also holds for live reads.
func TestUsageOpenIntervalMeasuredToLastSeen(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	if _, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-1", 1, 512, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	started := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	if _, err := r.db.Exec(`UPDATE usage_intervals SET started_at=?, last_seen_at=? WHERE sandbox_id=?`,
		started.Unix(), started.Add(5*time.Minute).Unix(), "sb1"); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	open, _ := r.OpenUsageIntervals(ctx)
	if len(open) != 1 {
		t.Fatalf("want 1 open interval, got %d", len(open))
	}
	if got := open[0].Duration(); got != 5*time.Minute {
		t.Fatalf("open duration = %v, want 5m (to last_seen_at)", got)
	}
}

func TestUsageTouchAdvancesHeartbeatAndCPU(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	if _, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-1", 2, 1024, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := r.TouchUsageInterval(ctx, "sb1", 1_000_000); err != nil {
		t.Fatalf("touch: %v", err)
	}
	// cpu.stat is monotonic per leaf, so a lower reading is noise (a stale
	// sample racing a fresher one) and must not walk the total backwards.
	if err := r.TouchUsageInterval(ctx, "sb1", 400_000); err != nil {
		t.Fatalf("touch 2: %v", err)
	}

	open, _ := r.OpenUsageIntervals(ctx)
	if open[0].CPUUsec != 1_000_000 {
		t.Fatalf("cpu_usec = %d, want 1000000 (must not regress)", open[0].CPUUsec)
	}
}

// A missing sample passes -1: keep whatever the sampler last recorded rather
// than zeroing a real bill because one cgroup read failed at teardown.
func TestUsageCloseWithoutSampleKeepsSampledCPU(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	if _, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-1", 2, 1024, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := r.TouchUsageInterval(ctx, "sb1", 7_000_000); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if _, _, err := r.CloseUsageInterval(ctx, "sb1", EndVMExit, -1); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, _ := r.UsageForSandbox(ctx, "sb1")
	if got[0].CPUUsec != 7_000_000 {
		t.Fatalf("cpu_usec = %d, want the last sampled 7000000", got[0].CPUUsec)
	}
}

// A sub-second create/destroy must not produce a negative duration.
func TestUsageFastCycleHasNonNegativeDuration(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	if _, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-1", 2, 1024, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := r.CloseUsageInterval(ctx, "sb1", EndDestroy, 0); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, _ := r.UsageForSandbox(ctx, "sb1")
	if d := got[0].Duration(); d < 0 {
		t.Fatalf("duration = %v, must never be negative", d)
	}
}

func TestUsageFlushLifecycle(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	// Open intervals are not facts yet, so they are never spooled.
	if _, err := r.OpenUsageInterval(ctx, "open-one", "host-a", "vm-1", 2, 1024, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := r.OpenUsageInterval(ctx, "closed-one", "host-a", "vm-2", 2, 1024, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := r.CloseUsageInterval(ctx, "closed-one", EndDestroy, 1_000); err != nil {
		t.Fatalf("close: %v", err)
	}

	pending, err := r.UnflushedUsageIntervals(ctx, 100)
	if err != nil {
		t.Fatalf("unflushed: %v", err)
	}
	if len(pending) != 1 || pending[0].SandboxID != "closed-one" {
		t.Fatalf("only closed intervals are spoolable, got %+v", pending)
	}

	if n, _ := r.CountUnflushedUsageIntervals(ctx); n != 1 {
		t.Fatalf("unflushed count = %d, want 1", n)
	}
	if n, _ := r.CountOpenUsageIntervals(ctx); n != 1 {
		t.Fatalf("open count = %d, want 1", n)
	}

	if err := r.MarkUsageFlushed(ctx, []string{pending[0].ID}); err != nil {
		t.Fatalf("mark flushed: %v", err)
	}
	if again, _ := r.UnflushedUsageIntervals(ctx, 100); len(again) != 0 {
		t.Fatalf("flushed interval still pending: %+v", again)
	}
}

// Pruning is bounded by durability: an interval that never reached the bucket
// must not be deleted, however old it is.
func TestUsagePruneOnlyDropsFlushedIntervals(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	for _, id := range []string{"flushed", "unflushed"} {
		if _, err := r.OpenUsageInterval(ctx, id, "host-a", "vm-"+id, 2, 1024, nil); err != nil {
			t.Fatalf("open %s: %v", id, err)
		}
		if _, _, err := r.CloseUsageInterval(ctx, id, EndDestroy, 1_000); err != nil {
			t.Fatalf("close %s: %v", id, err)
		}
	}
	if err := r.MarkUsageFlushed(ctx, []string{"host-a:flushed:1"}); err != nil {
		t.Fatalf("mark flushed: %v", err)
	}
	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Unix()
	if _, err := r.db.Exec(`UPDATE usage_intervals SET ended_at=?`, old); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := r.PruneUsageIntervals(ctx, time.Now().UTC().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d intervals, want 1 (only the flushed one)", n)
	}
	left, _ := r.UsageForSandbox(ctx, "unflushed")
	if len(left) != 1 {
		t.Fatal("prune deleted an interval that never reached durable storage")
	}
}

func TestUsageBetweenIncludesOverlappingIntervals(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	// before: fully in the past. spanning: started earlier, still open.
	// inside: entirely within the window.
	for _, id := range []string{"before", "spanning", "inside"} {
		if _, err := r.OpenUsageInterval(ctx, id, "host-a", "vm-"+id, 1, 512, nil); err != nil {
			t.Fatalf("open %s: %v", id, err)
		}
	}
	if _, _, err := r.CloseUsageInterval(ctx, "before", EndDestroy, 0); err != nil {
		t.Fatalf("close before: %v", err)
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := r.db.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	mustExec(`UPDATE usage_intervals SET started_at=?, ended_at=?, last_seen_at=? WHERE sandbox_id='before'`,
		now.Add(-4*time.Hour).Unix(), now.Add(-3*time.Hour).Unix(), now.Add(-3*time.Hour).Unix())
	mustExec(`UPDATE usage_intervals SET started_at=?, last_seen_at=? WHERE sandbox_id='spanning'`,
		now.Add(-3*time.Hour).Unix(), now.Unix())
	mustExec(`UPDATE usage_intervals SET started_at=?, last_seen_at=? WHERE sandbox_id='inside'`,
		now.Add(-30*time.Minute).Unix(), now.Unix())

	got, err := r.UsageBetween(ctx, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("usage between: %v", err)
	}
	seen := map[string]bool{}
	for _, iv := range got {
		seen[iv.SandboxID] = true
	}
	if seen["before"] {
		t.Fatal("an interval that ended before the window must not be billed in it")
	}
	if !seen["spanning"] {
		t.Fatal("an interval still open across the window must be billed in it")
	}
	if !seen["inside"] {
		t.Fatal("an interval inside the window is missing")
	}
}
