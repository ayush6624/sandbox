package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Stress and edge-case coverage for the billable ledger.
//
// The functional tests next door prove one caller at a time gets the right
// answer. Production does not look like that: teardown paths race each other for
// the same sandbox, the sampler walks the table while creates and freezes write
// it, and the spool reads it concurrently with all of the above. Money is the
// output, so the properties that matter here are invariants rather than
// examples — never two open intervals for one sandbox, never a gap or a repeat
// in a sandbox's seq, never a duration that exceeds the wall clock it was
// measured over, and totals that do not depend on who else was writing.

// ledgerInvariants is the assertion every stress test ends with: whatever the
// concurrency did, these must hold or an invoice is wrong.
func ledgerInvariants(t *testing.T, r *Registry, wallStart time.Time) {
	t.Helper()
	ctx := context.Background()
	rows, err := r.queryUsage(ctx, `ORDER BY sandbox_id, seq`)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}

	seenID := map[string]bool{}
	openPerSandbox := map[string]int{}
	seqPerSandbox := map[string][]int64{}
	wall := time.Since(wallStart) + 2*time.Second // +1s for second-truncation, +1s slack

	for _, iv := range rows {
		if seenID[iv.ID] {
			t.Fatalf("duplicate interval id %q: a consumer deduping on it would drop real usage", iv.ID)
		}
		seenID[iv.ID] = true
		if iv.EndedAt == nil {
			openPerSandbox[iv.SandboxID]++
		}
		seqPerSandbox[iv.SandboxID] = append(seqPerSandbox[iv.SandboxID], iv.Seq)
		if iv.Duration() < 0 {
			t.Fatalf("negative duration on %s: %+v", iv.ID, iv)
		}
		if iv.Duration() > wall {
			t.Fatalf("interval %s billed %s but the test has only run for %s", iv.ID, iv.Duration(), wall)
		}
		if iv.Vcpus <= 0 || iv.MemMIB <= 0 {
			t.Fatalf("interval %s has unbillable resources %+v", iv.ID, iv)
		}
	}
	for id, n := range openPerSandbox {
		if n > 1 {
			t.Fatalf("sandbox %s has %d open intervals: that is a double charge", id, n)
		}
	}
	for id, seqs := range seqPerSandbox {
		for i, seq := range seqs {
			if seq != int64(i+1) {
				t.Fatalf("sandbox %s has non-contiguous seq %v (a gap means a lost interval, a repeat means a double bill)", id, seqs)
			}
		}
	}

	// The SQL aggregate is what the API reports; the Go accessors are what the
	// tests reason about. A divergence under concurrency means the money the
	// customer sees depends on which code path answered.
	totals, err := r.UsageTotalsFor(ctx, UsageQuery{})
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	inMemory := SumUsage(rows)
	if totals.Intervals != inMemory.Intervals || totals.OpenIntervals != inMemory.OpenIntervals {
		t.Fatalf("counts diverge: sql=%+v go=%+v", totals, inMemory)
	}
	for _, d := range []struct {
		name     string
		sql, mem float64
	}{
		{"duration", totals.DurationSeconds, inMemory.DurationSeconds},
		{"vcpu", totals.VcpuSeconds, inMemory.VcpuSeconds},
		{"mem", totals.MemMIBSeconds, inMemory.MemMIBSeconds},
		{"cpu", totals.CPUSeconds, inMemory.CPUSeconds},
	} {
		if diff := d.sql - d.mem; diff > 1e-6 || diff < -1e-6 {
			t.Fatalf("%s totals diverge: sql=%f go=%f", d.name, d.sql, d.mem)
		}
	}
}

