package server

// Cross-host adopt / release (roadmap B4b). These are the host-side halves the
// gateway drives (B4c) to wake a hibernated sandbox on a host that never created
// it — dead-host recovery, or a drain off a live host.
//
//   POST /sandboxes/{id}/adopt    reconstruct from GCS (record + state + rootfs +
//                                 mem) under a fresh local identity, CAS the owner
//                                 fence, and wake via the clone path.
//   POST /sandboxes/{id}/release  freeze if running, confirm the sandbox is
//                                 durable in GCS, then drop the LOCAL row +
//                                 artifacts (GCS untouched) so another host can
//                                 adopt it. The drain source side.
//
// B4b uses the File backend for the adopt wake: it materializes the full mem
// image locally (assemble chunks, or rebase a diff) before the clone resume. The
// LAZY UFFD-chunk clone wake — resume-then-stream, the actual cross-host perf win
// — needs a vm.StartCloneUFFD and lands with the B4c measurement.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

// releaseDurableWait bounds how long /release waits for a just-frozen sandbox's
// background durability upload to publish its record.json commit marker before
// giving up (and keeping the sandbox local, safely un-adoptable).
const releaseDurableWait = 90 * time.Second

// fetchHibRecord pulls a hibernated sandbox's durable record (the cross-host
// commit marker). Absent → the sandbox was never made durable (diff-only on an
// old build, or an upload that never finished) and is not adoptable.
func (s *Server) fetchHibRecord(ctx context.Context, id string) (*hibRecord, error) {
	b, err := s.blob.GetBytes(ctx, hibRecordObj(id))
	if err != nil {
		return nil, err
	}
	var rec hibRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("decode hib record: %w", err)
	}
	if rec.ID != id {
		return nil, fmt.Errorf("hib record id %q does not match %q", rec.ID, id)
	}
	return &rec, nil
}

// materializeChunkedMem reassembles a full mem image from its GCS chunks into
// dest (zero chunks stay holes). This is the File-backend cost the chunk source
// exists to avoid; B4b pays it for correctness, B4c's lazy path removes it.
func (s *Server) materializeChunkedMem(ctx context.Context, id, dest string) error {
	m, err := s.fetchChunkManifest(ctx, id)
	if err != nil {
		return fmt.Errorf("fetch chunk manifest: %w", err)
	}
	fetch := func(hash string) ([]byte, error) {
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return s.blob.GetBytes(fctx, chunkObj(hash))
	}
	load := newChunkLoad(m, s.chunkCacheDir(), fetch)
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(int64(m.MemSize)); err != nil {
		return err
	}
	for idx := uint64(0); idx < uint64(len(m.Chunks)); idx++ {
		if m.Chunks[idx].Hash == chunkZeroHash {
			continue // hole: reads back as zeros from the truncated file
		}
		raw, err := load(idx)
		if err != nil {
			return fmt.Errorf("materialize chunk %d: %w", idx, err)
		}
		if _, err := f.WriteAt(raw, int64(idx*m.ChunkSize)); err != nil {
			return err
		}
	}
	return f.Sync()
}

// reconstructHibArtifacts stages a hibernated sandbox's mem, state, and rootfs
// on THIS host from GCS, returning the local full-mem and state paths the clone
// wake loads. rootfsPath must already be the identity-neutral per-sandbox path
// the FC state baked (RootfsPathFor(id) — fleet-wide config makes it match).
func (s *Server) reconstructHibArtifacts(ctx context.Context, rec *hibRecord, rootfsPath string) (memPath, statePath string, err error) {
	id := rec.ID
	memPath, statePath, _, err = s.cfg.Provisioner.SnapshotPaths(hibID(id))
	if err != nil {
		return "", "", err
	}

	// State.
	if err := s.blob.GetSparse(ctx, hibStateObj(id), statePath); err != nil {
		return "", "", fmt.Errorf("pull state: %w", err)
	}

	// Rootfs: reflink the base and overlay the diff extents, or pull the whole
	// sparse image for a cold-boot.
	if rec.RootfsForm == rootfsFormDiff && rec.RootfsBaseID != "" {
		_, baseRootfs, berr := s.ensureBaseLocal(ctx, rec.RootfsBaseID)
		if berr != nil {
			return "", "", fmt.Errorf("pull rootfs base %s: %w", rec.RootfsBaseID, berr)
		}
		if err := provisioner.CloneFile(baseRootfs, rootfsPath); err != nil {
			return "", "", fmt.Errorf("stage base rootfs: %w", err)
		}
		if err := s.blob.GetSparse(ctx, hibRootfsObj(id), rootfsPath); err != nil {
			return "", "", fmt.Errorf("overlay rootfs diff: %w", err)
		}
	} else {
		if err := s.blob.GetSparse(ctx, hibRootfsObj(id), rootfsPath); err != nil {
			return "", "", fmt.Errorf("pull rootfs: %w", err)
		}
	}

	// Mem: assemble chunks (full), or pull + rebase the diff onto the base.
	if rec.MemForm == memFormDiff {
		if err := s.blob.GetSparse(ctx, hibMemDiffObj(id), memPath); err != nil {
			return "", "", fmt.Errorf("pull diff mem: %w", err)
		}
		full, merr := s.materializeHibMem(ctx, memPath, rec.MemBaseID)
		if merr != nil {
			return "", "", fmt.Errorf("rebase diff mem: %w", merr)
		}
		memPath = full
	} else {
		if err := s.materializeChunkedMem(ctx, id, memPath); err != nil {
			return "", "", fmt.Errorf("assemble chunked mem: %w", err)
		}
	}
	return memPath, statePath, nil
}

