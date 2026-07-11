package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

// handleSnapshot pauses a running sandbox, writes a full Firecracker snapshot
// (memory + device state) plus a frozen copy of its rootfs, then resumes the
// sandbox so it keeps running. The resulting snapshot can be restored later
// into a new sandbox via POST /snapshots/{id}/restore.
//
// The source must be killed (or expire) before a restore can use the snapshot:
// the snapshot bakes in the guest IP and tap name, so a restore reuses both and
// would collide with the still-running source.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, status, err := s.snapshotSandbox(r.Context(), r.PathValue("id"), false)
	if err != nil {
		httpError(w, status, err)
		return
	}
	writeJSON(w, 201, snap)
}

// snapshotSandbox does the actual pause → snapshot → freeze rootfs → resume
// dance and records the row. golden marks the row as the server's golden
// snapshot (see golden.go). Returns the HTTP status to use on error.
//
// When the sandbox is a golden clone with dirty-page tracking (every
// hot-created sandbox), the snapshot is stored as a DIFF against its golden
// base: the mem file holds only pages dirtied since clone, and the GCS upload
// sends only rootfs extents that diverged from the base. Everything else
// (cold boots, restores, user fan-out clones, the golden build itself) is a
// self-contained FULL snapshot.
func (s *Server) snapshotSandbox(ctx context.Context, id string, golden bool) (registry.Snapshot, int, error) {
	sb, err := s.reg.Get(ctx, id)
	if err != nil {
		return registry.Snapshot{}, 404, err
	}
	v, ok := s.machines.Load(id)
	if !ok {
		return registry.Snapshot{}, 409, fmt.Errorf("sandbox %s is not running in this server", id)
	}
	m := v.(*vm.Machine)

	format, baseID := registry.FormatFull, ""
	if !golden && sb.BaseSnapshotID != "" && vm.DiffCapable(m) {
		// The base must still exist locally — it anchors the diff (and the
		// upload path reads its artifacts). A rebuilt/deleted golden falls
		// back to a full snapshot.
		if base, err := s.reg.GetSnapshot(ctx, sb.BaseSnapshotID); err == nil && base.Golden {
			format, baseID = registry.FormatDiff, base.ID
		}
	}
	snapType := vm.SnapshotFull
	if format == registry.FormatDiff {
		snapType = vm.SnapshotDiff
	}

	snapID := uuid.NewString()
	memPath, statePath, rootfsPath, err := s.cfg.Provisioner.SnapshotPaths(snapID)
	if err != nil {
		return registry.Snapshot{}, 500, fmt.Errorf("snapshot dir: %w", err)
	}

	// Resume on every exit path so a failed snapshot doesn't leave the source
	// sandbox frozen.
	if err := vm.Pause(ctx, m); err != nil {
		return registry.Snapshot{}, 500, fmt.Errorf("pause: %w", err)
	}
	resumed := false
	resume := func() {
		if !resumed {
			resumed = true
			if err := vm.Resume(context.Background(), m); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] resume after snapshot failed: %v\n", id, err)
			}
		}
	}
	defer resume()

	t0 := time.Now()
	if err := vm.Snapshot(ctx, m, memPath, statePath, snapType); err != nil {
		_ = s.cfg.Provisioner.CleanupSnapshot(snapID)
		return registry.Snapshot{}, 500, fmt.Errorf("create snapshot: %w", err)
	}
	// Copy the rootfs while the VM is paused so the disk matches the snapshot's
	// view of it. The source keeps writing to its own rootfs after resume.
	if err := s.cfg.Provisioner.CopyFileSparse(sb.RootfsPath, rootfsPath); err != nil {
		_ = s.cfg.Provisioner.CleanupSnapshot(snapID)
		return registry.Snapshot{}, 500, fmt.Errorf("freeze rootfs: %w", err)
	}
	resume()
	fmt.Fprintf(os.Stderr, "[%s] snapshot %s written in %s\n", id, snapID, time.Since(t0).Round(time.Millisecond))

	// Stamp the base rootfs so a rebuilt base (e.g. install-agent) invalidates
	// a golden snapshot on the next startup.
	var baseMtime, baseSize int64
	if fi, err := os.Stat(s.cfg.Provisioner.RootfsBase); err == nil {
		baseMtime, baseSize = fi.ModTime().Unix(), fi.Size()
	}

	snap := registry.Snapshot{
		ID:               snapID,
		SourceID:         id,
		TapDevice:        sb.TapDevice,
		GuestIP:          sb.GuestIP,
		MemPath:          memPath,
		StatePath:        statePath,
		RootfsPath:       rootfsPath,
		SourceRootfsPath: sb.RootfsPath,
		CreatedAt:        time.Now(),
		Golden:           golden,
		BaseMtime:        baseMtime,
		BaseSize:         baseSize,
		Format:           format,
		BaseID:           baseID,
	}
	if err := s.reg.CreateSnapshot(ctx, snap); err != nil {
		_ = s.cfg.Provisioner.CleanupSnapshot(snapID)
		return registry.Snapshot{}, 500, fmt.Errorf("record snapshot: %w", err)
	}
	// Durability: ship the snapshot to GCS in the background. The caller gets
	// its 201 now; until meta.json lands the snapshot is host-local only.
	if !golden && s.blob != nil {
		go s.uploadSnapshot(snap)
	}
	return snap, 201, nil
}

