package server

// Cross-host adoption claims shared ownership before reconstructing and waking
// the checkpoint. Eligible releases retain a peer generation and upload its
// backup asynchronously; other releases wait for the ordinary durable record.
//
// Chunked durable records may use the UFFD clone backend and fault memory from
// the captured manifest. Legacy diff records remain file-backed: an overlay is
// normalized during new publication but old records still need a local rebase.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"time"

	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

// releaseDurableWait bounds an explicit durability wait. An asynchronous
// handoff may already be offered when this wait expires.
const releaseDurableWait = 90 * time.Second

// fetchHibRecord pulls a hibernated sandbox's durable record (the cross-host
// commit marker). Absent → the sandbox was never made durable (diff-only on an
// old build, or an upload that never finished) and is not adoptable.
func (s *Server) fetchHibRecord(ctx context.Context, id string) (*hibRecord, error) {
	// A record this host has already invalidated is stale by construction: the
	// sandbox was woken, adopted, or destroyed here and only the object-store
	// delete is outstanding (drainHibInvalidations retries it). Refusing it is
	// what keeps a deferred delete from resurrecting a dead generation — both on
	// the adopt side and on the release side, which drops the local copy on the
	// strength of a record it sees in GCS.
	if s.hibNotAdoptable(id) {
		return nil, fmt.Errorf("durable record for %s is marked stale on this host (invalidation pending)", id)
	}
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
	load := newChunkLoad(m, s.memoryChunkCache(), fetch)
	return materializeMemoryChunks(dest, m, load)
}