// handleAdopt reconstructs a hibernated sandbox from GCS on this host and wakes
// it under a fresh local identity. Dispatched by the gateway on a route miss
// (owner gone) or a drain. Idempotent: a sandbox already local is served by the
// normal path.
func (s *Server) handleAdopt(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if s.blob == nil {
		httpError(w, http.StatusBadRequest, errors.New("adopt requires a snapshot bucket"))
		return
	}

	// Already here? A local running row is done; a local hibernated row is a
	// plain local wake, not a cross-host adopt.
	if sb, err := s.reg.Get(ctx, id); err == nil {
		switch sb.Status {
		case registry.StatusRunning:
			writeJSON(w, http.StatusOK, s.effectiveResources(sb))
			return
		case registry.StatusHibernated:
			woken, werr := s.wake(ctx, id)
			if werr != nil {
				httpError(w, 500, werr)
				return
			}
			writeJSON(w, http.StatusOK, s.effectiveResources(woken))
			return
		}
	}

	mu := s.wakeLock(id)
	mu.Lock()
	defer mu.Unlock()
	// Re-check under the lock — a concurrent adopt/wake may have landed the row.
	if sb, err := s.reg.Get(ctx, id); err == nil && sb.Status == registry.StatusRunning {
		writeJSON(w, http.StatusOK, s.effectiveResources(sb))
		return
	}

	if err := s.acquireCreate(ctx); err != nil {
		httpError(w, 499, fmt.Errorf("cancelled while queued for create slot: %w", err))
		return
	}
	defer s.releaseCreate()

	rec, err := s.fetchHibRecord(ctx, id)
	if err != nil {
		httpError(w, http.StatusNotFound, fmt.Errorf("sandbox %s not adoptable: %w", id, err))
		return
	}

	// Claim ownership before any local work — a lost CAS means another host is
	// adopting concurrently; back off and let it win.
	if _, err := s.acquireOwner(ctx, id); err != nil {
		if errors.Is(err, ErrOwnerContended) {
			w.Header().Set("Retry-After", "2")
			httpError(w, http.StatusServiceUnavailable, err)
			return
		}
		httpError(w, 500, fmt.Errorf("claim ownership: %w", err))
		return
	}

	sb, err := s.adopt(ctx, rec)
	if err != nil {
		capacityOrHTTPError(w, 500, fmt.Errorf("adopt %s: %w", id, err))
		return
	}
	writeJSON(w, http.StatusCreated, s.effectiveResources(sb))
}

// adopt does the reconstruct + local-row insert + clone wake. Caller holds the
// wake lock and the owner fence. On any post-insert failure it rolls the local
// state back (GCS + fence untouched, so a retry — here or elsewhere — still
// works).
func (s *Server) adopt(ctx context.Context, rec *hibRecord) (registry.Sandbox, error) {
	id := rec.ID
	rootfsPath := s.cfg.Provisioner.RootfsPathFor(id)
	memPath, statePath, err := s.reconstructHibArtifacts(ctx, rec, rootfsPath)
	if err != nil {
		_ = s.cfg.Provisioner.RemoveRootfs(rootfsPath)
		_ = s.cfg.Provisioner.CleanupSnapshot(hibID(id))
		return registry.Sandbox{}, err
	}

	var expiresAt *time.Time
	if rec.ExpiresAtUnix != nil {
		t := time.Unix(*rec.ExpiresAtUnix, 0)
		expiresAt = &t
	}
	// Fresh identity (tap/IP/port) from THIS host's pools; keep the sandbox's id,
	// name, resources, expiry, and hibernate window.
	sb, err := s.reg.Create(ctx, id, rec.Name, rootfsPath, expiresAt, rec.BaseSnapshotID, rec.HibernateAfterSec, rec.Vcpus, rec.MemMIB)
	if err != nil {
		_ = s.cfg.Provisioner.RemoveRootfs(rootfsPath)
		_ = s.cfg.Provisioner.CleanupSnapshot(hibID(id))
		return registry.Sandbox{}, fmt.Errorf("insert adopted row: %w", err)
	}
	// Re-expose the sandbox's extra ports (fresh host ports from this pool).
	for _, gp := range rec.ExtraGuestPorts {
		if _, perr := s.reg.AddPort(ctx, id, gp); perr != nil {
			fmt.Fprintf(os.Stderr, "[%s] adopt: re-expose guest port %d: %v\n", id, gp, perr)
		}
	}

	// Clone-path wake (fresh identity: unbridged tap, MMDS reidentify, GARP),
	// File backend off the reconstructed local mem.
	if err := s.wakeClone(ctx, sb, memPath, statePath); err != nil {
		s.adoptRollback(sb)
		return registry.Sandbox{}, fmt.Errorf("clone wake: %w", err)
	}
	// Primary port opened by finishClone; open the extra-port listeners too.
	if serr := s.syncSandboxPorts(ctx, sb); serr != nil {
		fmt.Fprintf(os.Stderr, "[%s] adopt: sync port listeners: %v\n", id, serr)
	}
	// The reconstructed local mem/state were consumed into the live VM; drop them.
	_ = s.cfg.Provisioner.CleanupSnapshot(hibID(id))
	s.act.touch(id)
	fmt.Fprintf(os.Stderr, "[%s] adopted onto this host (mem=%s rootfs=%s)\n", id, rec.MemForm, rec.RootfsForm)
	return sb, nil
}