// Every teardown path calls CloseUsageInterval, and several of them can fire for
// one sandbox at once: an explicit destroy while the TTL reaper fires while the
// VMM dies on its own. Exactly one of them may be told it closed the interval —
// the caller that hears "ok" is the one that credits the host's billable
// counters, so a second winner is a duplicated charge in the metrics.
func TestConcurrentClosesElectExactlyOneWinner(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	const sandboxes = 40
	const closers = 8

	for i := 0; i < sandboxes; i++ {
		if _, err := r.OpenUsageInterval(ctx, fmt.Sprintf("sb-%d", i), "host-a", "vm", 2, 1024, 0, nil); err != nil {
			t.Fatalf("open: %v", err)
		}
	}

	reasons := []string{EndDestroy, EndExpire, EndHibernate, EndVMExit}
	var winners atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < sandboxes; i++ {
		for c := 0; c < closers; c++ {
			wg.Add(1)
			go func(i, c int) {
				defer wg.Done()
				<-start
				_, ok, err := r.CloseUsageInterval(ctx, fmt.Sprintf("sb-%d", i), reasons[c%len(reasons)], int64(c)*1_000_000)
				if err != nil {
					t.Errorf("close: %v", err)
					return
				}
				if ok {
					winners.Add(1)
				}
			}(i, c)
		}
	}
	close(start)
	wg.Wait()

	if got := winners.Load(); got != sandboxes {
		t.Fatalf("%d closers reported closing an interval, want exactly %d (one per sandbox)", got, sandboxes)
	}
	ledgerInvariants(t, r, time.Now())
}

// A create that races a destroy for the same id, repeatedly. The ledger must
// never end up with two open intervals, and an open that loses to an existing
// one must say so with ErrUsageOpen rather than silently succeeding or
// corrupting the seq sequence.
func TestConcurrentOpenAndCloseKeepAtMostOneIntervalOpen(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	wallStart := time.Now()

	var wg sync.WaitGroup
	var opens, alreadyOpen atomic.Int64
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for round := 0; round < 40; round++ {
				id := fmt.Sprintf("sb-%d", round%4)
				if _, err := r.OpenUsageInterval(ctx, id, "host-a", "vm", 2, 1024, 0, nil); err != nil {
					if errors.Is(err, ErrUsageOpen) {
						alreadyOpen.Add(1)
					} else {
						t.Errorf("open: %v", err)
						return
					}
				} else {
					opens.Add(1)
				}
				if _, _, err := r.CloseUsageInterval(ctx, id, EndDestroy, -1); err != nil {
					t.Errorf("close: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	if opens.Load() == 0 || alreadyOpen.Load() == 0 {
		t.Fatalf("test did not exercise the contended path: opens=%d refused=%d", opens.Load(), alreadyOpen.Load())
	}
	ledgerInvariants(t, r, wallStart)
}

// Production churn: many sandboxes each cycling through hibernate/wake while
// the sampler heartbeats open intervals, readers page the ledger, and the spool
// drains closed rows. Everything runs at once because that is the only
// configuration in which the ledger's indexes, transactions and read handle are
// actually under contention.
func TestLedgerSurvivesConcurrentChurnSamplingAndSpooling(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	wallStart := time.Now()

	const sandboxes = 32
	const cycles = 6

	done := make(chan struct{})
	var background sync.WaitGroup

	// Sampler: heartbeat every open interval, as usageSampler does.
	background.Add(1)
	go func() {
		defer background.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			open, err := r.OpenUsageIntervals(ctx)
			if err != nil {
				t.Errorf("list open: %v", err)
				return
			}
			for _, iv := range open {
				if err := r.TouchUsageInterval(ctx, iv.SandboxID, iv.CPUUsec+500_000); err != nil {
					t.Errorf("touch: %v", err)
					return
				}
			}
		}
	}()

	// Spool: drain closed intervals and mark them durable, like usageSpooler.
	var flushed atomic.Int64
	background.Add(1)
	go func() {
		defer background.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			batch, err := r.UnflushedUsageIntervals(ctx, 16)
			if err != nil {
				t.Errorf("unflushed: %v", err)
				return
			}
			ids := make([]string, 0, len(batch))
			for _, iv := range batch {
				if iv.EndedAt == nil {
					t.Errorf("spool offered an OPEN interval %s: it is not a fact yet", iv.ID)
					return
				}
				ids = append(ids, iv.ID)
			}
			if err := r.MarkUsageFlushed(ctx, ids); err != nil {
				t.Errorf("mark flushed: %v", err)
				return
			}
			flushed.Add(int64(len(ids)))
		}
	}()

	// Readers: the API's two read shapes, running against a moving table.
	background.Add(1)
	go func() {
		defer background.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, _, err := r.QueryUsage(ctx, UsageQuery{Limit: 25}); err != nil {
				t.Errorf("query: %v", err)
				return
			}
			if _, err := r.UsageTotalsFor(ctx, UsageQuery{}); err != nil {
				t.Errorf("totals: %v", err)
				return
			}
		}
	}()

	// Churn: each sandbox opens and closes `cycles` intervals, as a
	// create + N hibernate/wake pairs + a destroy would.
	var churn sync.WaitGroup
	for i := 0; i < sandboxes; i++ {
		churn.Add(1)
		go func(i int) {
			defer churn.Done()
			id := fmt.Sprintf("sb-%03d", i)
			for c := 0; c < cycles; c++ {
				if _, err := r.OpenUsageInterval(ctx, id, "host-a", fmt.Sprintf("vm-%d-%d", i, c), 2, 1024, int64(c)*1_000_000, map[string]string{"team": "core"}); err != nil {
					t.Errorf("open %s: %v", id, err)
					return
				}
				reason := EndHibernate
				if c == cycles-1 {
					reason = EndDestroy
				}
				if _, _, err := r.CloseUsageInterval(ctx, id, reason, int64(c+1)*2_000_000); err != nil {
					t.Errorf("close %s: %v", id, err)
					return
				}
			}
		}(i)
	}
	churn.Wait()
	close(done)
	background.Wait()

	rows, err := r.UsageForSandbox(ctx, "sb-000")
	if err != nil {
		t.Fatalf("read one sandbox: %v", err)
	}
	if len(rows) != cycles {
		t.Fatalf("sandbox sb-000 has %d intervals, want %d (a hibernate/wake pair bills two)", len(rows), cycles)
	}
	totals, err := r.UsageTotalsFor(ctx, UsageQuery{})
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.Intervals != sandboxes*cycles {
		t.Fatalf("ledger has %d intervals, want %d", totals.Intervals, sandboxes*cycles)
	}
	if totals.OpenIntervals != 0 {
		t.Fatalf("%d intervals still open after every sandbox was destroyed", totals.OpenIntervals)
	}
	ledgerInvariants(t, r, wallStart)
}

