package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

// The golden snapshot makes POST /sandboxes hot by default: a snapshot of a
// freshly cold-booted pristine sandbox that creates clone from (the same
// identity-neutral mechanism as fan-out, N=1) instead of paying kernel boot +
// agent startup. It's entirely server-managed — clients never see it.
//
// ensureGolden runs once at startup: it adopts the previous run's golden
// snapshot if the base rootfs hasn't changed since, otherwise cold-boots a
// throwaway sandbox, snapshots it, and destroys it. Every failure is
// non-fatal — s.golden stays nil and creates simply cold-boot as before.
func (s *Server) ensureGolden(ctx context.Context) {
	// Whatever happens — adopt, build, or fail — the host is "warmed" once this
	// returns: the heartbeat may start advertising real free slots. A failed
	// build just means cold creates (slower, still functional); never leave the
	// host permanently unplaceable.
	//
	// This is the phase that dominates a cold worker's warm-up when the golden
	// has to be BUILT (cold boot + snapshot) and near-free when it's ADOPTED
	// from a baked data disk — the boot-phase timeline separates the two.
	defer func() {
		s.phases.mark(phaseGoldenSettled)
		close(s.warmed)
	}()

	snap, err := s.reg.GoldenSnapshot(ctx)
	if err != nil {
		// No golden row in the registry. A fresh host booted off a pre-baked
		// golden data-disk image carries the snapshot artifacts + a manifest
		// sidecar but an EMPTY DB (each host keeps its own SQLite, and reconcile
		// treats it as fresh): import the row from the manifest and fall into
		// the adopt path below. Any other case (no manifest, parse error, stale
		// artifacts) returns ok=false and we cold-build as before.
		var ok bool
		if snap, ok = s.importGoldenManifest(ctx); !ok {
			s.buildGolden(ctx)
			return
		}
	}

	if s.goldenUsable(snap) {
		if err := s.stageSnapshotRootfs(snap); err == nil {
			s.golden.Store(&snap)
			go s.uploadGoldenBase(snap)
			fmt.Fprintf(os.Stderr, "golden snapshot %s adopted; creates are hot\n", snap.ID)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "golden snapshot %s is stale or broken; rebuilding\n", snap.ID)
	_ = s.reg.DeleteSnapshot(ctx, snap.ID)
	_ = s.cfg.Provisioner.CleanupSnapshot(snap.ID)
	s.buildGolden(ctx)
}

// goldenUsable reports whether snap's artifacts are all on disk and the base
// rootfs still matches the stat recorded when snap was taken.
func (s *Server) goldenUsable(snap registry.Snapshot) bool {
	for _, p := range []string{snap.MemPath, snap.StatePath, snap.RootfsPath} {
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	fi, err := os.Stat(s.cfg.Provisioner.RootfsBase)
	if err != nil {
		return false
	}
	return fi.ModTime().Unix() == snap.BaseMtime && fi.Size() == snap.BaseSize
}

// buildGolden cold-boots a throwaway sandbox, snapshots it as golden, and
// destroys it. On success, subsequent creates clone the snapshot.
func (s *Server) buildGolden(ctx context.Context) {
	t0 := time.Now()
	fmt.Fprintln(os.Stderr, "building golden snapshot (cold boot + snapshot)...")

	// -1: the throwaway golden source must never be hibernated out from under
	// the snapshot step. No resource overrides — the golden snapshot always
	// bakes the template's vcpus/mem (override creates cold-boot instead).
	sb, err := s.createCold(ctx, "", nil, -1, 0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "golden snapshot: cold boot failed, creates stay cold: %v\n", err)
		return
	}
	snap, _, snapErr := s.snapshotSandbox(ctx, sb.ID, true, "")
	// The source exists only to be snapshotted — destroy it either way.
	if err := s.destroy(context.Background(), sb.ID); err != nil {
		fmt.Fprintf(os.Stderr, "golden snapshot: destroy source %s: %v\n", sb.ID, err)
	}
	if snapErr != nil {
		fmt.Fprintf(os.Stderr, "golden snapshot: snapshot failed, creates stay cold: %v\n", snapErr)
		return
	}
	if err := s.stageSnapshotRootfs(snap); err != nil {
		fmt.Fprintf(os.Stderr, "golden snapshot: stage rootfs failed, creates stay cold: %v\n", err)
		return
	}
	s.golden.Store(&snap)
	s.writeGoldenManifest(snap)
	go s.uploadGoldenBase(snap)
	fmt.Fprintf(os.Stderr, "golden snapshot %s built in %s; creates are hot\n", snap.ID, time.Since(t0).Round(time.Millisecond))
}

// goldenManifest is the self-describing sidecar written next to the golden
// snapshot's artifacts. It lets a fresh host booted off a pre-baked golden
// data-disk image ADOPT the golden (import the row, then validate + clone)
// instead of cold-building one. BaseMtime/BaseSize are carried explicitly
// because registry.Snapshot marks them json:"-" — yet goldenUsable keys the
// staleness check on exactly those two, so the manifest must persist them.
type goldenManifest struct {
	Snapshot  registry.Snapshot `json:"snapshot"`
	BaseMtime int64             `json:"base_mtime"`
	BaseSize  int64             `json:"base_size"`
}

// goldenManifestPath is the fixed on-disk location of the manifest — a stable
// name (independent of the golden's id) so import can find it without knowing
// the id. It rides the golden data-disk image because SnapshotDir lives on it.
func (s *Server) goldenManifestPath() string {
	return filepath.Join(s.cfg.Provisioner.SnapshotDir, "golden.json")
}

// writeGoldenManifest records the just-built golden as a data-disk sidecar so a
// future fresh host can import it. Best-effort (written atomically via a temp +
// rename): a failure only costs the cross-host adopt fast path, never the run.
func (s *Server) writeGoldenManifest(snap registry.Snapshot) {
	m := goldenManifest{Snapshot: snap, BaseMtime: snap.BaseMtime, BaseSize: snap.BaseSize}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "golden manifest: marshal: %v\n", err)
		return
	}
	path := s.goldenManifestPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "golden manifest: write %s: %v\n", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Fprintf(os.Stderr, "golden manifest: rename %s: %v\n", path, err)
	}
}

