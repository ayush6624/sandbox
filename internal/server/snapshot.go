package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
// Public restore/fanout use identity-neutral clone loading: the snapshot's
// baked network identity is replaced before its tap joins the bridge, so the
// source may still exist and its old tap/IP may be reused safely.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	// The body is optional (older clients send none); tolerate EOF.
	var body struct {
		Name             string `json:"name"`
		RetentionSeconds int    `json:"retention_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}
	if err := validateName(body.Name); err != nil {
		httpError(w, 400, err)
		return
	}
	if body.RetentionSeconds < 0 {
		httpError(w, 400, errors.New("retention_seconds must be non-negative"))
		return
	}
	var expiresAt *time.Time
	if body.RetentionSeconds > 0 {
		value := time.Now().Add(time.Duration(body.RetentionSeconds) * time.Second)
		expiresAt = &value
	}
	snap, status, err := s.snapshotSandbox(r.Context(), r.PathValue("id"), false, body.Name, expiresAt)
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
func (s *Server) snapshotSandbox(ctx context.Context, id string, golden bool, name string, expiresAt *time.Time) (registry.Snapshot, int, error) {
	role := registry.SnapshotRoleUser
	if golden {
		role = registry.SnapshotRoleBuiltin
	}
	return s.snapshotSandboxWithRole(ctx, id, golden, role, name, expiresAt)
}

func (s *Server) snapshotSandboxWithRole(ctx context.Context, id string, golden bool, role, name string, expiresAt *time.Time) (registry.Snapshot, int, error) {
	sb, err := s.reg.Get(ctx, id)
	if err != nil {
		return registry.Snapshot{}, 404, err
	}
	// Mark a valid user sandbox busy before taking its lifecycle lock. This
	// makes an idle-hibernation scan that has not committed to the freeze yet
	// back off. Keep the busy mark until after the lock is released (defer
	// ordering below is intentional).
	var done func()
	forgetActivity := false
	if !golden {
		done = s.act.begin(id)
		defer func() {
			done()
			// A concurrent destroy may have removed a row that existed at the
			// first read while this request waited for the lifecycle lock.
			if forgetActivity {
				s.act.forget(id)
			}
		}()
	}
	// Snapshot, hibernate, wake, and destroy all mutate the same VMM lifecycle.
	// Serialize them so a reaper cannot kill Firecracker while snapshot creation
	// is blocked in Pause or /snapshot/create.
	lifecycle := s.wakeLock(id)
	lifecycle.Lock()
	defer lifecycle.Unlock()

	// The row may have changed while this request waited for an in-progress
	// lifecycle operation. Re-read it under the lock and fail clearly instead
	// of operating on a stale Machine pointer.
	sb, err = s.reg.Get(ctx, id)
	if err != nil {
		forgetActivity = !golden
		return registry.Snapshot{}, 404, err
	}
	if sb.Status != registry.StatusRunning {
		return registry.Snapshot{}, 409, fmt.Errorf("sandbox %s is %s, not running", id, sb.Status)
	}
	v, ok := s.machines.Load(id)
	if !ok {
		return registry.Snapshot{}, 409, fmt.Errorf("sandbox %s is not running in this server", id)
	}
	m := v.(*vm.Machine)
	guestMAC, macErr := s.cfg.Provisioner.GuestMAC(sb.GuestIP)
	if macErr != nil {
		// Legacy/cold paths can lack a resolved neighbor. The snapshot remains
		// correct; its later restore simply uses normal ARP discovery.
		fmt.Fprintf(os.Stderr, "[%s] snapshot: record guest MAC: %v\n", id, macErr)
	}

	format, baseID := registry.FormatFull, ""
	var parentFullPath, goldenMemPath string
	var baseOp *keyedMutex
	usingHibLineage := false
	// Diff only while Server.diffBase vouches for the machine's bitmap. After
	// every successful Firecracker snapshot the bitmap resets, so the new
	// snapshot becomes the next parent. User-snapshot parents are cumulative
	// diffs over the immutable golden: after capture we merge the new layer
	// into that cumulative diff, keeping every public snapshot one level deep
	// (golden <- user snapshot) rather than creating distributed dependency
	// chains that make deletion and cross-host restore fragile.
	if v, ok := s.diffBase.Load(id); ok && !golden && vm.DiffCapable(m) {
		candidateID := v.(string)
		// Shared: the parent is only read here, so this fences its deletion
		// without blocking concurrent restores/fanouts of the same parent.
		candidateOp := s.snapshotLock(candidateID)
		candidateOp.RLock()
		if plan, ok := s.snapshotDiffPlan(ctx, candidateID); ok {
			if plan.parent.ID != "" {
				parentFullPath, err = s.materializeMem(ctx, plan.parent)
				ok = err == nil
			}
			if ok {
				format, baseID = registry.FormatDiff, plan.goldenID
				goldenMemPath = plan.goldenMemPath
				baseOp = candidateOp
			} else {
				candidateOp.RUnlock()
			}
		} else {
			candidateOp.RUnlock()
		}
	}
	// A machine woken from a differential hibernation has no public snapshot
	// row to name as diffBase. Its private full-memory reflink is nevertheless
	// the exact baseline of Firecracker's bitmap, so compose the new layer onto
	// it and publish the result as another one-level delta over the golden.
	if format == registry.FormatFull && !golden && vm.DiffCapable(m) {
		if v, ok := s.hibLineage.Load(id); ok {
			lineage := v.(hibernationLineage)
			candidateOp := s.snapshotLock(lineage.goldenID)
			candidateOp.RLock()
			if _, statErr := os.Stat(lineage.parentFullMem); statErr == nil {
				if goldenMem, _, baseErr := s.ensureBaseLocal(ctx, lineage.goldenID); baseErr == nil {
					format, baseID = registry.FormatDiff, lineage.goldenID
					parentFullPath = lineage.parentFullMem
					goldenMemPath = goldenMem
					baseOp = candidateOp
					usingHibLineage = true
				} else {
					candidateOp.RUnlock()
				}
			} else {
				candidateOp.RUnlock()
			}
		}
	}
	if baseOp != nil {
		defer baseOp.RUnlock()
	}
	snapType := vm.SnapshotFull
	if format == registry.FormatDiff {
		snapType = vm.SnapshotDiff
	}

	snapID := uuid.NewString()
	// Publish the id under its operation lock before creating any artifact or
	// row. A delete that discovers the row cannot overtake uploader
	// registration and later have meta.json resurrected behind it.
	snapshotOp := s.snapshotLock(snapID)
	snapshotOp.Lock()
	defer snapshotOp.Unlock()
	memPath, statePath, rootfsPath, err := s.cfg.Provisioner.SnapshotPaths(snapID)
	if err != nil {
		return registry.Snapshot{}, 500, fmt.Errorf("snapshot dir: %w", err)
	}

	// Resume on every exit path so a failed snapshot doesn't leave the source
	// sandbox frozen.
	setGuestSnapshotPoll(ctx, sb.GuestIP, true)
	if err := vm.Pause(ctx, m); err != nil {
		setGuestSnapshotPoll(context.Background(), sb.GuestIP, false)
		return registry.Snapshot{}, 500, fmt.Errorf("pause: %w", err)
	}
	resumed := false
	resume := func() {
		if !resumed {
			resumed = true
			if err := vm.Resume(context.Background(), m); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] resume after snapshot failed: %v\n", id, err)
			}
			// A golden source is destroyed immediately below. Do not disarm it:
			// Firecracker can resume against the snapshot's memory backing, so
			// writing false here can leak the normal polling state back into
			// the supposedly armed golden snapshot.
			if !golden {
				setGuestSnapshotPoll(context.Background(), sb.GuestIP, false)
			}
		}
	}
	defer resume()

	t0 := time.Now()
	// Once Firecracker is asked to snapshot, its dirty bitmap is either reset
	// or indeterminate. Keep a lineage used for flattening alive until this
	// request returns, then retire it; any unrelated stale lineage can go now.
	if usingHibLineage {
		defer s.clearHibernationLineage(id)
	} else {
		s.clearHibernationLineage(id)
	}
	err = vm.Snapshot(ctx, m, memPath, statePath, snapType)
	if err != nil {
		// Firecracker documents a failed snapshot as side-effect free, but the
		// wrapper can also fail while publishing jailed outputs or restoring
		// cgroup policy after Firecracker succeeded. Drop the lineage rather
		// than risk treating an indeterminate bitmap as a valid delta.
		s.diffBase.Delete(id)
		_ = s.cfg.Provisioner.CleanupSnapshot(snapID)
		return registry.Snapshot{}, 500, fmt.Errorf("create snapshot: %w", err)
	}
	// Firecracker reset the bitmap to the state represented by snapID. Until
	// the row and all artifacts commit below, no valid next parent exists.
	s.diffBase.Delete(id)
	// Copy the rootfs while the VM is paused so the disk matches the snapshot's
	// view of it. The source keeps writing to its own rootfs after resume.
	if err := s.cfg.Provisioner.CopyFileSparse(sb.RootfsPath, rootfsPath); err != nil {
		_ = s.cfg.Provisioner.CleanupSnapshot(snapID)
		return registry.Snapshot{}, 500, fmt.Errorf("freeze rootfs: %w", err)
	}
	resume()
	// A clone of a user snapshot dirties pages relative to that user's
	// cumulative delta, not directly relative to the golden. Compose the two
	// sparse layers after resuming the guest so this host-side work does not
	// extend pause time. The result remains a single delta over baseID.
	if parentFullPath != "" {
		if err := s.flattenSnapshotDiff(parentFullPath, goldenMemPath, memPath); err != nil {
			_ = s.cfg.Provisioner.CleanupSnapshot(snapID)
			return registry.Snapshot{}, 500, fmt.Errorf("flatten snapshot diff: %w", err)
		}
	}
	fmt.Fprintf(os.Stderr, "[%s] snapshot %s written in %s\n", id, snapID, time.Since(t0).Round(time.Millisecond))

	// Stamp the base rootfs so a rebuilt base (e.g. install-agent) invalidates
	// a golden snapshot on the next startup.
	var baseMtime, baseSize int64
	if fi, err := os.Stat(s.cfg.Provisioner.RootfsBase); err == nil {
		baseMtime, baseSize = fi.ModTime().Unix(), fi.Size()
	}

	snap := registry.Snapshot{
		ID:               snapID,
		Name:             name,
		SourceID:         id,
		TapDevice:        sb.TapDevice,
		GuestIP:          sb.GuestIP,
		GuestMAC:         guestMAC,
		MemPath:          memPath,
		StatePath:        statePath,
		RootfsPath:       rootfsPath,
		SourceRootfsPath: sb.RootfsPath,
		CreatedAt:        time.Now(),
		ExpiresAt:        expiresAt,
		Golden:           golden,
		Role:             role,
		BaseMtime:        baseMtime,
		BaseSize:         baseSize,
		Format:           format,
		BaseID:           baseID,
		// The snapshot bakes the source's vcpus/mem; record its overrides so
		// restored/cloned rows report the resources they actually run with.
		Vcpus:  sb.Vcpus,
		MemMIB: sb.MemMIB,
	}
	if err := s.reg.CreateSnapshot(ctx, snap); err != nil {
		_ = s.cfg.Provisioner.CleanupSnapshot(snapID)
		return registry.Snapshot{}, 500, fmt.Errorf("record snapshot: %w", err)
	}
	if format == registry.FormatDiff {
		// The current Firecracker bitmap now tracks writes since this exact
		// snapshot. Keeping the lineage is what makes repeated snapshots of the
		// same sandbox differential too.
		s.diffBase.Store(id, snap.ID)
	}
	// Durability: ship the snapshot to GCS in the background. The caller gets
	// its 201 now; until meta.json lands the snapshot is host-local only.
	if !golden && s.blob != nil {
		s.startSnapshotUpload(snap)
	}
	return snap, 201, nil
}

type snapshotDiffPlan struct {
	goldenID      string
	parent        registry.Snapshot
	goldenMemPath string
}

// snapshotDiffPlan resolves the snapshot represented by Firecracker's current
// dirty bitmap into a one-level delta plan. A golden parent needs no
// composition. A user parent is accepted only when it is itself a one-level
// delta over an available immutable golden; its cumulative memory delta is
// merged with the new Firecracker layer after capture.
func (s *Server) snapshotDiffPlan(ctx context.Context, candidateID string) (snapshotDiffPlan, bool) {
	candidate, err := s.reg.GetSnapshot(ctx, candidateID)
	if err != nil {
		return snapshotDiffPlan{}, false
	}
	if candidate.Golden {
		return snapshotDiffPlan{goldenID: candidate.ID}, true
	}
	if candidate.Format != registry.FormatDiff || candidate.BaseID == "" {
		return snapshotDiffPlan{}, false
	}
	// A local row lets us prove the root is actually golden. On a host that
	// pulled the user snapshot, the golden has no registry row; ensureBaseLocal
	// verifies the immutable bases/<id>/complete object and cache instead.
	var goldenMem string
	if root, err := s.reg.GetSnapshot(ctx, candidate.BaseID); err == nil {
		if !root.Golden {
			return snapshotDiffPlan{}, false
		}
		if goldenMem, _, err = s.ensureBaseLocal(ctx, root.ID); err != nil {
			return snapshotDiffPlan{}, false
		}
	} else {
		if goldenMem, _, err = s.ensureBaseLocal(ctx, candidate.BaseID); err != nil {
			return snapshotDiffPlan{}, false
		}
	}
	return snapshotDiffPlan{
		goldenID:      candidate.BaseID,
		parent:        candidate,
		goldenMemPath: goldenMem,
	}, true
}

// flattenSnapshotDiff applies Firecracker's new layer to the exact full memory
// image it was based on, then derives a fresh one-level delta from the
// materialized result's XFS sharing with the immutable golden. We deliberately
// do not union sparse layer extents: an allocated zero-filled block is valid
// data, not an "unchanged" marker, and treating it otherwise corrupts memory.
func (s *Server) flattenSnapshotDiff(parentFullPath, goldenMemPath, layerPath string) error {
	fullPath := filepath.Join(filepath.Dir(layerPath), "mem.full.bin")
	tmpFull := fullPath + ".tmp"
	tmpDelta := layerPath + ".cumulative"
	_ = os.Remove(tmpFull)
	_ = os.Remove(tmpDelta)
	if err := provisioner.CloneFile(parentFullPath, tmpFull); err != nil {
		return fmt.Errorf("clone full parent: %w", err)
	}
	if err := s.cfg.Provisioner.OverlaySparse(layerPath, tmpFull); err != nil {
		_ = os.Remove(tmpFull)
		return fmt.Errorf("overlay new layer: %w", err)
	}
	ranges, err := s.cfg.Provisioner.DiffExtents(tmpFull, goldenMemPath)
	if err != nil {
		_ = os.Remove(tmpFull)
		return fmt.Errorf("compare cumulative memory to golden: %w", err)
	}
	fi, err := os.Stat(tmpFull)
	if err != nil {
		_ = os.Remove(tmpFull)
		return err
	}
	if err := provisioner.WriteSparseRanges(tmpFull, tmpDelta, fi.Size(), ranges); err != nil {
		_ = os.Remove(tmpFull)
		return fmt.Errorf("write cumulative delta: %w", err)
	}
	if err := os.Rename(tmpDelta, layerPath); err != nil {
		_ = os.Remove(tmpFull)
		_ = os.Remove(tmpDelta)
		return fmt.Errorf("publish cumulative delta: %w", err)
	}
	if err := os.Rename(tmpFull, fullPath); err != nil {
		_ = os.Remove(tmpFull)
		return fmt.Errorf("publish materialized memory: %w", err)
	}
	return nil
}

// handleRestore boots a brand-new sandbox from a snapshot by loading its memory
// + device state and resuming — skipping kernel boot, init, and agent startup.
// The new sandbox gets fresh tap/IP/port resources. Firecracker initially loads
// the baked device state on an unbridged tap; StartClone + the thaw agent replace
// that identity from MMDS before finishClone joins it to the shared bridge.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	snapID := r.PathValue("id")

	var body struct {
		Name              string `json:"name"`
		TimeoutSec        int    `json:"timeout_sec"`
		HibernateAfterSec int    `json:"hibernate_after_sec"`
		Vcpus             int64  `json:"vcpus"`
		MemMIB            int64  `json:"mem_mib"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}
	if body.TimeoutSec < 0 {
		httpError(w, 400, errors.New("timeout_sec must be >= 0"))
		return
	}
	if body.HibernateAfterSec < -1 {
		httpError(w, 400, errors.New("hibernate_after_sec must be >= -1"))
		return
	}
	if body.Vcpus != 0 || body.MemMIB != 0 {
		httpError(w, 400, errors.New("vcpus/mem_mib cannot be set on restore: resources are baked into the snapshot when it is taken"))
		return
	}
	if err := validateName(body.Name); err != nil {
		httpError(w, 400, err)
		return
	}

	// Restores share the create bring-up budget: they do the same disk copy +
	// VM resume + agent wait as a create and storm the host just the same.
	if err := s.acquireCreate(ctx); err != nil {
		httpError(w, 499, fmt.Errorf("cancelled while queued for create slot: %w", err))
		return
	}
	defer s.releaseCreate()

	// Shared: any number of restores/fanouts may read one snapshot at once, and
	// all of them still exclude delete/metadata writes.
	snapshotOp := s.snapshotLock(snapID)
	snapshotOp.RLock()
	defer snapshotOp.RUnlock()

	snap, err := s.ensureSnapshotLocal(ctx, snapID)
	if err != nil {
		httpError(w, 404, fmt.Errorf("snapshot %s not found: %w", snapID, err))
		return
	}
	// A diff snapshot's mem file holds only dirty pages; Firecracker needs the
	// rebased full file. Internally single-flighted, so concurrent restores of
	// the same snapshot rebase it once.
	if snap.MemPath, err = s.materializeMem(ctx, snap); err != nil {
		httpError(w, 500, fmt.Errorf("materialize snapshot memory: %w", err))
		return
	}

	var expiresAt *time.Time
	if body.TimeoutSec > 0 {
		t := time.Now().Add(time.Duration(body.TimeoutSec) * time.Second)
		expiresAt = &t
	}

	t0 := time.Now()
	// An old tap/IP may already belong to an unrelated sandbox; unlike the
	// former 1:1 path, neither is reclaimed here.
	if err := s.ensureStagedRootfs(snap); err != nil {
		httpError(w, 500, fmt.Errorf("stage snapshot rootfs at baked path: %w", err))
		return
	}

	c := s.bringUpClone(ctx, snap, body.Name, expiresAt, body.HibernateAfterSec, false)
	if c.err != nil {
		capacityOrHTTPError(w, 500, fmt.Errorf("restore clone: %w", c.err))
		return
	}
	if err := s.finishClone(ctx, c); err != nil {
		_ = s.destroy(context.Background(), c.sb.ID)
		httpError(w, 500, fmt.Errorf("finish restore clone: %w", err))
		return
	}
	fmt.Fprintf(os.Stderr, "[%s] restored identity-neutral from %s in %s\n",
		c.sb.ID, snapID, time.Since(t0).Round(time.Millisecond))
	writeJSON(w, 201, s.effectiveResources(c.sb))
}

