package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

// Idle hibernation frees the resources of sandboxes nobody is talking to —
// the density lever that lets a host's slot count absorb bursts. A sandbox
// idle for cfg.HibernateAfter is paused, full-snapshotted (memory + device
// state; its rootfs file simply stays where it is — the frozen VM can't write
// to it), and killed. The row flips to status=hibernated and its tap/IP/port
// return to the pools. Hibernated sandboxes survive server restarts.
//
// Any agent-bound request (exec, files, dir, shell, expose-port) wakes it:
// a plain snapshot restore when its old tap+IP are still free (the common
// case — the pool pickers avoid hibernated identities), else the
// identity-neutral clone path (fresh tap/IP + MMDS reidentify + GARP, exactly
// like fan-out). Only network traffic to forwarded host ports does NOT count
// as activity and does NOT wake — the DNAT path bypasses the server.

// hibID names the snapshot-dir entry holding a sandbox's hibernation
// artifacts (mem + device state; the rootfs needs no frozen copy).
func hibID(id string) string { return "hib-" + id }

// hibernateTick is how often the reaper looks for idle sandboxes.
const hibernateTick = 30 * time.Second

// --- activity tracking ---

// activityTracker records, per sandbox, when the API last touched it and how
// many requests are in flight (a sandbox with an open shell or a running
// exec stream is never idle, no matter how long ago it started).
type activityTracker struct {
	mu       sync.Mutex
	last     map[string]time.Time
	inflight map[string]int
}

func newActivityTracker() *activityTracker {
	return &activityTracker{last: map[string]time.Time{}, inflight: map[string]int{}}
}

// begin marks a request against id as started; the returned func marks it
// done. Both ends bump last-activity, so the idle window starts when a
// long-running exec/shell ENDS, not when it began.
func (a *activityTracker) begin(id string) func() {
	a.mu.Lock()
	a.last[id] = time.Now()
	a.inflight[id]++
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.last[id] = time.Now()
		if a.inflight[id]--; a.inflight[id] <= 0 {
			delete(a.inflight, id)
		}
		a.mu.Unlock()
	}
}

func (a *activityTracker) touch(id string) {
	a.mu.Lock()
	a.last[id] = time.Now()
	a.mu.Unlock()
}

func (a *activityTracker) forget(id string) {
	a.mu.Lock()
	delete(a.last, id)
	delete(a.inflight, id)
	a.mu.Unlock()
}

// idleFor reports how long id has been idle. busy=true means requests are in
// flight right now. ok=false means the tracker has never seen id.
func (a *activityTracker) idleFor(id string) (idle time.Duration, busy, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	last, ok := a.last[id]
	if !ok {
		return 0, false, false
	}
	return time.Since(last), a.inflight[id] > 0, true
}

// --- wake/hibernate serialization ---

// wakeLock returns a mutex dedicated to one sandbox id, serializing
// hibernate, wake, and destroy against each other (mirrors pullLock).
func (s *Server) wakeLock(id string) *sync.Mutex {
	s.wakesMu.Lock()
	defer s.wakesMu.Unlock()
	mu, ok := s.wakes[id]
	if !ok {
		mu = &sync.Mutex{}
		s.wakes[id] = mu
	}
	return mu
}

// --- the idle reaper ---