// handleRestore boots a brand-new sandbox from a snapshot by loading its memory
// + device state and resuming — skipping kernel boot, init, and agent startup.
// The new sandbox reuses the snapshot's tap and guest IP (baked into the
// snapshot) and is allocated a fresh host port.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	snapID := r.PathValue("id")

	snap, err := s.ensureSnapshotLocal(ctx, snapID)
	if err != nil {
		httpError(w, 404, fmt.Errorf("snapshot %s not found: %w", snapID, err))
		return
	}
	// A diff snapshot's mem file holds only dirty pages; Firecracker needs the
	// rebased full file.
	if snap.MemPath, err = s.materializeMem(ctx, snap); err != nil {
		httpError(w, 500, fmt.Errorf("materialize snapshot memory: %w", err))
		return
	}

	var body struct {
		TimeoutSec int `json:"timeout_sec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}
	if body.TimeoutSec < 0 {
		httpError(w, 400, errors.New("timeout_sec must be >= 0"))
		return
	}
	var expiresAt *time.Time
	if body.TimeoutSec > 0 {
		t := time.Now().Add(time.Duration(body.TimeoutSec) * time.Second)
		expiresAt = &t
	}

	id := uuid.NewString()
	// The disk path is baked into the snapshot, so the restored VM's rootfs must
	// live exactly there — Firecracker reattaches the block device by that path.
	rootfsPath := snap.SourceRootfsPath

	// Insert the row first: its partial unique indexes gate on the snapshot's
	// tap + guest IP, so a restore fails cleanly (before any disk work) if the
	// source or a prior restore is still live.
	sb, err := s.reg.CreateRestore(ctx, id, rootfsPath, snap.TapDevice, snap.GuestIP, expiresAt)
	if err != nil {
		httpError(w, 409, fmt.Errorf("registry restore: %w", err))
		return
	}

	tRoot := time.Now()
	if err := s.cfg.Provisioner.CopyFileSparse(snap.RootfsPath, rootfsPath); err != nil {
		s.rollbackPreVM(id, sb)
		httpError(w, 500, fmt.Errorf("copy snapshot rootfs: %w", err))
		return
	}
	rootfsMS := time.Since(tRoot).Milliseconds()

	if err := s.cfg.Provisioner.CreateTap(sb.TapDevice); err != nil {
		s.rollbackPreVM(id, sb)
		httpError(w, 500, fmt.Errorf("create tap: %w", err))
		return
	}

	opts := s.cfg.VMTemplate
	opts.RootfsPath = rootfsPath
	opts.SocketPath = "" // auto-generate per VM

	tLoad := time.Now()
	m, rt, err := vm.NewMachineFromSnapshot(s.vmCtx, opts, snap.MemPath, snap.StatePath, false)
	if err != nil {
		s.rollbackPreVM(id, sb)
		httpError(w, 500, fmt.Errorf("new machine from snapshot: %w", err))
		return
	}
	if err := vm.Start(s.vmCtx, m); err != nil {
		_ = vm.StopForce(m)
		s.rollbackPreVM(id, sb)
		httpError(w, 500, fmt.Errorf("load snapshot + resume: %w", err))
		return
	}
	loadMS := time.Since(tLoad).Milliseconds()

	pid, err := vm.PID(m)
	if err != nil {
		_ = vm.StopForce(m)
		s.rollbackPreVM(id, sb)
		httpError(w, 500, fmt.Errorf("pid: %w", err))
		return
	}

	if err := s.cfg.Provisioner.AddPortForward(sb.HostPort, sb.GuestIP); err != nil {
		_ = vm.StopForce(m)
		s.rollbackPreVM(id, sb)
		httpError(w, 500, fmt.Errorf("port forward: %w", err))
		return
	}

	if err := s.reg.FinishStart(ctx, id, pid, rt.VMID, rt.SocketPath); err != nil {
		s.cfg.Provisioner.RemovePortForward(sb.HostPort, sb.GuestIP)
		_ = vm.StopForce(m)
		s.rollbackPreVM(id, sb)
		httpError(w, 500, fmt.Errorf("finish start: %w", err))
		return
	}

	s.machines.Store(id, m)
	s.act.touch(id)
	go func(id string) {
		err := vm.Wait(context.Background(), m)
		fmt.Fprintf(os.Stderr, "[%s] restored VM exited: %v\n", id, err)
	}(id)

	// Let the thaw agent step the guest's snapshot-stale wall clock now,
	// instead of NTP stepping it minutes forward later mid-exec.
	if err := vm.PushEpoch(ctx, rt.SocketPath); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] push epoch to mmds: %v\n", id, err)
	}

	// The agent is restored already-running in guest memory; it just needs the
	// network to settle (gratuitous ARP on the new tap). This is the win over
	// cold boot, where the agent has to start from scratch.
	tAgent := time.Now()
	if err := waitForAgent(ctx, sb.GuestIP, 30*time.Second); err != nil {
		_ = s.destroy(context.Background(), id)
		httpError(w, 500, fmt.Errorf("restored but agent never became ready: %w", err))
		return
	}
	agentMS := time.Since(tAgent).Milliseconds()
	fmt.Fprintf(os.Stderr, "[%s] restored from %s: rootfs_cp=%dms load+resume=%dms agent_ready=%dms\n",
		id, snapID, rootfsMS, loadMS, agentMS)

	sb.PID = pid
	sb.VMID = rt.VMID
	sb.SocketPath = rt.SocketPath
	writeJSON(w, 201, sb)
}

// clone is one in-flight fan-out clone between Phase 1 (resume) and Phase 2 (bridge).
type clone struct {
	sb         registry.Sandbox
	m          *vm.Machine
	vmID, sock string
	arp        *provisioner.ARPListener // opened on the unbridged tap before resume; nil = fixed-sleep fallback
	err        error
}

// reidentifyMargin bounds how long finishClone waits for the guest's
// gratuitous-ARP announce before bridging anyway. It doubles as the fallback
// sleep when no listener could be opened (or the snapshot predates the
// announcing agent), so the worst case equals the old fixed sleep.
const reidentifyMargin = 1500 * time.Millisecond

// handleFanout restores N identity-neutral clones from one snapshot concurrently.
// Each clone gets a fresh tap/IP/port from the pool (like a cold create) and its
// own reflink CoW rootfs; the snapshot's baked identity is discarded. Clones come
// up on UNBRIDGED taps and reidentify eth0 from MMDS (see vm.StartClone + the
// sandboxd thaw agent) before any tap joins br-fc, so the baked source IP — which
// every clone momentarily shares — never collides on the shared bridge.
func (s *Server) handleFanout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	snapID := r.PathValue("id")

	snap, err := s.ensureSnapshotLocal(ctx, snapID)
	if err != nil {
		httpError(w, 404, fmt.Errorf("snapshot %s not found: %w", snapID, err))
		return
	}
	// Rebase a diff snapshot's mem file once; every clone loads from it.
	if snap.MemPath, err = s.materializeMem(ctx, snap); err != nil {
		httpError(w, 500, fmt.Errorf("materialize snapshot memory: %w", err))
		return
	}

	var body struct {
		Count      int `json:"count"`
		TimeoutSec int `json:"timeout_sec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}
	if body.Count < 1 {
		httpError(w, 400, errors.New("count must be >= 1"))
		return
	}
	if body.TimeoutSec < 0 {
		httpError(w, 400, errors.New("timeout_sec must be >= 0"))
		return
	}
	var expiresAt *time.Time
	if body.TimeoutSec > 0 {
		t := time.Now().Add(time.Duration(body.TimeoutSec) * time.Second)
		expiresAt = &t
	}

	t0 := time.Now()

	// Firecracker opens the snapshot's baked rootfs path during LoadSnapshot —
	// before our per-clone PATCH /drives relocates it — so that path must exist
	// and be openable. The source's own rootfs was deleted when it was killed, so
	// stage the frozen rootfs there once (reflink, instant); remove it after all
	// clones have loaded+resumed onto their own CoW copies. Unlinking is safe even
	// if a 1:1 restore is running on it: open fds survive unlink on Linux.
	stagedBaked := false
	if _, statErr := os.Stat(snap.SourceRootfsPath); statErr != nil {
		if err := s.cfg.Provisioner.CopyFileSparse(snap.RootfsPath, snap.SourceRootfsPath); err != nil {
			httpError(w, 500, fmt.Errorf("stage snapshot rootfs at baked path: %w", err))
			return
		}
		stagedBaked = true
	}

	// Phase 1 (parallel): bring each clone up on an UNBRIDGED tap and resume it.
	// After resume the in-guest thaw agent reconfigures eth0 to the fresh IP/MAC
	// off MMDS — no host contact and no bridge needed for that step.
	clones := make([]*clone, body.Count)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8) // bound concurrent bring-up
	for i := 0; i < body.Count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			clones[i] = s.bringUpClone(snap, expiresAt)
		}(i)
	}
	wg.Wait()

	// All clones have loaded+resumed onto their own CoW rootfs; the staged baked
	// file is no longer needed (unlink is safe w.r.t. any still-open fds).
	if stagedBaked {
		_ = s.cfg.Provisioner.RemoveRootfs(snap.SourceRootfsPath)
	}

	// Phase 2 (parallel): wait for each clone's reidentify announce, bridge its
	// tap, DNAT, wait for its agent. The announce wait is per-clone inside
	// finishClone, so fast clones bridge without waiting on slow ones.
	live := make([]registry.Sandbox, 0, body.Count)
	var mu sync.Mutex
	for _, c := range clones {
		if c == nil || c.err != nil {
			if c != nil && c.err != nil {
				fmt.Fprintf(os.Stderr, "[fanout %s] clone bring-up failed: %v\n", snapID, c.err)
			}
			continue
		}
		wg.Add(1)
		go func(c *clone) {
			defer wg.Done()
			if err := s.finishClone(ctx, c); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] fanout clone finish failed: %v\n", c.sb.ID, err)
				_ = s.destroy(context.Background(), c.sb.ID)
				return
			}
			mu.Lock()
			live = append(live, c.sb)
			mu.Unlock()
		}(c)
	}
	wg.Wait()

	fmt.Fprintf(os.Stderr, "[fanout %s] %d/%d clones live in %s\n",
		snapID, len(live), body.Count, time.Since(t0).Round(time.Millisecond))
	if len(live) == 0 {
		httpError(w, 500, errors.New("all clones failed to start"))
		return
	}
	writeJSON(w, 201, live)
}