// clone is one in-flight fan-out clone between Phase 1 (resume) and Phase 2 (bridge).
type clone struct {
	sb         registry.Sandbox
	m          *vm.Machine
	vmID, sock string
	startedAt  time.Time
	setupTime  time.Duration
	launchTime vm.LaunchTimings
	arp        *provisioner.ARPListener // opened on the unbridged tap before resume; nil = fixed-sleep fallback
	guestMAC   string
	// baseSnap is the snapshot this machine was loaded from — the base its
	// dirty-page bitmap tracks against (recorded into Server.diffBase by
	// finishClone). Empty for machines whose load source is not a snapshot
	// row (hibernation wakes load from hib artifacts).
	baseSnap string
	// independent is true for a newly created sandbox and false for a
	// same-sandbox hibernation wake that must preserve its SSH identity.
	independent bool
	err         error
	// lifecycle is held from row allocation through the final readiness
	// transition so destroy/shutdown cannot tear resources out from under a
	// half-started clone. Wake clones already run under their caller's lock.
	lifecycle *keyedMutex
}

// reidentifyMargin bounds how long finishClone waits for the guest's
// gratuitous-ARP announce before bridging anyway. It doubles as the fallback
// sleep when no listener could be opened (or the snapshot predates the
// announcing agent), so the worst case equals the old fixed sleep.
const reidentifyMargin = 1500 * time.Millisecond