// importGoldenManifest adopts a golden baked onto the data disk when the
// registry has no golden row (fresh host from a pre-baked golden data-disk
// image). It reconstructs the row from the sidecar, validates the artifacts +
// base rootfs stat (goldenUsable), and re-inserts the row so the normal adopt
// path can clone it. Returns ok=false — "fall back to cold-building" — for
// every failure mode (absent/corrupt manifest, stale artifacts, insert error),
// so a bad or missing manifest can never do worse than today's cold build.
func (s *Server) importGoldenManifest(ctx context.Context) (registry.Snapshot, bool) {
	b, err := os.ReadFile(s.goldenManifestPath())
	if err != nil {
		return registry.Snapshot{}, false // no manifest: the normal cold-build path
	}
	var m goldenManifest
	if err := json.Unmarshal(b, &m); err != nil {
		fmt.Fprintf(os.Stderr, "golden manifest: parse: %v (cold-building instead)\n", err)
		return registry.Snapshot{}, false
	}
	snap := m.Snapshot
	snap.BaseMtime = m.BaseMtime
	snap.BaseSize = m.BaseSize
	snap.Golden = true
	if !s.goldenUsable(snap) {
		fmt.Fprintf(os.Stderr, "golden manifest %s present but artifacts/base stale; cold-building instead\n", snap.ID)
		return registry.Snapshot{}, false
	}
	if err := s.reg.CreateSnapshot(ctx, snap); err != nil {
		fmt.Fprintf(os.Stderr, "golden manifest %s: import row: %v (cold-building instead)\n", snap.ID, err)
		return registry.Snapshot{}, false
	}
	fmt.Fprintf(os.Stderr, "golden snapshot %s imported from data-disk manifest\n", snap.ID)
	return snap, true
}

// uploadGoldenBase eagerly pushes the golden's base template to GCS (once).
// User diff snapshots would upload it lazily on their first upload anyway;
// hibernation diffs need it EAGERLY — they anchor to the golden without ever
// uploading anything themselves, and hibernate only chooses the diff format
// once s.baseUploaded reports the anchor durable. Without this, a golden
// rebuild (agent update) would orphan every diff-hibernated sandbox.
func (s *Server) uploadGoldenBase(snap registry.Snapshot) {
	if s.blob == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := s.ensureBaseUploaded(ctx, snap); err != nil {
		fmt.Fprintf(os.Stderr, "[base %s] eager golden base upload failed (hibernation stays full-format): %v\n", snap.ID, err)
	}
}

// stageSnapshotRootfs makes sure the rootfs path baked into the snapshot
// exists: Firecracker opens it during LoadSnapshot, before the per-clone
// PATCH /drives relocates the disk. Unlike fan-out — which stages per call and
// removes it after — the golden snapshot's staged file is left in place so
// every create doesn't re-pay the copy. It's re-staged here if something else
// consumed it (e.g. a 1:1 restore of the golden snapshot ran on it).
func (s *Server) stageSnapshotRootfs(snap registry.Snapshot) error {
	s.stageMu.Lock()
	defer s.stageMu.Unlock()
	if _, err := os.Stat(snap.SourceRootfsPath); err == nil {
		return nil
	}
	return s.cfg.Provisioner.CopyFileSparse(snap.RootfsPath, snap.SourceRootfsPath)
}

// createFromSnapshot brings up one identity-neutral clone of snap — the same
// two-phase resume-then-bridge dance as fan-out, for a single sandbox.
func (s *Server) createFromSnapshot(ctx context.Context, snap registry.Snapshot, name string, expiresAt *time.Time, hibernateAfterSec int) (registry.Sandbox, error) {
	if err := s.stageSnapshotRootfs(snap); err != nil {
		return registry.Sandbox{}, fmt.Errorf("stage snapshot rootfs: %w", err)
	}

	t0 := time.Now()
	c := s.bringUpClone(snap, name, expiresAt, hibernateAfterSec)
	if c.err != nil {
		return registry.Sandbox{}, c.err
	}

	// finishClone waits for the guest's reidentify announce (or the fixed
	// margin, for pre-announce agents) before bridging the tap.
	if err := s.finishClone(ctx, c); err != nil {
		_ = s.destroy(context.Background(), c.sb.ID)
		return registry.Sandbox{}, err
	}
	fmt.Fprintf(os.Stderr, "[%s] hot create from golden snapshot %s in %s\n",
		c.sb.ID, snap.ID, time.Since(t0).Round(time.Millisecond))
	return c.sb, nil
}