// hibernateLoop periodically freezes sandboxes idle past their window: the
// per-sandbox hibernate_after_sec when set (>0 custom, -1 never), else the
// host-wide cfg.HibernateAfter (0 = no default — only opted-in sandboxes
// hibernate). Serial on purpose (each hibernation writes ~memMiB to disk —
// a stampede would saturate I/O).
func (s *Server) hibernateLoop(ctx context.Context) {
	ticker := time.NewTicker(hibernateTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			running, err := s.reg.List(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hibernate: list sandboxes: %v\n", err)
				continue
			}
			for _, sb := range running {
				window := s.cfg.HibernateAfter
				if sb.HibernateAfterSec > 0 {
					window = time.Duration(sb.HibernateAfterSec) * time.Second
				}
				if sb.HibernateAfterSec < 0 || window <= 0 {
					continue // opted out, or no per-sandbox value and no host default
				}
				// Only sandboxes whose VM we actually hold; rows mid-create
				// aren't in machines yet.
				if _, ok := s.machines.Load(sb.ID); !ok {
					continue
				}
				idle, busy, ok := s.act.idleFor(sb.ID)
				if !ok {
					// First sighting (e.g. server code just started tracking):
					// start its idle clock now.
					s.act.touch(sb.ID)
					continue
				}
				if busy || idle < window {
					continue
				}
				if err := s.hibernate(ctx, sb.ID); err != nil {
					fmt.Fprintf(os.Stderr, "[%s] hibernate failed: %v\n", sb.ID, err)
				}
			}
		}
	}
}

// hibernate freezes one running sandbox to disk and releases its resources.
func (s *Server) hibernate(ctx context.Context, id string) error {
	mu := s.wakeLock(id)
	mu.Lock()
	defer mu.Unlock()

	sb, err := s.reg.Get(ctx, id)
	if err != nil {
		return err
	}
	if sb.Status != registry.StatusRunning {
		return fmt.Errorf("sandbox %s is %s, not running", id, sb.Status)
	}
	v, ok := s.machines.Load(id)
	if !ok {
		return fmt.Errorf("sandbox %s has no VM in this server", id)
	}
	m := v.(*vm.Machine)
	// Re-check under the lock: a request may have raced in since the reaper's
	// scan decided this sandbox was idle.
	if _, busy, _ := s.act.idleFor(id); busy {
		return fmt.Errorf("sandbox %s is busy", id)
	}

	t0 := time.Now()
	memPath, statePath, _, err := s.cfg.Provisioner.SnapshotPaths(hibID(id))
	if err != nil {
		return fmt.Errorf("hibernate dir: %w", err)
	}
	if err := vm.Pause(ctx, m); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	if err := vm.Snapshot(ctx, m, memPath, statePath, vm.SnapshotFull); err != nil {
		// Snapshot failed — thaw the sandbox and pretend nothing happened.
		if rerr := vm.Resume(context.Background(), m); rerr != nil {
			fmt.Fprintf(os.Stderr, "[%s] resume after failed hibernate snapshot: %v\n", id, rerr)
		}
		_ = s.cfg.Provisioner.CleanupSnapshot(hibID(id))
		return fmt.Errorf("snapshot: %w", err)
	}

	// Frozen state is on disk; the VM process is now redundant. Kill it — no
	// guest shutdown, the guest must not observe anything.
	s.machines.Delete(id)
	_ = vm.StopForce(m)

	// Release host-side resources. Extra-port DNAT rules go too; their rows
	// stay, and wake re-adds the rules (against the possibly-new guest IP).
	if ports, err := s.reg.Ports(ctx, id); err == nil {
		for _, pm := range ports {
			s.cfg.Provisioner.RemovePortForwardTo(pm.HostPort, sb.GuestIP, pm.GuestPort)
		}
	}
	s.cfg.Provisioner.RemovePortForward(sb.HostPort, sb.GuestIP)
	_ = s.cfg.Provisioner.DeleteTap(sb.TapDevice)

	if err := s.reg.Hibernate(context.Background(), id); err != nil {
		// Row didn't flip — the VM is already gone, so surface loudly. The
		// row stays 'running' and reconcile will clean it up on next restart.
		return fmt.Errorf("mark hibernated (sandbox is frozen but row is stale!): %w", err)
	}
	s.act.forget(id)
	fmt.Fprintf(os.Stderr, "[%s] hibernated in %s (idle sandbox frozen, slot freed)\n",
		id, time.Since(t0).Round(time.Millisecond))
	return nil
}

// --- wake ---