// maxFanoutCount bounds the clones one fanout request may ask for. It matches
// the v1 batch cap (internal/apiv1 createBatch: count 1..100) so the two entry
// points into the same machinery can't disagree. Unbounded, a single
// authenticated call sized its own work: `count` registry transactions, rootfs
// clones, VMMs and 30 s agent waits, all under one snapshotLock(snapID).
const maxFanoutCount = 100

// fanoutParallelism caps how many of the host's create permits one fanout may
// hold. The permits cover the whole batch (see handleFanout on why they cannot
// be taken per clone under the snapshot lock), so this is deliberately a
// fraction of a fleet host's budget (24): a large fanout paces itself without
// starving ordinary creates.
//
// It is also the ceiling worth raising LAST. The per-clone cost is dominated by
// two waits inside finishClone — the guest's reidentify announce and its SSH
// keygen — and both are guest-CPU-bound on a host running ~6:1 CPU
// oversubscription. Past some concurrency the announce simply gets slower and
// starts tripping finishClone's second margin, so this wants tuning against
// measured reidentify latency, not a guess.
const fanoutParallelism = 8

// runBounded runs fn for indices [0,n) with at most limit concurrent calls and
// returns when all have finished. Fanout runs each clone's bring-up AND finish
// through one call of this (see fanoutClones): finish holds a 30 s agent wait,
// so it must be paced just as carefully as bring-up.
func runBounded(limit, n int, fn func(i int)) {
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// finishOne runs fanout phase 2 for one clone: wait for its reidentify
// announce, bridge its tap, gate on its agent. A clone that never resumed is
// skipped (logged); one that fails to finish is destroyed, so a partial batch
// leaks nothing.
func (s *Server) finishOne(ctx context.Context, snapID string, c *clone) (registry.Sandbox, bool) {
	finish := s.finishCloneFn
	if finish == nil {
		finish = s.finishClone
	}
	if c == nil || c.err != nil {
		if c != nil && c.err != nil {
			fmt.Fprintf(os.Stderr, "[fanout %s] clone bring-up failed: %v\n", snapID, c.err)
		}
		return registry.Sandbox{}, false
	}
	if err := finish(ctx, c); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] fanout clone finish failed: %v\n", c.sb.ID, err)
		_ = s.destroy(context.Background(), c.sb.ID)
		return registry.Sandbox{}, false
	}
	return c.sb, true
}