// bringUpClone allocates resources for one clone and resumes it on an unbridged
// tap. The tap is NOT yet on the bridge — finishClone does that after reidentify.
func (s *Server) bringUpClone(snap registry.Snapshot, expiresAt *time.Time) *clone {
	id := uuid.NewString()
	rootfsPath := s.cfg.Provisioner.RootfsPathFor(id)
	// Clones of the golden snapshot record it as their diff base; clones of a
	// user snapshot don't (no diff chains — their snapshots go full).
	baseID := ""
	if snap.Golden {
		baseID = snap.ID
	}
	sb, err := s.reg.Create(s.vmCtx, id, rootfsPath, expiresAt, baseID)
	if err != nil {
		return &clone{err: fmt.Errorf("registry create: %w", err)}
	}
	if _, err := s.cfg.Provisioner.CloneRootfs(id, snap.RootfsPath); err != nil {
		s.rollbackPreVM(id, sb)
		return &clone{sb: sb, err: fmt.Errorf("clone rootfs: %w", err)}
	}
	if err := s.cfg.Provisioner.CreateTapUnbridged(sb.TapDevice); err != nil {
		s.rollbackPreVM(id, sb)
		return &clone{sb: sb, err: fmt.Errorf("create tap: %w", err)}
	}
	// Listen for the guest's reidentify announce BEFORE resuming, so it can't
	// be missed. Failure is non-fatal: finishClone falls back to a fixed sleep.
	arp, err := provisioner.ListenARP(sb.TapDevice)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] arp listener on %s failed (will sleep instead): %v\n", id, sb.TapDevice, err)
		arp = nil
	}
	opts := s.cfg.VMTemplate
	opts.SocketPath = ""
	m, rt, err := vm.StartClone(s.vmCtx, opts, vm.CloneParams{
		MemPath:         snap.MemPath,
		StatePath:       snap.StatePath,
		CloneRootfsPath: rootfsPath,
		TapDevice:       sb.TapDevice,
		GuestIP:         sb.GuestIP,
		MacAddress:      randomMAC(),
		GatewayIP:       s.cfg.GatewayIP,
		Prefix:          24,
		Gen:             id,
	})
	if err != nil {
		if arp != nil {
			_ = arp.Close()
		}
		s.rollbackPreVM(id, sb)
		return &clone{sb: sb, err: fmt.Errorf("start clone: %w", err)}
	}
	return &clone{sb: sb, m: m, vmID: rt.VMID, sock: rt.SocketPath, arp: arp}
}

