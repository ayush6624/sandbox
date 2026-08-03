package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

// testMeteringServer gives the server a template so effectiveResources has
// defaults to resolve, which is the part the ledger must record.
func testMeteringServer(t *testing.T) *Server {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"), registry.Pools{
		TapPrefix:  "fc",
		TapMax:     4,
		GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.0.13",
		PortMin:    5200,
		PortMax:    5203,
	})
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })
	s := New(Config{HostID: "host-test"}, reg)
	s.cfg.VMTemplate.Vcpus = 2
	s.cfg.VMTemplate.MemMIB = 1024
	return s
}

// The registry stores 0 for "template default" and the sandbox row is deleted
// on destroy, so an interval that recorded 0 would be unbillable forever.
func TestMeterStartSnapshotsEffectiveResources(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()

	s.meterStart(ctx, registry.Sandbox{ID: "sb1", VMID: "vm-1"})
	s.meterStart(ctx, registry.Sandbox{ID: "sb2", VMID: "vm-2", Vcpus: 4, MemMIB: 4096})

	def, err := s.reg.UsageForSandbox(ctx, "sb1")
	if err != nil {
		t.Fatalf("read sb1: %v", err)
	}
	if len(def) != 1 {
		t.Fatalf("want 1 interval for sb1, got %d", len(def))
	}
	if def[0].Vcpus != 2 || def[0].MemMIB != 1024 {
		t.Fatalf("template defaults not resolved into the ledger: %+v", def[0])
	}

	over, _ := s.reg.UsageForSandbox(ctx, "sb2")
	if over[0].Vcpus != 4 || over[0].MemMIB != 4096 {
		t.Fatalf("per-sandbox override not recorded: %+v", over[0])
	}
}

// A ready-pool VM runs at our expense until someone claims it. Metering it
// earlier would invent phantom usage on every worker — at warm_pool_size 8,
// eight sandboxes' worth per host.
func TestWarmPoolVMIsNotMeteredBeforeClaim(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()

	warm, err := s.reg.CreateWarm(ctx, "warm1", "/tmp/warm1.ext4", "", 0, 0)
	if err != nil {
		t.Fatalf("create warm: %v", err)
	}
	if err := s.reg.MarkWarmReady(ctx, warm.ID); err != nil {
		t.Fatalf("mark warm ready: %v", err)
	}

	if n, _ := s.reg.CountOpenUsageIntervals(ctx); n != 0 {
		t.Fatalf("a ready pool VM opened %d billable intervals, want 0", n)
	}

	claimed, ok := s.claimWarm(ctx, "mine", nil, 0)
	if !ok {
		t.Fatal("claimWarm found no ready VM")
	}
	if n, _ := s.reg.CountOpenUsageIntervals(ctx); n != 1 {
		t.Fatalf("claim opened %d intervals, want 1", n)
	}
	got, _ := s.reg.UsageForSandbox(ctx, claimed.ID)
	if len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("claim must open seq 1: %+v", got)
	}
	// The bill starts at the claim, not at the pool build — ClaimWarm resets
	// created_at to the same moment, so the two must agree.
	if got[0].StartedAt.Before(claimed.CreatedAt.Add(-2 * time.Second)) {
		t.Fatalf("interval started %v, before the claim at %v", got[0].StartedAt, claimed.CreatedAt)
	}
}

// A bring-up that never reaches MarkRunning bills nothing. This is also why a
// gateway create that fails over to another host cannot double-bill.
func TestFailedBringUpLeavesNoInterval(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()

	if _, err := s.reg.CreateStarting(ctx, "sb1", "", "/tmp/sb1.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create starting: %v", err)
	}
	// Teardown of a never-published sandbox: the meter close runs, and must be
	// a no-op rather than materializing an interval.
	s.meterStop(ctx, "sb1", registry.EndDestroy)

	if got, _ := s.reg.UsageForSandbox(ctx, "sb1"); len(got) != 0 {
		t.Fatalf("a failed bring-up produced %d intervals: %+v", len(got), got)
	}
}