// The sampler must never resurrect a closed interval. A Touch that landed after
// the close would move last_seen_at past ended_at and bill time the VM did not
// run — the one direction of error a customer notices.
func TestTouchAfterCloseCannotExtendABill(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	if _, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-1", 2, 1024, 0, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	closed, _, err := r.CloseUsageInterval(ctx, "sb1", EndDestroy, 1_000_000)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := r.TouchUsageInterval(ctx, "sb1", 900_000_000); err != nil {
			t.Fatalf("touch: %v", err)
		}
	}

	after, err := r.UsageForSandbox(ctx, "sb1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("want 1 interval, got %d", len(after))
	}
	if !after[0].LastSeenAt.Equal(closed.LastSeenAt) || after[0].CPUUsec != closed.CPUUsec {
		t.Fatalf("a post-close touch mutated a closed bill: before=%+v after=%+v", closed, after[0])
	}
}

// A host that dies mid-churn: every open interval must be truncated to its last
// heartbeat, never to the moment recovery ran. Billing an outage is the failure
// this whole last_seen_at mechanism exists to prevent.
func TestCrashRecoveryUnderChurnNeverBillsPastLastHeartbeat(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	const sandboxes = 25
	for i := 0; i < sandboxes; i++ {
		id := fmt.Sprintf("sb-%d", i)
		if _, err := r.OpenUsageInterval(ctx, id, "host-a", "vm", 2, 1024, 0, nil); err != nil {
			t.Fatalf("open: %v", err)
		}
		// Half the fleet was still heartbeating when the host died; the other
		// half had gone quiet an hour earlier.
		if i%2 == 0 {
			if _, err := r.db.ExecContext(ctx,
				`UPDATE usage_intervals SET started_at=?, last_seen_at=? WHERE sandbox_id=?`,
				time.Now().Add(-2*time.Hour).Unix(), time.Now().Add(-1*time.Hour).Unix(), id); err != nil {
				t.Fatalf("age interval: %v", err)
			}
		}
	}

	n, err := r.CloseAbandonedUsageIntervals(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != sandboxes {
		t.Fatalf("recovered %d intervals, want %d", n, sandboxes)
	}

	rows, err := r.queryUsage(ctx, `ORDER BY sandbox_id`)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, iv := range rows {
		if iv.EndedAt == nil {
			t.Fatalf("%s still open after recovery", iv.ID)
		}
		if !iv.EndedAt.Equal(iv.LastSeenAt) {
			t.Fatalf("%s closed at %s but last heartbeat was %s: the gap is unserved time", iv.ID, iv.EndedAt, iv.LastSeenAt)
		}
		if iv.EndReason != EndCrash {
			t.Fatalf("%s closed as %q, want %q so an auditor can see the host died", iv.ID, iv.EndReason, EndCrash)
		}
	}
	// The aged half must bill an hour, not the three hours to "now".
	totals, err := r.UsageTotalsFor(ctx, UsageQuery{})
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	aged := (sandboxes + 1) / 2 // i%2==0
	if want := float64(aged) * 3600; totals.DurationSeconds > want+5 {
		t.Fatalf("recovery billed %.0f seconds, want at most ~%.0f (the outage is not billable)", totals.DurationSeconds, want)
	}
}

// Reads must not depend on who is writing. Under a heavy write load the totals
// for a fixed, already-closed selection must stay byte-identical: a number that
// drifts with concurrent traffic is a number nobody can reconcile an invoice
// against.
func TestTotalsForAClosedSelectionAreStableUnderConcurrentWrites(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	// A settled ledger for one sandbox.
	for c := 0; c < 3; c++ {
		if _, err := r.OpenUsageInterval(ctx, "settled", "host-a", "vm", 4, 2048, 0, nil); err != nil {
			t.Fatalf("open: %v", err)
		}
		if _, _, err := r.CloseUsageInterval(ctx, "settled", EndHibernate, int64(c+1)*1_000_000); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	baseline, err := r.UsageTotalsFor(ctx, UsageQuery{SandboxID: "settled"})
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}

	done := make(chan struct{})
	var writers sync.WaitGroup
	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := 0; ; i++ {
				select {
				case <-done:
					return
				default:
				}
				id := fmt.Sprintf("noise-%d-%d", w, i)
				if _, err := r.OpenUsageInterval(ctx, id, "host-a", "vm", 2, 1024, 0, nil); err != nil {
					t.Errorf("noise open: %v", err)
					return
				}
				if _, _, err := r.CloseUsageInterval(ctx, id, EndDestroy, 1_000_000); err != nil {
					t.Errorf("noise close: %v", err)
					return
				}
			}
		}(w)
	}

	for i := 0; i < 200; i++ {
		got, err := r.UsageTotalsFor(ctx, UsageQuery{SandboxID: "settled"})
		if err != nil {
			close(done)
			writers.Wait()
			t.Fatalf("totals under load: %v", err)
		}
		if got != baseline {
			close(done)
			writers.Wait()
			t.Fatalf("totals for a settled sandbox changed under concurrent writes: %+v vs %+v", got, baseline)
		}
	}
	close(done)
	writers.Wait()
}

