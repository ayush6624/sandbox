package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"

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
	ctx := r.Context()
	id := r.PathValue("id")

	sb, err := s.reg.Get(ctx, id)
	if err != nil {
		httpError(w, 404, err)
		return
	}
	v, ok := s.machines.Load(id)
	if !ok {
		httpError(w, 409, fmt.Errorf("sandbox %s is not running in this server", id))
		return
	}
	m := v.(*vm.Machine)

	snapID := uuid.NewString()
	memPath, statePath, rootfsPath, err := s.cfg.Provisioner.SnapshotPaths(snapID)
	if err != nil {
		httpError(w, 500, fmt.Errorf("snapshot dir: %w", err))
		return
	}

	// Pause → snapshot → freeze rootfs → resume. Resume on every exit path so a
	// failed snapshot doesn't leave the source sandbox frozen.
	if err := vm.Pause(ctx, m); err != nil {
		httpError(w, 500, fmt.Errorf("pause: %w", err))
		return
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
	if err := vm.Snapshot(ctx, m, memPath, statePath); err != nil {
		_ = s.cfg.Provisioner.CleanupSnapshot(snapID)
		httpError(w, 500, fmt.Errorf("create snapshot: %w", err))
		return
	}
	// Copy the rootfs while the VM is paused so the disk matches the snapshot's
	// view of it. The source keeps writing to its own rootfs after resume.
	if err := s.cfg.Provisioner.CopyFileSparse(sb.RootfsPath, rootfsPath); err != nil {
		_ = s.cfg.Provisioner.CleanupSnapshot(snapID)
		httpError(w, 500, fmt.Errorf("freeze rootfs: %w", err))
		return
	}
	resume()
	fmt.Fprintf(os.Stderr, "[%s] snapshot %s written in %s\n", id, snapID, time.Since(t0).Round(time.Millisecond))

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
	}
	if err := s.reg.CreateSnapshot(ctx, snap); err != nil {
		_ = s.cfg.Provisioner.CleanupSnapshot(snapID)
		httpError(w, 500, fmt.Errorf("record snapshot: %w", err))
		return
	}
	writeJSON(w, 201, snap)
}

// handleRestore boots a brand-new sandbox from a snapshot by loading its memory
// + device state and resuming — skipping kernel boot, init, and agent startup.
// The new sandbox reuses the snapshot's tap and guest IP (baked into the
// snapshot) and is allocated a fresh host port.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	snapID := r.PathValue("id")

	snap, err := s.reg.GetSnapshot(ctx, snapID)
	if err != nil {
		httpError(w, 404, fmt.Errorf("snapshot %s not found", snapID))
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
	go func(id string) {
		err := vm.Wait(context.Background(), m)
		fmt.Fprintf(os.Stderr, "[%s] restored VM exited: %v\n", id, err)
	}(id)

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

// handleDeleteSnapshot removes a snapshot's row and its artifact files.
func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.reg.DeleteSnapshot(r.Context(), id); err != nil {
		httpError(w, 404, err)
		return
	}
	_ = s.cfg.Provisioner.CleanupSnapshot(id)
	w.WriteHeader(http.StatusNoContent)
}