// An open interval whose VM is gone cannot be accruing. If the sampler merely
// extended it, a missed close would keep growing a customer's invoice forever —
// the one failure mode of this design that silently costs real money.
func TestSamplerClosesIntervalWithNoLiveVM(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()

	s.meterStart(ctx, registry.Sandbox{ID: "sb1", VMID: "vm-1"})
	if n, _ := s.reg.CountOpenUsageIntervals(ctx); n != 1 {
		t.Fatal("setup: interval should be open")
	}

	// No entry in s.machines — the VMM is gone.
	s.sampleOpenUsage(ctx)

	if n, _ := s.reg.CountOpenUsageIntervals(ctx); n != 0 {
		t.Fatalf("%d intervals still open with no live VM: billing would run away", n)
	}
	got, _ := s.reg.UsageForSandbox(ctx, "sb1")
	if got[0].EndReason != registry.EndVMExit {
		t.Fatalf("end_reason = %q, want %q", got[0].EndReason, registry.EndVMExit)
	}
}

// A live VM's interval must survive sampling: the sampler advances it, and an
// unreadable CPU counter (no cgroup on a dev host) must not close it.
func TestSamplerKeepsLiveIntervalOpen(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()

	s.meterStart(ctx, registry.Sandbox{ID: "sb1", VMID: "vm-1"})
	// A placeholder machine: SampleUsage will fail on it (no cgroup leaf, no
	// process), which is exactly the degraded case worth pinning.
	s.machines.Store("sb1", &vm.Machine{})

	s.sampleOpenUsage(ctx)

	if n, _ := s.reg.CountOpenUsageIntervals(ctx); n != 1 {
		t.Fatalf("a live sandbox's interval was closed by sampling: %d open", n)
	}
}

// A sample that cannot be read must not zero a real bill.
func TestSampleCPUUsecReturnsNegativeWhenUnavailable(t *testing.T) {
	s := testMeteringServer(t)

	if got := s.sampleCPUUsec("absent"); got != -1 {
		t.Fatalf("sampleCPUUsec with no machine = %d, want -1 (keep last known)", got)
	}
	s.machines.Store("sb1", &vm.Machine{})
	if got := s.sampleCPUUsec("sb1"); got != -1 {
		t.Fatalf("sampleCPUUsec with an unreadable machine = %d, want -1", got)
	}
}

// During a drain, a freeze is a platform event. Recording it as an ordinary
// idle hibernation would make a fleet roll look like every customer's sandbox
// going idle at the same instant.
func TestShutdownOverridesEndReason(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()

	s.meterStart(ctx, registry.Sandbox{ID: "sb1", VMID: "vm-1"})
	s.shuttingDown.Store(true)
	s.meterStop(ctx, "sb1", registry.EndHibernate)

	got, _ := s.reg.UsageForSandbox(ctx, "sb1")
	if got[0].EndReason != registry.EndShutdown {
		t.Fatalf("end_reason = %q, want %q", got[0].EndReason, registry.EndShutdown)
	}
}

// Metering must never fail a lifecycle operation, so a duplicate open is
// logged and swallowed rather than propagated.
func TestMeterStartDoubleOpenDoesNotPanicOrDuplicate(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()

	s.meterStart(ctx, registry.Sandbox{ID: "sb1", VMID: "vm-1"})
	s.meterStart(ctx, registry.Sandbox{ID: "sb1", VMID: "vm-2"})

	got, _ := s.reg.UsageForSandbox(ctx, "sb1")
	if len(got) != 1 {
		t.Fatalf("double open produced %d intervals, want 1", len(got))
	}
	if got[0].VMID != "vm-1" {
		t.Fatalf("the second open overwrote the first: vm_id = %q", got[0].VMID)
	}
}

// reconcile's contract: intervals open at startup were abandoned by a dead
// server, and are truncated to their last heartbeat rather than to now.
func TestRecoverUsageIntervalsClosesAbandoned(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()

	s.meterStart(ctx, registry.Sandbox{ID: "sb1", VMID: "vm-1"})
	s.recoverUsageIntervals(ctx)

	got, _ := s.reg.UsageForSandbox(ctx, "sb1")
	if got[0].EndedAt == nil {
		t.Fatal("interval left open across a restart")
	}
	if got[0].EndReason != registry.EndCrash {
		t.Fatalf("end_reason = %q, want %q", got[0].EndReason, registry.EndCrash)
	}
	if !got[0].EndedAt.Equal(got[0].LastSeenAt) {
		t.Fatalf("ended_at %v must equal last_seen_at %v", got[0].EndedAt, got[0].LastSeenAt)
	}
}