// adoptRollback removes the half-adopted local state after a failed clone wake.
// GCS artifacts and the owner fence are left intact — the sandbox stays
// adoptable (by a retry here, or another host). Caller holds the wake lock.
func (s *Server) adoptRollback(sb registry.Sandbox) {
	if v, ok := s.machines.LoadAndDelete(sb.ID); ok {
		_ = vm.StopForce(v.(*vm.Machine))
	}
	s.pf.CloseSandbox(sb.ID)
	_ = s.cfg.Provisioner.DeleteTap(sb.TapDevice)
	_ = s.cfg.Provisioner.CleanupSnapshot(hibID(sb.ID))
	_ = s.cfg.Provisioner.RemoveRootfs(sb.RootfsPath)
	if err := s.reg.Destroy(context.Background(), sb.ID); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] adopt rollback: destroy row: %v\n", sb.ID, err)
	}
}

// handleRelease is the drain source side: freeze the sandbox if running, confirm
// it is durable in GCS, then drop the LOCAL row + artifacts (GCS untouched) so a
// target host can adopt it. If durability can't be confirmed, it aborts and
// keeps everything local — a sandbox is never dropped locally before it is
// safely reconstructable elsewhere.
func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if s.blob == nil {
		httpError(w, http.StatusBadRequest, errors.New("release requires a snapshot bucket"))
		return
	}
	sb, err := s.reg.Get(ctx, id)
	if err != nil {
		httpError(w, http.StatusNotFound, err)
		return
	}

	// Freeze first (hibernate takes the wake lock itself; don't hold it across).
	if sb.Status == registry.StatusRunning {
		if err := s.hibernate(ctx, id, false); err != nil {
			httpError(w, http.StatusConflict, fmt.Errorf("freeze for release: %w", err))
			return
		}
	} else if sb.Status != registry.StatusHibernated {
		httpError(w, http.StatusConflict, fmt.Errorf("sandbox %s is %s, cannot release", id, sb.Status))
		return
	}

	mu := s.wakeLock(id)
	mu.Lock()
	defer mu.Unlock()
	sb, err = s.reg.Get(ctx, id)
	if err != nil {
		httpError(w, http.StatusNotFound, err)
		return
	}
	if sb.Status != registry.StatusHibernated {
		httpError(w, http.StatusConflict, fmt.Errorf("sandbox %s is %s, cannot release", id, sb.Status))
		return
	}

	// Confirm durability before dropping anything local: poll for the record.json
	// commit marker the freeze uploads in the background.
	if err := s.awaitDurable(ctx, id); err != nil {
		httpError(w, http.StatusServiceUnavailable, fmt.Errorf("not yet durable, keeping local: %w", err))
		return
	}

	// Drop the local row + artifacts (GCS untouched). Inlined rather than
	// s.destroy (which would re-take the wake lock we already hold).
	s.pf.CloseSandbox(id)
	_ = s.cfg.Provisioner.CleanupSnapshot(hibID(id))
	_ = s.cfg.Provisioner.RemoveRootfs(sb.RootfsPath)
	if err := s.reg.Destroy(ctx, id); err != nil {
		httpError(w, 500, fmt.Errorf("drop local row: %w", err))
		return
	}
	s.act.forget(id)
	fmt.Fprintf(os.Stderr, "[%s] released from this host (durable in GCS; adoptable elsewhere)\n", id)
	w.WriteHeader(http.StatusNoContent)
}

// awaitDurable blocks until id's record.json exists in GCS (the freeze's
// background upload finished) or releaseDurableWait elapses.
func (s *Server) awaitDurable(ctx context.Context, id string) error {
	deadline := time.Now().Add(releaseDurableWait)
	for {
		if _, err := s.fetchHibRecord(ctx, id); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("record.json not present after %s", releaseDurableWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