func materializeMemoryChunks(dest string, m *chunkManifest, load func(uint64) ([]byte, error)) error {
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

type stagedHibernation struct {
	MemPath   string
	StatePath string
	Chunks    *vm.UFFDChunkSource
	Peer      *hibPeerSource
	hydration *peerHydrationTask
}

func (s *stagedHibernation) takeChunks() *vm.UFFDChunkSource {
	chunks := s.Chunks
	s.Chunks = nil
	return chunks
}

func (s *stagedHibernation) takeHydration() *peerHydrationTask {
	hydration := s.hydration
	s.hydration = nil
	return hydration
}

// Close releases every source that staging still owns. A source removed with a
// take method belongs to its consumer and is deliberately skipped here.
func (s *stagedHibernation) Close() error {
	var err error
	if chunks := s.takeChunks(); chunks != nil && chunks.Close != nil {
		err = errors.Join(err, chunks.Close())
	}
	if hydration := s.takeHydration(); hydration != nil {
		err = errors.Join(err, hydration.Close())
	}
	return err
}

// reconstructHibArtifacts stages state and rootfs from the retained peer or GCS,
// returning either a lazy chunk source or a complete memory file. rootfsPath
// must match the identity-neutral path baked into the Firecracker state.
func (s *Server) reconstructHibArtifacts(ctx context.Context, rec *hibRecord, rootfsPath string) (stagedHibernation, error) {
	if rec.Peer != nil && rec.MemForm == memFormChunked {
		staged, err := s.reconstructPeerHibernation(ctx, rec, rootfsPath)
		if err == nil {
			return staged, nil
		}
		s.met.hibPeerFallbacks.Add(1)
		fmt.Fprintf(os.Stderr, "[%s] peer reconstruction unavailable (%v); using GCS\n", rec.ID, err)
		// Peer paths may contain partial sparse overlays. Cloud staging starts
		// clean, especially for holes absent from the durable stream.
		_ = s.cfg.Provisioner.CleanupSnapshot(hibID(rec.ID))
		_ = os.Remove(rootfsPath)
	}
	id := rec.ID
	memPath, statePath, _, err := s.cfg.Provisioner.SnapshotPaths(hibID(id))
	if err != nil {
		return stagedHibernation{}, err
	}

	// State.
	if err := s.blob.GetSparse(ctx, hibStateObj(id), statePath); err != nil {
		return stagedHibernation{}, fmt.Errorf("pull state: %w", err)
	}

	// Rootfs: reflink the base and overlay the diff extents, or pull the whole
	// sparse image for a cold-boot.
	if rec.RootfsForm == rootfsFormDiff && rec.RootfsBaseID != "" {
		baseRootfs, berr := s.ensureBaseRootfsLocal(ctx, rec.RootfsBaseID)
		if berr != nil {
			return stagedHibernation{}, fmt.Errorf("pull rootfs base %s: %w", rec.RootfsBaseID, berr)
		}
		if err := provisioner.CloneFile(baseRootfs, rootfsPath); err != nil {
			return stagedHibernation{}, fmt.Errorf("stage base rootfs: %w", err)
		}
		if err := s.blob.GetSparse(ctx, hibRootfsObj(id), rootfsPath); err != nil {
			return stagedHibernation{}, fmt.Errorf("overlay rootfs diff: %w", err)
		}
	} else {
		if err := s.blob.GetSparse(ctx, hibRootfsObj(id), rootfsPath); err != nil {
			return stagedHibernation{}, fmt.Errorf("pull rootfs: %w", err)
		}
	}

	// A committed chunked record may start lazily; legacy diff records retain
	// the eager rebase fallback because their overlay is not a fault source.
	if rec.MemForm == memFormChunked && s.cfg.UFFDRestore {
		chunks, err := s.loadHibChunkSource(ctx, id)
		if err != nil {
			return stagedHibernation{}, fmt.Errorf("load hibernation chunks: %w", err)
		}
		pending := stagedHibernation{Chunks: chunks}
		if chunks.Total == 0 || chunks.Total%(1<<20) != 0 || chunks.Total>>20 > uint64(math.MaxInt64) {
			return stagedHibernation{}, errors.Join(
				fmt.Errorf("invalid hibernation chunk memory size %d", chunks.Total),
				pending.Close(),
			)
		}
		actualMIB := int64(chunks.Total >> 20)
		if rec.MemMIB == 0 {
			rec.MemMIB = actualMIB
		}
		if rec.MemMIB < 0 || rec.MemMIB != actualMIB {
			return stagedHibernation{}, errors.Join(
				fmt.Errorf("hibernation chunk memory %d does not match record %d MiB", chunks.Total, rec.MemMIB),
				pending.Close(),
			)
		}
		return stagedHibernation{StatePath: statePath, Chunks: pending.takeChunks()}, nil
	}
	// Mem: assemble chunks (full), or pull + rebase the diff onto the base.
	if rec.MemForm == memFormDiff {
		if err := s.blob.GetSparse(ctx, hibMemDiffObj(id), memPath); err != nil {
			return stagedHibernation{}, fmt.Errorf("pull diff mem: %w", err)
		}
		full, merr := s.materializeHibMem(ctx, memPath, rec.MemBaseID)
		if merr != nil {
			return stagedHibernation{}, fmt.Errorf("rebase diff mem: %w", merr)
		}
		memPath = full
	} else {
		if err := s.materializeChunkedMem(ctx, id, memPath); err != nil {
			return stagedHibernation{}, fmt.Errorf("assemble chunked mem: %w", err)
		}
	}
	return stagedHibernation{MemPath: memPath, StatePath: statePath}, nil
}

// handleAdopt reconstructs a durable hibernated sandbox on this host and wakes
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

	claim, err := s.claimHandoff(ctx, id)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrOwnerContended) {
			status = http.StatusServiceUnavailable
		}
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		httpError(w, status, fmt.Errorf("claim adoption %s: %w", id, err))
		return
	}
	sb, err := s.adoptWithClaim(ctx, &claim.Control.Record, claim)
	if err != nil {
		err = s.finishFailedAdoption(claim, err)
		capacityOrHTTPError(w, 500, fmt.Errorf("adopt %s: %w", id, err))
		return
	}
	writeJSON(w, http.StatusCreated, s.effectiveResources(sb))
}

func (s *Server) finishFailedAdoption(claim *handoffClaim, attemptErr error) error {
	if attemptErr == nil || claim == nil {
		return attemptErr
	}
	id := claim.Control.Record.ID
	if _, live := s.machines.Load(id); live {
		return attemptErr
	}
	recoveryCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.reg.Get(recoveryCtx, id); !errors.Is(err, sql.ErrNoRows) {
		return attemptErr
	}
	if err := s.reopenHandoff(recoveryCtx, claim); err != nil {
		return errors.Join(attemptErr, fmt.Errorf("reopen failed adoption: %w", err))
	}
	return attemptErr
}