// ensureRunning returns the sandbox row, waking it first when hibernated.
// Every agent-bound handler goes through this.
func (s *Server) ensureRunning(ctx context.Context, id string) (registry.Sandbox, error) {
	sb, err := s.reg.Get(ctx, id)
	if err != nil {
		return sb, err
	}
	if sb.Status != registry.StatusHibernated {
		return sb, nil
	}
	return s.wake(ctx, id)
}

// wake brings a hibernated sandbox back to running and blocks until its agent
// answers. Concurrent wakes of the same id collapse onto one (the losers find
// the row already running when they get the lock).
func (s *Server) wake(ctx context.Context, id string) (registry.Sandbox, error) {
	mu := s.wakeLock(id)
	mu.Lock()
	defer mu.Unlock()

	sb, err := s.reg.Get(ctx, id)
	if err != nil {
		return sb, err
	}
	if sb.Status == registry.StatusRunning {
		return sb, nil // another request won the race and woke it
	}
	if sb.Status != registry.StatusHibernated {
		return sb, fmt.Errorf("sandbox %s is %s", id, sb.Status)
	}

	memPath, statePath, _, err := s.cfg.Provisioner.SnapshotPaths(hibID(id))
	if err != nil {
		return sb, err
	}
	for _, p := range []string{memPath, statePath} {
		if _, err := os.Stat(p); err != nil {
			return sb, fmt.Errorf("hibernation artifacts missing for %s: %w", id, err)
		}
	}

	t0 := time.Now()
	sb, same, err := s.reg.Wake(ctx, id)
	if err != nil {
		return sb, err
	}
	if same {
		err = s.wakeRestore(ctx, sb, memPath, statePath)
	} else {
		err = s.wakeClone(ctx, sb, memPath, statePath)
	}
	if err != nil {
		// Roll the row back to hibernated — the artifacts are untouched, so
		// the sandbox stays wakeable (or destroyable) later.
		s.rollbackWake(sb)
		return sb, fmt.Errorf("wake %s: %w", id, err)
	}

	// Re-add extra-port DNAT rules against the (possibly new) guest IP.
	if ports, perr := s.reg.Ports(ctx, id); perr == nil {
		for _, pm := range ports {
			if aerr := s.cfg.Provisioner.AddPortForwardTo(pm.HostPort, sb.GuestIP, pm.GuestPort); aerr != nil {
				fmt.Fprintf(os.Stderr, "[%s] wake: re-add port %d->%d: %v\n", id, pm.HostPort, pm.GuestPort, aerr)
			}
		}
	}

	// The frozen memory was consumed into the live VM; drop the artifacts.
	_ = s.cfg.Provisioner.CleanupSnapshot(hibID(id))
	s.act.touch(id)
	fmt.Fprintf(os.Stderr, "[%s] woke from hibernation in %s (same_identity=%v)\n",
		id, time.Since(t0).Round(time.Millisecond), same)
	return sb, nil
}

// rollbackWake undoes a failed wake attempt: kills any half-started VM,
// removes whatever host-side resources were added, and flips the row back to
// hibernated. Best-effort throughout — the artifacts on disk stay intact.
func (s *Server) rollbackWake(sb registry.Sandbox) {
	if v, ok := s.machines.LoadAndDelete(sb.ID); ok {
		_ = vm.StopForce(v.(*vm.Machine))
	}
	s.cfg.Provisioner.RemovePortForward(sb.HostPort, sb.GuestIP)
	_ = s.cfg.Provisioner.DeleteTap(sb.TapDevice)
	if err := s.reg.Hibernate(context.Background(), sb.ID); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] rollback to hibernated failed: %v\n", sb.ID, err)
	}
}