// Money must not depend on pagination. With more intervals than any page can
// hold, every limit must report the same totals and only the row count may
// change.
func TestTotalsAreIndependentOfPageSizeAtScale(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	const sandboxes = 250
	for i := 0; i < sandboxes; i++ {
		id := fmt.Sprintf("sb-%03d", i)
		if _, err := r.OpenUsageInterval(ctx, id, "host-a", "vm", int64(1+i%4), int64(512*(1+i%4)), 0, nil); err != nil {
			t.Fatalf("open: %v", err)
		}
		if _, _, err := r.CloseUsageInterval(ctx, id, EndDestroy, int64(i)*1_000); err != nil {
			t.Fatalf("close: %v", err)
		}
	}

	full, err := r.UsageTotalsFor(ctx, UsageQuery{})
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if full.Intervals != sandboxes {
		t.Fatalf("ledger has %d intervals, want %d", full.Intervals, sandboxes)
	}
	for _, limit := range []int{1, 7, 100, sandboxes, sandboxes * 2} {
		rows, truncated, err := r.QueryUsage(ctx, UsageQuery{Limit: limit})
		if err != nil {
			t.Fatalf("page limit=%d: %v", limit, err)
		}
		if want := min(limit, sandboxes); len(rows) != want {
			t.Fatalf("limit=%d returned %d rows, want %d", limit, len(rows), want)
		}
		if truncated != (limit < sandboxes) {
			t.Fatalf("limit=%d truncated=%v", limit, truncated)
		}
		totals, err := r.UsageTotalsFor(ctx, UsageQuery{Limit: limit})
		if err != nil {
			t.Fatalf("totals limit=%d: %v", limit, err)
		}
		if totals != full {
			t.Fatalf("limit=%d changed the amount owed: %+v vs %+v", limit, totals, full)
		}
	}
}