// finishClones runs phase 2 for an already-resumed batch, at most limit in
// flight. The announce wait is per-clone inside finishClone, so fast clones
// bridge without waiting on slow ones.
func (s *Server) finishClones(ctx context.Context, snapID string, clones []*clone, limit int) []registry.Sandbox {
	live := make([]registry.Sandbox, 0, len(clones))
	var mu sync.Mutex
	runBounded(limit, len(clones), func(i int) {
		if sb, ok := s.finishOne(ctx, snapID, clones[i]); ok {
			mu.Lock()
			live = append(live, sb)
			mu.Unlock()
		}
	})
	return live
}

// fanoutClones brings up and finishes each clone as ONE pipeline, at most limit
// in flight, so a clone bridges as soon as its own guest announces instead of
// waiting for the slowest resume in the batch. clones[i] is recorded for the
// caller's capacity-vs-genuine failure classification.
func (s *Server) fanoutClones(ctx context.Context, snapID string, snap registry.Snapshot, clones []*clone,
	limit int, expiresAt *time.Time, hibernateAfterSec int) []registry.Sandbox {
	live := make([]registry.Sandbox, 0, len(clones))
	var mu sync.Mutex
	runBounded(limit, len(clones), func(i int) {
		c := s.bringUpClone(ctx, snap, "", expiresAt, hibernateAfterSec, false)
		clones[i] = c
		if sb, ok := s.finishOne(ctx, snapID, c); ok {
			mu.Lock()
			live = append(live, sb)
			mu.Unlock()
		}
	})
	return live
}