// wakeRestore resumes the snapshot on its original identity — the tap and
// guest IP baked into the frozen memory are still free, so this is a plain
// restore: no reidentify, no GARP wait.
func (s *Server) wakeRestore(ctx context.Context, sb registry.Sandbox, memPath, statePath string) error {
	if err := s.cfg.Provisioner.CreateTap(sb.TapDevice); err != nil {
		return fmt.Errorf("create tap: %w", err)
	}

	opts := s.cfg.VMTemplate
	opts.RootfsPath = sb.RootfsPath
	opts.SocketPath = ""
	m, rt, err := vm.NewMachineFromSnapshot(s.vmCtx, opts, memPath, statePath, false)
	if err != nil {
		return fmt.Errorf("new machine from snapshot: %w", err)
	}
	if err := vm.Start(s.vmCtx, m); err != nil {
		_ = vm.StopForce(m)
		return fmt.Errorf("load snapshot + resume: %w", err)
	}
	pid, err := vm.PID(m)
	if err != nil {
		_ = vm.StopForce(m)
		return fmt.Errorf("pid: %w", err)
	}
	if err := s.cfg.Provisioner.AddPortForward(sb.HostPort, sb.GuestIP); err != nil {
		_ = vm.StopForce(m)
		return fmt.Errorf("port forward: %w", err)
	}
	if err := s.reg.FinishStart(ctx, sb.ID, pid, rt.VMID, rt.SocketPath); err != nil {
		_ = vm.StopForce(m)
		return fmt.Errorf("finish start: %w", err)
	}
	s.machines.Store(sb.ID, m)
	go func(id string) {
		_ = vm.Wait(context.Background(), m)
		fmt.Fprintf(os.Stderr, "[%s] woken VM exited\n", id)
	}(sb.ID)

	// Step the guest's snapshot-stale wall clock (same as 1:1 restore).
	if err := vm.PushEpoch(ctx, rt.SocketPath); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] push epoch to mmds: %v\n", sb.ID, err)
	}
	if err := waitForAgent(ctx, sb.GuestIP, 30*time.Second); err != nil {
		return fmt.Errorf("agent never ready after wake: %w", err)
	}
	return nil
}

// wakeClone resumes the snapshot under a fresh identity — something claimed
// the old tap/IP while the sandbox slept. Same two-phase resume-then-bridge
// dance as fan-out: unbridged tap, MMDS reidentify, bridge on the GARP
// announce. Gen must differ from anything the frozen agent has seen, or it
// would skip the reidentify.
func (s *Server) wakeClone(ctx context.Context, sb registry.Sandbox, memPath, statePath string) error {
	if err := s.cfg.Provisioner.CreateTapUnbridged(sb.TapDevice); err != nil {
		return fmt.Errorf("create tap: %w", err)
	}
	arp, err := provisioner.ListenARP(sb.TapDevice)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] arp listener on %s failed (will sleep instead): %v\n", sb.ID, sb.TapDevice, err)
		arp = nil
	}
	opts := s.cfg.VMTemplate
	opts.SocketPath = ""
	m, rt, err := vm.StartClone(s.vmCtx, opts, vm.CloneParams{
		MemPath:         memPath,
		StatePath:       statePath,
		CloneRootfsPath: sb.RootfsPath, // its own rootfs, exactly where the snapshot expects it
		TapDevice:       sb.TapDevice,
		GuestIP:         sb.GuestIP,
		MacAddress:      randomMAC(),
		GatewayIP:       s.cfg.GatewayIP,
		Prefix:          24,
		Gen:             uuid.NewString(),
	})
	if err != nil {
		if arp != nil {
			_ = arp.Close()
		}
		return fmt.Errorf("start clone: %w", err)
	}
	c := &clone{sb: sb, m: m, vmID: rt.VMID, sock: rt.SocketPath, arp: arp}
	return s.finishClone(ctx, c)
}

// --- HTTP ---

// handleHibernate manually freezes a running sandbox (the reaper does the
// same automatically after the configured idle window).
func (s *Server) handleHibernate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.reg.Get(r.Context(), id); err != nil {
		httpError(w, 404, err)
		return
	}
	if err := s.hibernate(r.Context(), id); err != nil {
		httpError(w, 409, err)
		return
	}
	sb, err := s.reg.Get(r.Context(), id)
	if err != nil {
		httpError(w, 500, err)
		return
	}
	writeJSON(w, 200, sb)
}