// The consumed-CPU counter is monotonic per interval and floored at the
// baseline. A wake reuses the sandbox id with a brand-new cgroup leaf whose
// counter starts near zero, so a sampler that subtracted a stale baseline would
// go negative — and a leaf that resets mid-interval must not erase what was
// already recorded.
func TestConsumedCPUIsMonotonicAcrossResetsAndOutOfOrderSamples(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	if _, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-1", 2, 1024, 5_000_000, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, sample := range []int64{6_000_000, 9_000_000, 1_000_000 /* leaf reset */, 4_000_000, -1 /* unreadable */} {
		if err := r.TouchUsageInterval(ctx, "sb1", sample); err != nil {
			t.Fatalf("touch %d: %v", sample, err)
		}
	}
	closed, _, err := r.CloseUsageInterval(ctx, "sb1", EndDestroy, 2_000_000)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.CPUUsec != 4_000_000 {
		t.Fatalf("cpu_usec = %d, want the high-water mark 4000000 (9000000 sampled - 5000000 baseline)", closed.CPUUsec)
	}
	if closed.CPUSeconds() < 0 {
		t.Fatalf("negative consumed CPU: %f", closed.CPUSeconds())
	}
}

// Windowed reads are the shape an invoice is built from, and they select by
// overlap. Under a mixed ledger the window must include everything that ran
// during it — including intervals that started before it and are still open —
// and nothing that ran only outside it.
func TestWindowedInvoiceSelectionUnderMixedTraffic(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	type row struct {
		id            string
		start, end    time.Time
		open          bool
		wantInJanuary bool
	}
	windowFrom := now.Add(-24 * time.Hour)
	windowTo := now.Add(-12 * time.Hour)
	rows := []row{
		{id: "before", start: now.Add(-48 * time.Hour), end: now.Add(-36 * time.Hour)},
		{id: "straddles-start", start: now.Add(-30 * time.Hour), end: now.Add(-20 * time.Hour), wantInJanuary: true},
		{id: "inside", start: now.Add(-20 * time.Hour), end: now.Add(-18 * time.Hour), wantInJanuary: true},
		{id: "straddles-end", start: now.Add(-14 * time.Hour), end: now.Add(-2 * time.Hour), wantInJanuary: true},
		{id: "after", start: now.Add(-6 * time.Hour), end: now.Add(-1 * time.Hour)},
		{id: "still-open-from-before", start: now.Add(-40 * time.Hour), open: true, wantInJanuary: true},
	}
	for _, want := range rows {
		if _, err := r.OpenUsageInterval(ctx, want.id, "host-a", "vm", 2, 1024, 0, nil); err != nil {
			t.Fatalf("open %s: %v", want.id, err)
		}
		if want.open {
			if _, err := r.db.ExecContext(ctx,
				`UPDATE usage_intervals SET started_at=?, last_seen_at=? WHERE sandbox_id=?`,
				want.start.Unix(), now.Unix(), want.id); err != nil {
				t.Fatalf("age open %s: %v", want.id, err)
			}
			continue
		}
		if _, _, err := r.CloseUsageInterval(ctx, want.id, EndDestroy, -1); err != nil {
			t.Fatalf("close %s: %v", want.id, err)
		}
		if _, err := r.db.ExecContext(ctx,
			`UPDATE usage_intervals SET started_at=?, ended_at=?, last_seen_at=? WHERE sandbox_id=?`,
			want.start.Unix(), want.end.Unix(), want.end.Unix(), want.id); err != nil {
			t.Fatalf("age %s: %v", want.id, err)
		}
	}

	got, _, err := r.QueryUsage(ctx, UsageQuery{From: windowFrom, To: windowTo})
	if err != nil {
		t.Fatalf("window query: %v", err)
	}
	selected := map[string]bool{}
	for _, iv := range got {
		selected[iv.SandboxID] = true
	}
	for _, want := range rows {
		if selected[want.id] != want.wantInJanuary {
			t.Fatalf("sandbox %s selected=%v, want %v for window [%s, %s)", want.id, selected[want.id], want.wantInJanuary, windowFrom, windowTo)
		}
	}

	// Totals must cover the same selection, whole — an interval is never
	// clipped to the window (cpu_usec cannot be apportioned).
	totals, err := r.UsageTotalsFor(ctx, UsageQuery{From: windowFrom, To: windowTo})
	if err != nil {
		t.Fatalf("window totals: %v", err)
	}
	if totals.Intervals != 4 {
		t.Fatalf("window selected %d intervals, want 4", totals.Intervals)
	}
	if totals.OpenIntervals != 1 {
		t.Fatalf("window reports %d open intervals, want 1", totals.OpenIntervals)
	}
}

