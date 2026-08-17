package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
)

// reconcile cleans up state left behind by a previous server run. The server
// owns all VMs in-process, so on startup every registry row is stale: either
// the firecracker process is dead (host reboot, crash) or it's an orphan we
// can no longer control via the SDK. Both get torn down and their resources
// (DNAT rules, tap, rootfs copy, row) released.
func (s *Server) reconcile(ctx context.Context) {
	// Every private post-wake lineage belongs to a Firecracker bitmap held by
	// the previous process, so none can survive a server restart. Sweep the
	// directory rather than only known rows: a crash between row deletion and
	// deferred cleanup must not leak a 1 GiB logical reflink indefinitely.
	if s.cfg.Provisioner != nil {
		if entries, err := os.ReadDir(s.cfg.Provisioner.SnapshotDir); err == nil {
			for _, entry := range entries {
				if entry.IsDir() && strings.HasPrefix(entry.Name(), "hib-lineage-") {
					_ = s.cfg.Provisioner.CleanupSnapshot(entry.Name())
				}
			}
		} else if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "reconcile: list snapshot directory for private lineage cleanup: %v\n", err)
		}
	}
	// Billable intervals exist only while a VM does, and VMs live only inside a
	// running server — so every interval still open at startup was abandoned by
	// the previous process. Close them at their last heartbeat, never at now:
	// the host was down, and billing that span is the one error in the ledger a
	// customer would certainly notice.
	s.recoverUsageIntervals(ctx)

	rows, err := s.reg.All(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile: list registry: %v\n", err)
		return
	}
	for _, sb := range rows {
		// Hibernated sandboxes are the one kind of row that legitimately
		// outlives a server run: no VM, no tap, no DNAT — just the rootfs
		// file, hibernation snapshot, and explicit port listeners, all of which
		// must survive so
		// the sandbox stays wakeable after the restart. EXCEPT one that another
		// host has adopted away while we were down (roadmap B4): the durable
		// owner fence then names a different host, and keeping our stale local
		// row would re-bind its port listeners and re-heartbeat the id, splitting
		// ownership. Relinquish it — drop the local row + artifacts, leaving the
		// GCS artifacts (now the adopter's) untouched. Only on a definitive
		// "fence names another host" read; any GCS error or absent fence keeps
		// the row (we can't prove we lost it).
		if sb.Status == registry.StatusHibernated {
			s.relinquishIfAdoptedAway(ctx, sb)
			continue
		}
		if isFirecrackerProc(sb.PID) {
			killWithGrace(sb.PID, 5*time.Second)
		}
		// Legacy DNAT cleanup: port forwarding is a userspace proxy now (no
		// kernel rules to remove), but hosts upgrading from the DNAT scheme
		// may still carry rules for these rows. Removing a nonexistent rule
		// is harmless. Read port mappings before reg.Destroy deletes
		// their rows.
		if ports, err := s.reg.Ports(ctx, sb.ID); err == nil {
			for _, pm := range ports {
				if pm.HostPort != 0 {
					s.cfg.Provisioner.RemovePortForwardTo(pm.HostPort, sb.GuestIP, pm.GuestPort)
				}
			}
		} else {
			fmt.Fprintf(os.Stderr, "reconcile: list ports for %s: %v\n", sb.ID, err)
		}
		_ = s.cfg.Provisioner.DeleteTap(sb.TapDevice)
		_ = s.cfg.Provisioner.DeleteNetns(sb.ID)
		_ = s.cfg.Provisioner.RemoveRootfs(sb.RootfsPath)
		// A crash mid-hibernate can leave artifacts behind a still-'running' row.
		_ = s.cfg.Provisioner.CleanupSnapshot(hibID(sb.ID))
		if err := s.reg.Destroy(ctx, sb.ID); err != nil {
			fmt.Fprintf(os.Stderr, "reconcile: destroy row %s: %v\n", sb.ID, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "reconcile: cleaned up stale sandbox %s (pid %d)\n", sb.ID, sb.PID)
	}
	s.reconcileNetns(ctx)
}

// reconcileNetns drops per-VM network namespaces no row claims. They are bind
// mounts under /var/run/netns, so like taps they outlive a serve crash — and
// unlike taps nothing else would ever notice them, so a leak is permanent.
// Sweeping the directory (rather than only the rows above) is what covers a
// crash between row deletion and teardown.
func (s *Server) reconcileNetns(ctx context.Context) {
	if s.cfg.Provisioner == nil {
		return
	}
	names, err := provisioner.ListNetns()
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile: list network namespaces: %v\n", err)
		return
	}
	if len(names) == 0 {
		return
	}
	// Hibernated rows keep their namespace: they are wakeable, and the wake
	// reuses it rather than rebuilding it.
	live := map[string]bool{}
	if rows, err := s.reg.All(ctx); err == nil {
		for _, sb := range rows {
			live[provisioner.NetnsName(sb.ID)] = true
		}
	} else {
		// Without the row list we cannot tell an orphan from a live namespace,
		// and deleting a live one would cut a running sandbox off the network.
		fmt.Fprintf(os.Stderr, "reconcile: skip namespace sweep, cannot list rows: %v\n", err)
		return
	}
	removed := 0
	for _, name := range names {
		if live[name] {
			continue
		}
		if out, err := exec.Command("ip", "netns", "del", name).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "reconcile: delete orphan netns %s: %v: %s\n", name, err, out)
			continue
		}
		removed++
	}
	if removed > 0 {
		fmt.Fprintf(os.Stderr, "reconcile: removed %d orphaned network namespace(s)\n", removed)
	}
}

// isFirecrackerProc reports whether pid is alive AND is a firecracker process,
// guarding against PID reuse after a reboot. Returns false on non-Linux
// (no /proc), which is fine — the server only runs on Linux.
func isFirecrackerProc(pid int) bool {
	if pid <= 0 {
		return false
	}
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(comm)) == "firecracker"
}

// killWithGrace SIGTERMs pid, waits up to grace for it to exit, then SIGKILLs.
func killWithGrace(pid int, grace time.Duration) {
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
