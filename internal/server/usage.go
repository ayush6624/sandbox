package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

// usageSampleInterval bounds how much of a bill a host crash can lose: an
// interval left open by a dead server is truncated to its last heartbeat, so a
// shorter tick costs a little SQLite traffic and buys accuracy.
const usageSampleInterval = 60 * time.Second

// Billable metering. One interval per VMM lifetime — see
// docs/usage-metering-plan.md and registry.UsageInterval.
//
// Every function here is best-effort by construction: metering must never fail
// a create, a freeze, or a destroy. A lost sample degrades the bill; a create
// that 500s because the ledger hiccuped is an outage.

// meterStart opens a billable interval for a sandbox that has just become
// usable. Callers invoke it AFTER the readiness gates (MarkRunning, a warm
// claim, or a wake) — never at create acceptance, because bring-up latency and
// pre-claim ready-pool runtime are the platform's cost, not the customer's.
//
// sb must carry the identity the sandbox is actually running with; effective
// resources are resolved here so the ledger never stores "0 = template default"
// for a row whose template it can no longer look up.
func (s *Server) meterStart(ctx context.Context, sb registry.Sandbox) {
	ctx, cancel := meterCtx(ctx)
	defer cancel()

	eff := s.effectiveResources(sb)
	// Baseline the cgroup counter at open. On the default create path the VM
	// has been running in the ready pool since long before this claim, so its
	// leaf already holds ~18 CPU-seconds of boot and idle that belong to us —
	// reported raw, that made consumed CPU exceed the interval's own physical
	// ceiling. -1 (unreadable) records no baseline rather than a wrong one.
	_, err := s.reg.OpenUsageInterval(ctx, sb.ID, s.hostID(), sb.VMID, eff.Vcpus, eff.MemMIB, s.sampleCPUUsec(sb.ID), sb.Metadata)
	switch {
	case err == nil:
	case errors.Is(err, registry.ErrUsageOpen):
		// Two open intervals would charge one VM twice. The ledger's partial
		// unique index refuses it, so this is a missed close somewhere — a bug
		// worth shouting about, but not one worth failing the sandbox for.
		fmt.Fprintf(os.Stderr, "[%s] usage: interval already open (missed close?): %v\n", sb.ID, err)
	default:
		fmt.Fprintf(os.Stderr, "[%s] usage: open interval: %v\n", sb.ID, err)
	}
}

// meterStop closes a sandbox's billable interval.
//
// It samples the VM's consumed CPU FIRST, because the cgroup leaf is removed
// when the VMM exits and this is the last moment the reading exists. Callers
// must therefore invoke it before stopping the VM. When the VM is already gone
// the sample fails and the ledger keeps whatever the sampler last recorded.
//
// reason is overridden during shutdown: a freeze driven by SIGTERM is a
// platform event, and reading "hibernate" for the whole fleet at once would
// hide that from anyone auditing an invoice.
func (s *Server) meterStop(ctx context.Context, id, reason string) {
	ctx, cancel := meterCtx(ctx)
	defer cancel()

	if s.shuttingDown.Load() {
		reason = registry.EndShutdown
	}
	closed, ok, err := s.reg.CloseUsageInterval(ctx, id, reason, s.sampleCPUUsec(id))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] usage: close interval: %v\n", id, err)
		return
	}
	if ok {
		s.creditBillable(closed)
	}
}

// creditBillable advances this host's cumulative billable counters.
//
// Crediting happens at CLOSE, with the interval's whole quantity, because that
// is the first moment the amount is final. The counters are therefore lumpy —
// a long-lived sandbox contributes nothing until it is frozen or destroyed —
// and they reset when the process does, which is exactly what a Prometheus
// counter is allowed to do. Deriving them from the ledger instead would look
// smoother and be wrong: pruning deletes spooled rows after the retention
// window, so a ledger-derived counter would silently DECREASE and be read as a
// reset.
func (s *Server) creditBillable(iv registry.UsageInterval) {
	seconds := int64(iv.Duration() / time.Second)
	s.met.billableVcpuSeconds.Add(iv.Vcpus * seconds)
	s.met.billableMemMIBSeconds.Add(iv.MemMIB * seconds)
	s.met.consumedCPUUsec.Add(iv.CPUUsec)
}

// meterCtx detaches a ledger write from its caller's cancellation.
//
// Both hooks run on paths carrying a REQUEST context. A client that disconnects
// mid-destroy would otherwise cancel the close, and a claim whose caller went
// away right after ClaimWarm would never open an interval at all — handing out a
// running sandbox that bills nothing. These writes are local, sub-millisecond,
// and must land regardless; the timeout only bounds a wedged database.
func meterCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

// sampleCPUUsec reads a live VM's consumed CPU, or -1 when it cannot be read —
// no machine, an unjailed launch on a platform without /proc, or a VMM that has
// already exited and taken its cgroup leaf with it. -1 means "keep the last
// known total" rather than "zero", so one failed read never erases a real bill.
func (s *Server) sampleCPUUsec(id string) int64 {
	v, ok := s.machines.Load(id)
	if !ok {
		return -1
	}
	sample, err := vm.SampleUsage(v.(*vm.Machine))
	if err != nil {
		return -1
	}
	return sample.CPUUsec
}

// usageSampler advances the heartbeat and consumed-CPU total of every open
// interval. It is what bounds crash loss to one tick.
//
// It also self-heals: an open interval whose VM is gone cannot be accruing, so
// it is closed rather than extended. Without that, a freeze or teardown whose
// close failed would keep billing a sandbox that no longer exists — the one
// failure mode of this design that silently grows a customer's invoice.
func (s *Server) usageSampler(ctx context.Context) {
	ticker := time.NewTicker(usageSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sampleOpenUsage(ctx)
		}
	}
}

func (s *Server) sampleOpenUsage(ctx context.Context) {
	open, err := s.reg.OpenUsageIntervals(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "usage: list open intervals: %v\n", err)
		return
	}
	for _, iv := range open {
		if _, live := s.machines.Load(iv.SandboxID); !live {
			// No VMM: whatever ended it did not get to close the interval.
			// Close at the last heartbeat (-1 keeps the sampled CPU) so the
			// bill stops where the VM did.
			closed, ok, err := s.reg.CloseUsageInterval(ctx, iv.SandboxID, registry.EndVMExit, -1)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[%s] usage: close orphaned interval: %v\n", iv.SandboxID, err)
				continue
			}
			if ok {
				s.creditBillable(closed)
			}
			fmt.Fprintf(os.Stderr, "[%s] usage: closed interval with no live VM (missed close)\n", iv.SandboxID)
			continue
		}
		if err := s.reg.TouchUsageInterval(ctx, iv.SandboxID, s.sampleCPUUsec(iv.SandboxID)); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] usage: touch interval: %v\n", iv.SandboxID, err)
		}
	}
}

// recoverUsageIntervals closes intervals left open by a server that died,
// truncating each to its last heartbeat. Called from reconcile, where every
// open interval is abandoned by definition: intervals exist only while a VM
// does, and VMs live only inside a running server.
//
// The truncation is the point. Closing at "now" instead would bill every
// customer for however long the host was down.
func (s *Server) recoverUsageIntervals(ctx context.Context) {
	n, err := s.reg.CloseAbandonedUsageIntervals(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "usage: close abandoned intervals: %v\n", err)
		return
	}
	if n > 0 {
		fmt.Fprintf(os.Stderr, "usage: closed %d interval(s) abandoned by a previous server, truncated to their last heartbeat\n", n)
	}
}