// handleFanout restores N identity-neutral clones from one snapshot concurrently.
// Each clone gets a fresh tap/IP/port from the pool (like a cold create) and its
// own reflink CoW rootfs; the snapshot's baked identity is discarded. Clones come
// up on UNBRIDGED taps and reidentify eth0 from MMDS (see vm.StartClone + the
// sandboxd thaw agent) before any tap joins br-fc, so the baked source IP — which
// every clone momentarily shares — never collides on the shared bridge.
func (s *Server) handleFanout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	snapID := r.PathValue("id")

	var body struct {
		Count             int   `json:"count"`
		TimeoutSec        int   `json:"timeout_sec"`
		HibernateAfterSec int   `json:"hibernate_after_sec"`
		Vcpus             int64 `json:"vcpus"`
		MemMIB            int64 `json:"mem_mib"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}
	if body.Count < 1 || body.Count > maxFanoutCount {
		httpError(w, 400, fmt.Errorf("count must be between 1 and %d", maxFanoutCount))
		return
	}
	if body.TimeoutSec < 0 {
		httpError(w, 400, errors.New("timeout_sec must be >= 0"))
		return
	}
	if body.HibernateAfterSec < -1 {
		httpError(w, 400, errors.New("hibernate_after_sec must be >= -1"))
		return
	}
	if body.Vcpus != 0 || body.MemMIB != 0 {
		httpError(w, 400, errors.New("vcpus/mem_mib cannot be set on fanout: resources are baked into the snapshot when it is taken"))
		return
	}

	// Fail fast on a batch this host plainly cannot hold: otherwise the handler
	// allocates, boots and tears down count−free clones a wave at a time before
	// reporting a capacity failure it could see up front. Advisory only
	// (capacity moves under us, and warm/hibernated rows shift it); the
	// per-clone registry admission stays the authority.
	warmAvailable := 0
	if counts, err := s.reg.WarmCountByTemplate(ctx); err == nil {
		warmAvailable = counts[snapID]
		if warmAvailable > body.Count {
			warmAvailable = body.Count
		}
	}
	ordinaryNeeded := body.Count - warmAvailable
	if free, err := s.reg.FreeSlots(ctx); err == nil && ordinaryNeeded > free {
		capacityOrHTTPError(w, http.StatusServiceUnavailable, fmt.Errorf(
			"fanout needs %d ordinary clones after %d warm matches, exceeding this host's %d free slots: %w",
			ordinaryNeeded, warmAvailable, free, registry.ErrPoolExhausted))
		return
	}

	// Bring-ups are gated on the host-wide create budget, exactly like
	// handleCreate and handleRestore. Fanout used to bypass createSem entirely,
	// so one call could boot-storm a host already at its create ceiling.
	//
	// The permits are taken HERE, before snapshotLock and the stage lock, and
	// held for the whole batch. Keep that order: every other consumer acquires
	// createSem *then* the snapshot lock, and taking them the other way round
	// makes a permit-holder wait on a lock-holder that is waiting for permits.
	// (The consumers hold the snapshot lock shared now, so two READERS can no
	// longer deadlock each other that way — but a pending exclusive holder, a
	// delete or a metadata write, still makes the inverted order a hazard.)
	// Only the first permit is waited for — the rest are opportunistic, because
	// blocking for more while holding some lets two concurrent fanouts split a
	// small budget and deadlock.
	if err := s.acquireCreate(ctx); err != nil {
		httpError(w, 499, fmt.Errorf("cancelled while queued for create slot: %w", err))
		return
	}
	limit := 1
	for limit < body.Count && limit < fanoutParallelism && s.tryAcquireCreate() {
		limit++
	}
	defer func() {
		for i := 0; i < limit; i++ {
			s.releaseCreate()
		}
	}()

	// Shared, like handleRestore: concurrent fanouts of one snapshot are safe
	// now that the staged rootfs is permanent, and they still exclude a delete.
	snapshotOp := s.snapshotLock(snapID)
	snapshotOp.RLock()
	defer snapshotOp.RUnlock()

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

	var expiresAt *time.Time
	if body.TimeoutSec > 0 {
		t := time.Now().Add(time.Duration(body.TimeoutSec) * time.Second)
		expiresAt = &t
	}

	t0 := time.Now()
	if err := s.ensureStagedRootfs(snap); err != nil {
		httpError(w, 500, fmt.Errorf("stage snapshot rootfs at baked path: %w", err))
		return
	}

	// Each clone runs bring-up (resume on an UNBRIDGED tap; the in-guest thaw
	// agent then reconfigures eth0 off MMDS) and finish (await the reidentify
	// announce, bridge, gate on the agent) as ONE pipeline, `limit` of them at a
	// time. These used to be two runBounded passes with a barrier between,
	// which existed only so the staged baked rootfs could be unlinked at a known
	// point; the file is permanent now, so the barrier bought nothing but
	// wall-clock — every clone had to resume before any clone could bridge.
	live := make([]registry.Sandbox, 0, body.Count)
	for len(live) < body.Count {
		ready, ok := s.claimWarmForTemplate(ctx, snapID, "", expiresAt, body.HibernateAfterSec)
		if !ok {
			break
		}
		live = append(live, ready)
	}
	remaining := body.Count - len(live)
	clones := make([]*clone, remaining)
	live = append(live, s.fanoutClones(ctx, snapID, snap, clones, limit, expiresAt, body.HibernateAfterSec)...)

	fmt.Fprintf(os.Stderr, "[fanout %s] %d/%d clones live in %s\n",
		snapID, len(live), body.Count, time.Since(t0).Round(time.Millisecond))
	if len(live) == 0 {
		// If any clone died on pool exhaustion, the whole fanout failed on
		// capacity — report it as such so callers back off instead of bailing.
		for _, c := range clones {
			if c != nil && c.err != nil && errors.Is(c.err, registry.ErrPoolExhausted) {
				capacityOrHTTPError(w, 500, fmt.Errorf("all clones failed to start: %w", c.err))
				return
			}
		}
		httpError(w, 500, errors.New("all clones failed to start"))
		return
	}
	for i := range live {
		live[i] = s.effectiveResources(live[i])
	}
	writeJSON(w, 201, live)
}

// bringUpClone allocates resources for one clone and resumes it on an unbridged
// tap. The tap is NOT yet on the bridge — finishClone does that after reidentify.
func (s *Server) bringUpClone(ctx context.Context, snap registry.Snapshot, name string, expiresAt *time.Time, hibernateAfterSec int, warming bool) *clone {
	startedAt := time.Now()
	id := uuid.NewString()
	lifecycle := s.wakeLock(id)
	lifecycle.Lock()
	failed := true
	defer func() {
		if failed {
			lifecycle.Unlock()
		}
	}()
	rootfsPath := s.cfg.Provisioner.RootfsPathFor(id)
	// Clones of the golden snapshot record it as their diff base; clones of a
	// user snapshot don't (no diff chains — their snapshots go full).
	baseID := ""
	if snap.Golden {
		baseID = snap.ID
	}
	// Clones run with the snapshot's baked vcpus/mem; the row records them.
	var sb registry.Sandbox
	var err error
	if warming {
		sb, err = s.reg.CreateWarmForTemplate(ctx, id, rootfsPath, baseID, snap.ID, snap.Vcpus, snap.MemMIB)
	} else {
		sb, err = s.reg.CreateStarting(ctx, id, name, rootfsPath, expiresAt, baseID, hibernateAfterSec, snap.Vcpus, snap.MemMIB)
	}
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
	opts := s.restoreOptions(sb)
	setupTime := time.Since(startedAt)
	guestMAC := randomMAC()
	m, rt, err := vm.StartClone(s.vmCtx, opts, vm.CloneParams{
		MemPath:         snap.MemPath,
		StatePath:       snap.StatePath,
		CloneRootfsPath: rootfsPath,
		TapDevice:       sb.TapDevice,
		GuestIP:         sb.GuestIP,
		MacAddress:      guestMAC,
		GatewayIP:       s.cfg.GatewayIP,
		Prefix:          s.guestSubnetBits(),
		Gen:             id,
	})
	if err != nil {
		if arp != nil {
			_ = arp.Close()
		}
		s.rollbackPreVM(id, sb)
		return &clone{sb: sb, err: fmt.Errorf("start clone: %w", err)}
	}
	if err := provisioner.WakeThawAgent(sb.TapDevice); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] thaw wake on %s failed (poll fallback remains): %v\n", id, sb.TapDevice, err)
	}
	failed = false
	return &clone{
		sb: sb, m: m, vmID: rt.VMID, sock: rt.SocketPath, arp: arp,
		guestMAC: guestMAC,
		baseSnap: snap.ID, independent: true, startedAt: startedAt,
		setupTime: setupTime, launchTime: rt.LaunchTimings, lifecycle: lifecycle,
	}
}

// finishClone waits for the guest's reidentify announce, then bridges the
// clone's tap, sets up port forwarding, records it, and waits for its agent on
// the fresh IP.
func (s *Server) finishClone(ctx context.Context, c *clone) error {
	if c.lifecycle != nil {
		defer c.lifecycle.Unlock()
	}
	sb, m := c.sb, c.m

	phaseStarted := time.Now()
	reidentified := false
	// The tap must stay off the bridge until the guest sheds the snapshot's
	// baked IP. Normally the thaw agent's gratuitous ARP tells us the moment
	// that happens (~200-400ms); the timeout covers agents that predate the
	// announce, matching the old fixed sleep.
	if c.arp != nil {
		if err := c.arp.WaitForIdentity(sb.GuestIP, c.guestMAC, reidentifyMargin); err != nil {
			// A listener was open but no announce arrived. Under a boot storm a
			// CPU-starved guest can miss the margin while still being a modern,
			// announcing agent — bridging now would put two guests with the same
			// baked IP on the bridge. Give it one more margin before falling
			// back to blind bridging (which stays, for pre-announce agents).
			if err2 := c.arp.WaitForIdentity(sb.GuestIP, c.guestMAC, reidentifyMargin); err2 != nil {
				fmt.Fprintf(os.Stderr, "[%s] no reidentify announce after %s (agent in snapshot predates GARP?): %v\n",
					sb.ID, 2*reidentifyMargin, err2)
			} else {
				reidentified = true
			}
		} else {
			reidentified = true
		}
		_ = c.arp.Close()
		c.arp = nil
	} else {
		time.Sleep(reidentifyMargin)
	}
	reidentifyTime := time.Since(phaseStarted)

	phaseStarted = time.Now()
	pid, err := vm.PID(m)
	if err != nil {
		_ = vm.StopForce(m)
		return fmt.Errorf("pid: %w", err)
	}
	if err := s.cfg.Provisioner.AttachTapToBridge(sb.TapDevice); err != nil {
		_ = vm.StopForce(m)
		return fmt.Errorf("attach tap: %w", err)
	}
	if reidentified {
		if err := s.cfg.Provisioner.PrimeGuestNetwork(sb.TapDevice, sb.GuestIP, c.guestMAC); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] prime clone network (ARP fallback remains): %v\n", sb.ID, err)
		}
	}
	if err := s.reg.FinishStart(ctx, sb.ID, pid, c.vmID, c.sock); err != nil {
		_ = vm.StopForce(m)
		return fmt.Errorf("finish start: %w", err)
	}
	s.machines.Store(sb.ID, m)
	if c.baseSnap != "" {
		s.diffBase.Store(sb.ID, c.baseSnap)
	}
	s.act.touch(sb.ID)
	s.watchMachine(sb.ID, m, "clone VM")
	finishStartTime := time.Since(phaseStarted)
	phaseStarted = time.Now()
	if err := waitForAgent(ctx, sb.GuestIP, 30*time.Second); err != nil {
		return fmt.Errorf("agent never ready on %s: %w", sb.GuestIP, err)
	}
	agentTime := time.Since(phaseStarted)
	// Deterministic clock step before the clone is handed out — covers hot
	// creates, fan-out, and clone-path wakes (StartClone's MMDS epoch_ms is
	// polled and can lag the readiness gate by a tick).
	phaseStarted = time.Now()
	syncGuestClock(ctx, sb.GuestIP)
	clockTime := time.Since(phaseStarted)
	identityTime := time.Duration(0)
	if c.independent {
		phaseStarted = time.Now()
		if err := initializeGuestIdentity(ctx, sb.GuestIP, sb.ID); err != nil {
			return fmt.Errorf("initialize guest identity: %w", err)
		}
		identityTime = time.Since(phaseStarted)
	}
	if sb.Status == registry.StatusStarting {
		if err := s.reg.MarkRunning(ctx, sb.ID); err != nil {
			return fmt.Errorf("publish running clone: %w", err)
		}
		c.sb.Status = registry.StatusRunning
		// A warm-pool clone stays 'preparing'/'warming' and is deliberately
		// NOT metered here: its runtime before a claim is platform overhead.
		// claimWarm opens the interval at the moment a customer gets it.
		metered := c.sb
		metered.VMID = c.vmID // the row's vm_id; c.sb carries the pre-launch copy
		s.meterStart(ctx, metered)
	}
	launch := c.launchTime
	fmt.Fprintf(os.Stderr,
		"[%s] clone phases: setup=%s prepare=%s process_api=%s snapshot_load=%s drive=%s mmds=%s resume=%s reidentify=%s finish=%s agent=%s clock=%s identity=%s total=%s\n",
		sb.ID, roundMS(c.setupTime), roundMS(launch.Prepare), roundMS(launch.ProcessToAPI),
		roundMS(launch.SnapshotLoad), roundMS(launch.DrivePatch), roundMS(launch.MMDS),
		roundMS(launch.Resume), roundMS(reidentifyTime), roundMS(finishStartTime),
		roundMS(agentTime), roundMS(clockTime), roundMS(identityTime), roundMS(time.Since(c.startedAt)))
	return nil
}

func roundMS(d time.Duration) time.Duration {
	return d.Round(time.Millisecond)
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
	if err := s.deleteSnapshot(r.Context(), id); err != nil {
		code := http.StatusNotFound
		if errors.Is(err, registry.ErrSnapshotInUse) {
			code = http.StatusConflict
		}
		httpError(w, code, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteSnapshot(ctx context.Context, id string) error {
	op := s.snapshotLock(id)
	op.Lock()
	defer op.Unlock()
	return s.deleteSnapshotLocked(ctx, id)
}

func (s *Server) deleteSnapshotLocked(ctx context.Context, id string) error {
	snap, err := s.reg.GetSnapshot(ctx, id)
	if err != nil {
		return err
	}
	if snap.Golden {
		return fmt.Errorf("%w: server-managed template snapshot cannot be deleted", registry.ErrSnapshotInUse)
	}
	if dependencies, err := s.reg.SnapshotDependencyCount(ctx, id); err != nil {
		return err
	} else if dependencies > 0 {
		return fmt.Errorf("%w: snapshot %s has %d dependent resources", registry.ErrSnapshotInUse, id, dependencies)
	}
	// Ensure no background writer can recreate meta.json or keep reading files
	// after this point.
	s.cancelSnapshotUpload(id)
	if s.blob != nil {
		// Remove the durable commit before deleting the local row. If GCS is
		// unavailable, keep the snapshot locally registered and return an error
		// rather than claim deletion while another host can still restore it.
		if err := s.blob.Delete(ctx, snapObj(id, "meta.json")); err != nil {
			return fmt.Errorf("delete durable snapshot commit: %w", err)
		}
	}
	if err := s.reg.DeleteSnapshot(ctx, id); err != nil {
		return err
	}
	// If the golden snapshot was deleted, stop hot-creating from it. Creates
	// cold-boot until the next server restart rebuilds a golden snapshot.
	if g := s.golden.Load(); g != nil && g.ID == id {
		s.golden.Store(nil)
	}
	if s.blob != nil {
		go s.deleteSnapshotPayloadObjects(id)
	}
	_ = s.cfg.Provisioner.CleanupSnapshot(id)
	s.removeStagedRootfs(ctx, snap)
	return nil
}

// removeStagedRootfs drops the copy ensureStagedRootfs left at the snapshot's
// baked rootfs path. That path lives outside SnapshotDir (it is the SOURCE
// sandbox's rootfs path), so CleanupSnapshot does not cover it, and leaving it
// would accumulate one file per restored-then-deleted snapshot.
//
// It is skipped while the source sandbox still owns that path — then the file
// is a LIVE sandbox's rootfs that we never staged. Callers hold the snapshot's
// exclusive lock, so no restore can be mid-load; and an unlink is safe against
// still-open fds on Linux regardless.
func (s *Server) removeStagedRootfs(ctx context.Context, snap registry.Snapshot) {
	if snap.SourceRootfsPath == "" || snap.Golden {
		return
	}
	if src, err := s.reg.Get(ctx, snap.SourceID); err == nil && src.RootfsPath == snap.SourceRootfsPath {
		return
	}
	if err := s.cfg.Provisioner.RemoveRootfs(snap.SourceRootfsPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "[snapshot %s] remove staged rootfs %s: %v\n", snap.ID, snap.SourceRootfsPath, err)
	}
}

// deleteExpiredSnapshot repeats the expiry decision under the same operation
// lock used by retention updates and deletion.
func (s *Server) deleteExpiredSnapshot(ctx context.Context, id string, cutoff time.Time) error {
	op := s.snapshotLock(id)
	op.Lock()
	defer op.Unlock()
	snap, err := s.reg.GetSnapshot(ctx, id)
	if err != nil {
		return err
	}
	if snap.ExpiresAt == nil || !snap.ExpiresAt.Before(cutoff) {
		return nil
	}
	return s.deleteSnapshotLocked(ctx, id)
}

// handleSnapshotPublicFields persists public retention metadata after the
// immutable snapshot has been captured.
func (s *Server) handleSnapshotPublicFields(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name             string     `json:"name"`
		RetentionSeconds int        `json:"retention_seconds"`
		ExpiresAt        *time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}
	if body.RetentionSeconds < 0 {
		httpError(w, 400, errors.New("retention_seconds must be non-negative"))
		return
	}
	op := s.snapshotLock(r.PathValue("id"))
	op.Lock()
	defer op.Unlock()
	expiresAt := body.ExpiresAt
	if expiresAt == nil && body.RetentionSeconds > 0 {
		value := time.Now().Add(time.Duration(body.RetentionSeconds) * time.Second)
		expiresAt = &value
	}
	snap, err := s.reg.SetSnapshotPublicFields(r.Context(), r.PathValue("id"), body.Name, expiresAt)
	if err != nil {
		httpError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleSnapshotWarmTarget(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WarmTarget *int `json:"warm_target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}
	if body.WarmTarget == nil {
		httpError(w, 400, errors.New("warm_target is required"))
		return
	}
	target := *body.WarmTarget
	budget := s.warmPoolBudget()
	if target < 0 || target > budget {
		httpError(w, 400, fmt.Errorf("warm_target must be between 0 and warm_pool_budget (%d)", budget))
		return
	}
	s.warmPolicyMu.Lock()
	defer s.warmPolicyMu.Unlock()
	id := r.PathValue("id")
	snaps, err := s.reg.ListSnapshots(r.Context())
	if err != nil {
		httpError(w, 500, err)
		return
	}
	total := s.cfg.WarmPoolSize
	found := false
	for _, snap := range snaps {
		if snap.ID == id {
			found = snap.Role == registry.SnapshotRoleTemplate
			continue
		}
		if snap.Role == registry.SnapshotRoleTemplate {
			total += snap.WarmTarget
		}
	}
	if !found {
		httpError(w, 404, fmt.Errorf("template snapshot %s not found", id))
		return
	}
	if total+target > budget {
		httpError(w, 409, fmt.Errorf("warm targets total %d exceeds warm_pool_budget %d", total+target, budget))
		return
	}
	updated, err := s.reg.SetSnapshotWarmTarget(r.Context(), id, target)
	if err != nil {
		httpError(w, 404, err)
		return
	}
	s.kickWarmPool()
	writeJSON(w, http.StatusOK, updated)
}