// adopt does the reconstruct + local-row insert + clone wake. Caller holds the
// wake lock and the owner fence. On any post-insert failure it rolls the local
// state back (GCS + fence untouched, so a retry — here or elsewhere — still
// works).
func (s *Server) adopt(ctx context.Context, rec *hibRecord) (registry.Sandbox, error) {
	return s.adoptWithClaim(ctx, rec, nil)
}

func (s *Server) adoptWithClaim(ctx context.Context, rec *hibRecord, claim *handoffClaim) (result registry.Sandbox, retErr error) {
	id := rec.ID
	rootfsPath := s.cfg.Provisioner.RootfsPathFor(id)
	var staged stagedHibernation
	var err error
	if claim != nil && claim.Control.Descriptor != nil {
		staged, err = s.reconstructHandoff(ctx, claim.Control.Descriptor, rootfsPath)
	} else {
		staged, err = s.reconstructHibArtifacts(ctx, rec, rootfsPath)
	}
	if err != nil {
		_ = s.cfg.Provisioner.RemoveRootfs(rootfsPath)
		_ = s.cfg.Provisioner.CleanupSnapshot(hibID(id))
		return registry.Sandbox{}, err
	}
	defer func() {
		retErr = errors.Join(retErr, staged.Close())
	}()

	var expiresAt *time.Time
	if rec.ExpiresAtUnix != nil {
		t := time.Unix(*rec.ExpiresAtUnix, 0)
		expiresAt = &t
	}
	// Fresh identity (tap/IP) from THIS host's pools; keep the sandbox's id,
	// name, resources, expiry, and hibernate window.
	sb, err := s.reg.CreateStarting(ctx, id, rec.Name, rootfsPath, expiresAt, rec.BaseSnapshotID, rec.HibernateAfterSec, rec.Vcpus, rec.MemMIB)
	if err != nil {
		_ = s.cfg.Provisioner.RemoveRootfs(rootfsPath)
		_ = s.cfg.Provisioner.CleanupSnapshot(hibID(id))
		return registry.Sandbox{}, fmt.Errorf("insert adopted row: %w", err)
	}
	// Continue the sandbox's billable-interval numbering rather than restarting
	// it, so the line items on a bill stay unique across the move.
	if rec.UsageSeq > 0 {
		if perr := s.reg.SetUsageSeqFloor(ctx, id, rec.UsageSeq); perr != nil {
			fmt.Fprintf(os.Stderr, "[%s] adopt: carry usage sequence %d: %v\n", id, rec.UsageSeq, perr)
		}
	}
	// Restore the labels and provenance the sandbox carried on its old host.
	// Best-effort: an adopted sandbox that has lost its labels is still a
	// working sandbox, but it bills unattributably, so this is worth a loud
	// line rather than a failed adoption.
	if len(rec.Metadata) > 0 || rec.SourceType != "" || rec.SourceID != "" {
		if _, perr := s.reg.SetPublicFields(ctx, id, rec.SourceType, rec.SourceID, rec.Metadata); perr != nil {
			fmt.Fprintf(os.Stderr, "[%s] adopt: restore labels and provenance: %v\n", id, perr)
		} else {
			sb.Metadata, sb.SourceType, sb.SourceID = rec.Metadata, rec.SourceType, rec.SourceID
		}
	}
	// Re-expose the sandbox's explicit ports (fresh host ports from this pool).
	for _, gp := range append(rec.GuestPorts, rec.LegacyGuestPorts...) {
		if _, perr := s.reg.AddPort(ctx, id, gp); perr != nil {
			fmt.Fprintf(os.Stderr, "[%s] adopt: re-expose guest port %d: %v\n", id, gp, perr)
		}
	}
	for _, gp := range rec.URLGuestPorts {
		if _, perr := s.reg.AddURLPort(ctx, id, gp); perr != nil {
			fmt.Fprintf(os.Stderr, "[%s] adopt: re-expose URL guest port %d: %v\n", id, gp, perr)
		}
	}
	for _, raw := range rec.PublicPorts {
		if _, perr := s.reg.AddURLPort(ctx, id, raw.GuestPort); perr != nil {
			fmt.Fprintf(os.Stderr, "[%s] adopt: re-expose raw guest port %d: %v\n", id, raw.GuestPort, perr)
			continue
		}
		if perr := s.reg.SetPublicPort(ctx, id, raw.GuestPort, raw.PublicPort); perr != nil {
			fmt.Fprintf(os.Stderr, "[%s] adopt: restore public port %d: %v\n", id, raw.PublicPort, perr)
		}
	}

	// Clone-path wake (fresh identity: unbridged tap, MMDS reidentify, GARP),
	// File backend off the reconstructed local mem.
	if claim != nil {
		if err := s.authorizeHandoffRun(ctx, claim); err != nil {
			return registry.Sandbox{}, errors.Join(err, s.adoptRollback(sb))
		}
	}
	lazy := staged.Chunks != nil
	if err := s.wakeClone(ctx, sb, &staged); err != nil {
		return registry.Sandbox{}, errors.Join(fmt.Errorf("clone wake: %w", err), s.adoptRollback(sb))
	}
	sb.Status = registry.StatusRunning
	if lazy {
		s.clearHibernationLineage(id)
	}
	// Open the restored explicit-port listeners.
	if serr := s.syncSandboxPorts(ctx, sb); serr != nil {
		fmt.Fprintf(os.Stderr, "[%s] adopt: sync port listeners: %v\n", id, serr)
	}
	// The frozen generation has become a live, mutable VM. Remove its durable
	// commit marker so a later route miss or host failure cannot resurrect the
	// old checkpoint as a second copy.
	if err := s.invalidateHibernationRecord(ctx, id); err != nil {
		return registry.Sandbox{}, errors.Join(err, s.adoptRollback(sb))
	}
	// The reconstructed local mem/state were consumed into the live VM; drop them.
	_ = s.cfg.Provisioner.CleanupSnapshot(hibID(id))
	s.act.touch(id)
	if hydration := staged.takeHydration(); hydration != nil {
		s.startPeerHydration(id, hydration)
	}
	fmt.Fprintf(os.Stderr, "[%s] adopted onto this host (mem=%s rootfs=%s)\n", id, rec.MemForm, rec.RootfsForm)
	return sb, nil
}