// Metadata is the only attribution the ledger carries. It must survive the
// round trip intact at the sizes the v1 API accepts (64 entries, 1 KiB values),
// including keys that would break a naive encoding.
func TestMetadataRoundTripsAtAPILimits(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	meta := map[string]string{}
	for i := 0; i < 64; i++ {
		meta[fmt.Sprintf("key.with/odd\"chars-%d", i)] = strings.Repeat("v", 1024)
	}
	if _, err := r.OpenUsageInterval(ctx, "sb1", "host-a", "vm-1", 2, 1024, 0, meta); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := r.CloseUsageInterval(ctx, "sb1", EndDestroy, -1); err != nil {
		t.Fatalf("close: %v", err)
	}
	rows, err := r.UsageForSandbox(ctx, "sb1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows[0].Metadata) != len(meta) {
		t.Fatalf("metadata lost: got %d entries, want %d", len(rows[0].Metadata), len(meta))
	}
	for k, v := range meta {
		if rows[0].Metadata[k] != v {
			t.Fatalf("metadata entry %q did not round trip", k)
		}
	}
}

// A sandbox that lives and dies inside one second still produced a VM. It bills
// zero seconds (whole-second resolution), but it must produce a real, closed,
// spoolable row — dropping it would make short-lived sandboxes invisible to
// reconciliation rather than merely free.
func TestSubSecondSandboxesStillProduceALedgerRow(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	const n = 50
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("sb-%d", i)
		if _, err := r.OpenUsageInterval(ctx, id, "host-a", "vm", 2, 1024, 0, nil); err != nil {
			t.Fatalf("open: %v", err)
		}
		if _, _, err := r.CloseUsageInterval(ctx, id, EndDestroy, 250_000); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	totals, err := r.UsageTotalsFor(ctx, UsageQuery{})
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.Intervals != n {
		t.Fatalf("%d rows, want %d", totals.Intervals, n)
	}
	if totals.DurationSeconds < 0 {
		t.Fatalf("negative billed duration %f", totals.DurationSeconds)
	}
	pending, err := r.UnflushedUsageIntervals(ctx, 1000)
	if err != nil {
		t.Fatalf("unflushed: %v", err)
	}
	if len(pending) != n {
		t.Fatalf("%d rows queued for durability, want %d: a zero-duration interval is still evidence", len(pending), n)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// A sandbox that moves hosts continues its interval numbering instead of
// restarting it. The public API presents a line item as "<sandbox>:<sequence>"
// and promises that is unique, so a restart produces two rows claiming to be
// the same charge — and a consumer deduping on it drops a real interval, on
// exactly the path a host failure takes.
func TestAdoptedSandboxContinuesItsIntervalNumbering(t *testing.T) {
	ctx := context.Background()

	// Host A bills three intervals (a create plus two hibernate/wake cycles).
	hostA := testRegistry(t)
	for i := 0; i < 3; i++ {
		if _, err := hostA.OpenUsageInterval(ctx, "sb1", "host-a", "vm", 2, 1024, 0, nil); err != nil {
			t.Fatalf("open on host A: %v", err)
		}
		if _, _, err := hostA.CloseUsageInterval(ctx, "sb1", EndHibernate, -1); err != nil {
			t.Fatalf("close on host A: %v", err)
		}
	}
	carried, err := hostA.LastUsageSeq(ctx, "sb1")
	if err != nil {
		t.Fatalf("last seq: %v", err)
	}
	if carried != 3 {
		t.Fatalf("host A reports last sequence %d, want 3", carried)
	}

	// Host B adopts it, carrying that number in the durable record.
	hostB := testRegistry(t)
	if err := hostB.SetUsageSeqFloor(ctx, "sb1", carried); err != nil {
		t.Fatalf("set floor: %v", err)
	}
	iv, err := hostB.OpenUsageInterval(ctx, "sb1", "host-b", "vm", 2, 1024, 0, nil)
	if err != nil {
		t.Fatalf("open on host B: %v", err)
	}
	if iv.Seq != 4 {
		t.Fatalf("the adopted sandbox restarted at sequence %d, colliding with host A's line items", iv.Seq)
	}
	if iv.ID != "host-b:sb1:4" {
		t.Fatalf("ledger id = %q, want host-b:sb1:4", iv.ID)
	}
	if _, _, err := hostB.CloseUsageInterval(ctx, "sb1", EndDestroy, -1); err != nil {
		t.Fatalf("close on host B: %v", err)
	}
	next, err := hostB.OpenUsageInterval(ctx, "sb1", "host-b", "vm", 2, 1024, 0, nil)
	if err != nil {
		t.Fatalf("reopen on host B: %v", err)
	}
	if next.Seq != 5 {
		t.Fatalf("second interval on host B has sequence %d, want 5", next.Seq)
	}

	// A floor can only move forward: a stale record must not renumber a sandbox
	// backwards into ids it has already used.
	if err := hostB.SetUsageSeqFloor(ctx, "sb1", 1); err != nil {
		t.Fatalf("lower floor: %v", err)
	}
	if got, _ := hostB.LastUsageSeq(ctx, "sb1"); got != 5 {
		t.Fatalf("a stale floor rewound the sequence to %d", got)
	}
}

// A floor is bookkeeping for intervals that are still here. Once a sandbox's
// rows are pruned it protects nothing, and would otherwise accumulate one row
// per migrated sandbox forever.
func TestUsageSeqFloorIsPrunedWithTheIntervalsItProtects(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	if err := r.SetUsageSeqFloor(ctx, "sb1", 7); err != nil {
		t.Fatalf("set floor: %v", err)
	}
	if _, err := r.OpenUsageInterval(ctx, "sb1", "host-b", "vm", 2, 1024, 0, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := r.CloseUsageInterval(ctx, "sb1", EndDestroy, -1); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := r.MarkUsageFlushed(ctx, []string{"host-b:sb1:8"}); err != nil {
		t.Fatalf("mark flushed: %v", err)
	}

	// Still holding rows: the floor stays.
	if _, err := r.PruneUsageIntervals(ctx, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("prune (nothing due): %v", err)
	}
	if got, _ := r.LastUsageSeq(ctx, "sb1"); got != 8 {
		t.Fatalf("floor dropped while its intervals were still present (last seq %d)", got)
	}

	// Rows pruned: the floor goes with them.
	if _, err := r.PruneUsageIntervals(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got, _ := r.LastUsageSeq(ctx, "sb1"); got != 0 {
		t.Fatalf("floor survived the pruning of every interval it covered (last seq %d)", got)
	}
}
