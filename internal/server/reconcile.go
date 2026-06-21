package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

// reconcile cleans up state left behind by a previous server run. The server
// owns all VMs in-process, so on startup every registry row is stale: either
// the firecracker process is dead (host reboot, crash) or it's an orphan we
// can no longer control via the SDK. Both get torn down and their resources
// (DNAT rules, tap, rootfs copy, row) released.
func (s *Server) reconcile(ctx context.Context) {
	rows, err := s.reg.All(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile: list registry: %v\n", err)
		return
	}
	for _, sb := range rows {
		if isFirecrackerProc(sb.PID) {
			killWithGrace(sb.PID, 5*time.Second)
		}
		// Read extra port mappings before reg.Destroy deletes their rows.
		if ports, err := s.reg.Ports(ctx, sb.ID); err == nil {
			for _, pm := range ports {
				s.cfg.Provisioner.RemovePortForwardTo(pm.HostPort, sb.GuestIP, pm.GuestPort)
			}
		} else {
			fmt.Fprintf(os.Stderr, "reconcile: list extra ports for %s: %v\n", sb.ID, err)
		}
		s.cfg.Provisioner.RemovePortForward(sb.HostPort, sb.GuestIP)
		_ = s.cfg.Provisioner.DeleteTap(sb.TapDevice)
		_ = s.cfg.Provisioner.RemoveRootfs(sb.RootfsPath)
		if err := s.reg.Destroy(ctx, sb.ID); err != nil {
			fmt.Fprintf(os.Stderr, "reconcile: destroy row %s: %v\n", sb.ID, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "reconcile: cleaned up stale sandbox %s (pid %d)\n", sb.ID, sb.PID)
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