// adoptRollback removes the half-adopted local state after a failed clone wake.
// GCS artifacts and the owner fence are left intact — the sandbox stays
// adoptable (by a retry here, or another host). Caller holds the wake lock.
func (s *Server) adoptRollback(sb registry.Sandbox) error {
	if v, ok := s.machines.Load(sb.ID); ok {
		m := v.(*vm.Machine)
		vm.PreserveFailureLog(m)
		_ = vm.StopForce(m)
		if stopped, err := waitMachine(func(ctx context.Context) error { return vm.Wait(ctx, m) }, forcedExitGrace); !stopped {
			return fmt.Errorf("handoff VM exit unconfirmed: %w", err)
		}
		s.machines.Delete(sb.ID)
	}
	s.pf.CloseSandbox(sb.ID)
	_ = s.cfg.Provisioner.DeleteTap(sb.TapDevice)
	_ = s.cfg.Provisioner.CleanupSnapshot(hibID(sb.ID))
	_ = s.cfg.Provisioner.RemoveRootfs(sb.RootfsPath)
	if err := s.reg.Destroy(context.Background(), sb.ID); err != nil {
		return fmt.Errorf("adopt rollback: destroy row: %w", err)
	}
	return nil
}

// handleRelease is the drain source side: freeze the sandbox if running, confirm
// it is durable in GCS, then drop the LOCAL row + artifacts (GCS untouched) so a
// target host can adopt it. If durability can't be confirmed, it aborts and
// keeps everything local — a sandbox is never dropped locally before it is
// safely reconstructable elsewhere.
func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if s.canHandoff() {
		s.handleHandoffRelease(w, r)
		return
	}
	if s.blob == nil {
		httpError(w, http.StatusBadRequest, errors.New("release requires a snapshot bucket"))
		return
	}
	sb, err := s.reg.Get(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) && s.hasRetainedHibernation(id) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpError(w, http.StatusNotFound, err)
		return
	}

	// Freeze first (hibernate takes the wake lock itself; don't hold it across).
	// Strict durability: this is the one caller that goes on to DROP the local
	// copy because it saw a record.json, so a stale one it failed to delete must
	// abort the release rather than be frozen over.
	if sb.Status == registry.StatusRunning {
		if err := s.hibernateForRelease(ctx, id); err != nil {
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

	peer, err := s.releaseHibernation(ctx, sb)
	if err != nil {
		httpError(w, 500, err)
		return
	}
	if peer != nil {
		w.Header().Set("X-Sandbox-Hibernation-Generation", peer.Generation)
	}
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