// finishClone waits for the guest's reidentify announce, then bridges the
// clone's tap, sets up port forwarding, records it, and waits for its agent on
// the fresh IP.
func (s *Server) finishClone(ctx context.Context, c *clone) error {
	sb, m := c.sb, c.m

	// The tap must stay off the bridge until the guest sheds the snapshot's
	// baked IP. Normally the thaw agent's gratuitous ARP tells us the moment
	// that happens (~200-400ms); the timeout covers agents that predate the
	// announce, matching the old fixed sleep.
	if c.arp != nil {
		if err := c.arp.WaitForSenderIP(sb.GuestIP, reidentifyMargin); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] no reidentify announce (agent in snapshot predates GARP?): %v\n", sb.ID, err)
		}
		_ = c.arp.Close()
		c.arp = nil
	} else {
		time.Sleep(reidentifyMargin)
	}

	pid, err := vm.PID(m)
	if err != nil {
		_ = vm.StopForce(m)
		return fmt.Errorf("pid: %w", err)
	}
	if err := s.cfg.Provisioner.AttachTapToBridge(sb.TapDevice); err != nil {
		_ = vm.StopForce(m)
		return fmt.Errorf("attach tap: %w", err)
	}
	if err := s.cfg.Provisioner.AddPortForward(sb.HostPort, sb.GuestIP); err != nil {
		_ = vm.StopForce(m)
		return fmt.Errorf("port forward: %w", err)
	}
	if err := s.reg.FinishStart(ctx, sb.ID, pid, c.vmID, c.sock); err != nil {
		_ = vm.StopForce(m)
		return fmt.Errorf("finish start: %w", err)
	}
	s.machines.Store(sb.ID, m)
	s.act.touch(sb.ID)
	go func(id string) {
		_ = vm.Wait(context.Background(), m)
		fmt.Fprintf(os.Stderr, "[%s] clone VM exited\n", id)
	}(sb.ID)
	if err := waitForAgent(ctx, sb.GuestIP, 30*time.Second); err != nil {
		return fmt.Errorf("agent never ready on %s: %w", sb.GuestIP, err)
	}
	return nil
}

// handleListSnapshots returns all saved snapshots.
func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	snaps, err := s.reg.ListSnapshots(r.Context())
	if err != nil {
		httpError(w, 500, err)
		return
	}
	if snaps == nil {
		snaps = []registry.Snapshot{}
	}
	writeJSON(w, 200, snaps)
}

// handleDeleteSnapshot removes a snapshot's row, its artifact files, and (in
// the background) its GCS objects. Base templates are never deleted — other
// snapshots may still reference them.
func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.reg.DeleteSnapshot(r.Context(), id); err != nil {
		httpError(w, 404, err)
		return
	}
	// If the golden snapshot was deleted, stop hot-creating from it. Creates
	// cold-boot until the next server restart rebuilds a golden snapshot.
	if g := s.golden.Load(); g != nil && g.ID == id {
		s.golden.Store(nil)
	}
	_ = s.cfg.Provisioner.CleanupSnapshot(id)
	if s.blob != nil {
		go s.deleteSnapshotObjects(id)
	}
	w.WriteHeader(http.StatusNoContent)
}
