package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
)

// GCS snapshot durability. Layout in the bucket:
//
//	bases/<golden-id>/{mem.sz, rootfs.sz, complete}   # base template, uploaded once per golden version
//	snaps/<snap-id>/{mem.sz, rootfs.sz, state.sz, meta.json}
//
// meta.json is uploaded LAST and is the commit marker: a snapshot without it
// is invisible to pulls, so partial uploads are never restorable (rather than
// corrupt). For format=diff snapshots, mem.sz encodes only the dirty pages
// (the Firecracker diff file's data regions) and rootfs.sz only the extents
// that diverged from the base rootfs — both are overlays applied on top of a
// copy of the base at pull time. Base templates are immutable and never
// deleted (snapshots reference them indefinitely).

func snapObj(id, name string) string { return "snaps/" + id + "/" + name }
func baseObj(id, name string) string { return "bases/" + id + "/" + name }

// uploadTimeout bounds one background snapshot upload end to end.
const uploadTimeout = 30 * time.Minute

// uploadSnapshot performs one recoverable attempt. The dispatcher owns retry
// state; this function publishes immutable artifacts and commits metadata last.
func (s *Server) uploadSnapshot(ctx context.Context, snap registry.Snapshot) error {
	t0 := time.Now()
	committed, err := s.snapshotCommitted(ctx, snap.ID)
	if err != nil || committed {
		return err
	}
	for _, path := range []string{snap.RootfsPath, snap.MemPath, snap.StatePath} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s", errSnapshotArtifacts, path)
		}
	}
	var rootfsRanges []provisioner.Range
	if snap.Format == registry.FormatDiff {
		base, err := s.reg.GetSnapshot(ctx, snap.BaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: base %s missing", errSnapshotArtifacts, snap.BaseID)
		}
		if err != nil {
			return err
		}
		if err := s.ensureBaseUploaded(ctx, base); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w: base files missing: %v", errSnapshotArtifacts, err)
			}
			return fmt.Errorf("upload base template: %w", err)
		}
		rootfsRanges, err = s.cfg.Provisioner.DiffExtents(snap.RootfsPath, base.RootfsPath)
		if err != nil {
			// A full overlay is valid even when the base is unavailable locally.
			info, statErr := os.Stat(snap.RootfsPath)
			if statErr != nil {
				return fmt.Errorf("%w: %v", errSnapshotArtifacts, statErr)
			}
			rootfsRanges = []provisioner.Range{{Off: 0, Len: info.Size()}}
		}
	}

	var memBytes, rootfsBytes int64
	if snap.Format == registry.FormatDiff {
		rootfsBytes, err = s.blob.PutRanges(ctx, snapObj(snap.ID, "rootfs.sz"), snap.RootfsPath, toBlobRanges(rootfsRanges))
	} else {
		rootfsBytes, err = s.blob.PutSparse(ctx, snapObj(snap.ID, "rootfs.sz"), snap.RootfsPath)
	}
	if err == nil {
		memBytes, err = s.blob.PutSparse(ctx, snapObj(snap.ID, "mem.sz"), snap.MemPath)
	}
	if err == nil {
		_, err = s.blob.PutSparse(ctx, snapObj(snap.ID, "state.sz"), snap.StatePath)
	}
	if err != nil {
		return err
	}
	snap.Upload = nil // host-local delivery status is not transferable metadata
	meta, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	_, err = s.blob.PutBytesIfGenerationMatch(ctx, snapObj(snap.ID, "meta.json"), meta, 0)
	if errors.Is(err, gcsblob.ErrPreconditionFailed) {
		// A lost response may hide our successful commit. A tombstone, however,
		// permanently wins over publication and must never count as success.
		committed, err = s.snapshotCommitted(ctx, snap.ID)
		if err == nil && !committed {
			return errors.New("snapshot commit disappeared after publication conflict")
		}
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[snapshot %s] uploaded to gs://%s (%s): mem=%dMiB rootfs=%dMiB payload in %s\n",
		snap.ID, s.blob.Bucket(), snap.Format, memBytes>>20, rootfsBytes>>20, time.Since(t0).Round(time.Millisecond))
	return nil
}

// snapshotLock serializes all local consumers and deletion of one snapshot.
// Different snapshot ids remain independent.
func (s *Server) snapshotLock(id string) *keyedMutex {
	return s.snapshotLocks.acquire(id)
}

// baseUploaded reports whether a base template is known-durable in GCS —
// the precondition for anchoring a hibernation diff to it. Only reflects
// uploads this process has verified (the map re-fills from the Exists check
// on the eager golden upload at startup).
func (s *Server) baseUploaded(id string) bool {
	if s.blob == nil {
		return false
	}
	s.baseUpMu.Lock()
	defer s.baseUpMu.Unlock()
	return s.basesUploaded[id]
}

// ensureBaseUploaded uploads a golden snapshot's mem+rootfs as an immutable
// base template, once. The "complete" marker commits it; meta.json of any
// snapshot referencing the base is only uploaded after this returns.
func (s *Server) ensureBaseUploaded(ctx context.Context, base registry.Snapshot) error {
	select {
	case s.baseUploadGate <- struct{}{}:
		defer func() { <-s.baseUploadGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	s.baseUpMu.Lock()
	uploaded := s.basesUploaded[base.ID]
	s.baseUpMu.Unlock()
	if uploaded {
		return nil
	}
	if ok, err := s.blob.Exists(ctx, baseObj(base.ID, "complete")); err != nil {
		return err
	} else if ok {
		s.baseUpMu.Lock()
		s.basesUploaded[base.ID] = true
		s.baseUpMu.Unlock()
		return nil
	}

	t0 := time.Now()
	fmt.Fprintf(os.Stderr, "[base %s] uploading base template to gs://%s...\n", base.ID, s.blob.Bucket())
	memBytes, err := s.blob.PutSparse(ctx, baseObj(base.ID, "mem.sz"), base.MemPath)
	if err != nil {
		return fmt.Errorf("upload base mem: %w", err)
	}
	rootfsBytes, err := s.blob.PutSparse(ctx, baseObj(base.ID, "rootfs.sz"), base.RootfsPath)
	if err != nil {
		return fmt.Errorf("upload base rootfs: %w", err)
	}
	meta, _ := json.Marshal(base)
	if err := s.blob.PutBytes(ctx, baseObj(base.ID, "complete"), meta); err != nil {
		return fmt.Errorf("commit base: %w", err)
	}
	s.baseUpMu.Lock()
	s.basesUploaded[base.ID] = true
	s.baseUpMu.Unlock()
	fmt.Fprintf(os.Stderr, "[base %s] uploaded: mem=%dMiB rootfs=%dMiB payload in %s\n",
		base.ID, memBytes>>20, rootfsBytes>>20, time.Since(t0).Round(time.Millisecond))
	return nil
}

// pullLock returns a mutex dedicated to one pull key (snapshot or base id),
// so concurrent restores of the same id download once while different ids
// proceed in parallel.
func (s *Server) pullLock(key string) *keyedMutex {
	return s.pulls.acquire(key)
}

// ensureSnapshotLocal returns the snapshot row, pulling the snapshot down
// from GCS onto this host first when it isn't known locally — the path that
// makes any live host able to restore any snapshot, including ones whose
// creating host is gone.
var errSnapshotNotFound = errors.New("snapshot not found")

func (s *Server) ensureSnapshotLocal(ctx context.Context, snapID string) (registry.Snapshot, error) {
	return s.ensureSnapshotLocalFrom(ctx, snapID, "")
}

// ensureSnapshotLocalFrom prefers a live peer supplied by the gateway, then
// falls back to durable GCS. Both paths share the same per-snapshot pull lock,
// so a burst downloads once on each target host regardless of request count.
func (s *Server) ensureSnapshotLocalFrom(ctx context.Context, snapID, peer string) (registry.Snapshot, error) {
	snap, err := s.reg.GetSnapshot(ctx, snapID)
	if err == nil {
		return snap, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return registry.Snapshot{}, err
	}
	if s.blob == nil && peer == "" {
		return registry.Snapshot{}, errSnapshotNotFound
	}

	mu := s.pullLock("snap:" + snapID)
	mu.Lock()
	defer mu.Unlock()
	// Another request may have completed the pull while we waited.
	if snap, err := s.reg.GetSnapshot(ctx, snapID); err == nil {
		return snap, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return registry.Snapshot{}, err
	}
	if peer != "" {
		t0 := time.Now()
		pulled, bytes, peerErr := s.pullSnapshotFromPeer(ctx, snapID, peer)
		if peerErr == nil {
			s.met.snapshotPeerPulls.Add(1)
			fmt.Fprintf(os.Stderr, "[snapshot %s] pulled from peer %s: wire=%dMiB in %s\n",
				snapID, peer, bytes>>20, time.Since(t0).Round(time.Millisecond))
			return pulled, nil
		}
		s.met.snapshotPeerFailures.Add(1)
		fmt.Fprintf(os.Stderr, "[snapshot %s] peer %s unavailable (%v); falling back to GCS\n",
			snapID, peer, peerErr)
	}
	if s.blob == nil {
		return registry.Snapshot{}, fmt.Errorf("not in local registry and peer transfer failed; no snapshot bucket configured")
	}
	if peer != "" {
		s.met.snapshotGCSFallbacks.Add(1)
	}

	metaBytes, err := s.blob.GetBytes(ctx, snapObj(snapID, "meta.json"))
	if err != nil {
		if errors.Is(err, gcsblob.ErrNotExist) {
			// An unavailable peer may still hold an unfinished upload.
			if peer == "" {
				return registry.Snapshot{}, errSnapshotNotFound
			}
			return registry.Snapshot{}, fmt.Errorf("not in local registry or gs://%s", s.blob.Bucket())
		}
		return registry.Snapshot{}, fmt.Errorf("fetch snapshot meta: %w", err)
	}
	meta, err := decodeSnapshotCommit(metaBytes, snapID)
	if err != nil {
		return registry.Snapshot{}, fmt.Errorf("decode snapshot meta: %w", err)
	}

	t0 := time.Now()
	memPath, statePath, rootfsPath, err := s.cfg.Provisioner.SnapshotPaths(snapID)
	if err != nil {
		return registry.Snapshot{}, err
	}

	if meta.Format == registry.FormatDiff {
		baseMem, baseRootfs, err := s.ensureBaseLocal(ctx, meta.BaseID)
		if err != nil {
			return registry.Snapshot{}, fmt.Errorf("pull base template %s: %w", meta.BaseID, err)
		}
		_ = baseMem // the mem diff stays a diff on disk; materializeMem rebases it at restore time
		// Rootfs: start from a reflink of the base, overlay the changed extents.
		if err := provisioner.CloneFile(baseRootfs, rootfsPath); err != nil {
			return registry.Snapshot{}, fmt.Errorf("stage base rootfs: %w", err)
		}
	}
	for _, obj := range []struct{ name, path string }{
		{"rootfs.sz", rootfsPath},
		{"mem.sz", memPath},
		{"state.sz", statePath},
	} {
		if err := s.blob.GetSparse(ctx, snapObj(snapID, obj.name), obj.path); err != nil {
			_ = s.cfg.Provisioner.CleanupSnapshot(snapID)
			return registry.Snapshot{}, fmt.Errorf("pull %s: %w", obj.name, err)
		}
	}

	row := meta
	row.MemPath, row.StatePath, row.RootfsPath = memPath, statePath, rootfsPath
	row.Golden = false
	row.Durability = "durable"
	if err := s.reg.CreateSnapshot(ctx, row); err != nil {
		_ = s.cfg.Provisioner.CleanupSnapshot(snapID)
		return registry.Snapshot{}, fmt.Errorf("record pulled snapshot: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[snapshot %s] pulled from gs://%s (%s) in %s\n",
		snapID, s.blob.Bucket(), row.Format, time.Since(t0).Round(time.Millisecond))
	return row, nil
}

// baseCachePaths returns where a pulled base template's artifacts live on
// this host (under the snapshot dir so they share the XFS reflink domain).
func (s *Server) baseCachePaths(baseID string) (mem, rootfs string) {
	dir := filepath.Join(s.cfg.Provisioner.SnapshotDir, "bases", baseID)
	return filepath.Join(dir, "mem.bin"), filepath.Join(dir, "rootfs.ext4")
}

// ensureBaseLocal makes a base template's artifacts available locally: the
// local golden row if this host created the base, the base cache if it was
// pulled before, otherwise a GCS download (once per host per base).
func (s *Server) ensureBaseLocal(ctx context.Context, baseID string) (mem, rootfs string, err error) {
	// This host's own golden?
	if base, err := s.reg.GetSnapshot(ctx, baseID); err == nil {
		if _, e1 := os.Stat(base.MemPath); e1 == nil {
			if _, e2 := os.Stat(base.RootfsPath); e2 == nil {
				return base.MemPath, base.RootfsPath, nil
			}
		}
	}

	mem, rootfs = s.baseCachePaths(baseID)
	mu := s.pullLock("base:" + baseID)
	mu.Lock()
	defer mu.Unlock()
	if _, e1 := os.Stat(mem); e1 == nil {
		if _, e2 := os.Stat(rootfs); e2 == nil {
			return mem, rootfs, nil
		}
	}
	if s.blob == nil {
		return "", "", fmt.Errorf("base template %s is not on disk and no snapshot bucket is configured", baseID)
	}
	if err := os.MkdirAll(filepath.Dir(mem), 0o755); err != nil {
		return "", "", err
	}
	t0 := time.Now()
	fmt.Fprintf(os.Stderr, "[base %s] pulling base template from gs://%s (one-time)...\n", baseID, s.blob.Bucket())
	// Download to .tmp then rename, so a crash never leaves a plausible but
	// truncated base.
	for _, obj := range []struct{ name, path string }{
		{"mem.sz", mem},
		{"rootfs.sz", rootfs},
	} {
		tmp := obj.path + ".tmp"
		if err := s.blob.GetSparse(ctx, baseObj(baseID, obj.name), tmp); err != nil {
			_ = os.Remove(tmp)
			return "", "", err
		}
		if err := os.Rename(tmp, obj.path); err != nil {
			return "", "", err
		}
	}
	fmt.Fprintf(os.Stderr, "[base %s] base template cached in %s\n", baseID, time.Since(t0).Round(time.Millisecond))
	return mem, rootfs, nil
}

// ensureBaseRootfsLocal is the disk-only half of ensureBaseLocal. Lazy
// hibernation adoption needs the base rootfs to overlay a diff, but must not
// turn that into an eager download of the base guest memory.
func (s *Server) ensureBaseRootfsLocal(ctx context.Context, baseID string) (string, error) {
	if base, err := s.reg.GetSnapshot(ctx, baseID); err == nil {
		if _, statErr := os.Stat(base.RootfsPath); statErr == nil {
			return base.RootfsPath, nil
		}
	}
	_, rootfs := s.baseCachePaths(baseID)
	mu := s.pullLock("base:" + baseID)
	mu.Lock()
	defer mu.Unlock()
	if _, err := os.Stat(rootfs); err == nil {
		return rootfs, nil
	}
	if s.blob == nil {
		return "", fmt.Errorf("base template %s rootfs is not on disk and no snapshot bucket is configured", baseID)
	}
	if err := os.MkdirAll(filepath.Dir(rootfs), 0o755); err != nil {
		return "", err
	}
	tmp := rootfs + ".tmp"
	if err := s.blob.GetSparse(ctx, baseObj(baseID, "rootfs.sz"), tmp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, rootfs); err != nil {
		return "", err
	}
	return rootfs, nil
}

// materializeMem returns a full, restorable mem file for snap: the file
// itself for full snapshots, or (for diff snapshots) a cached rebase of the
// dirty pages onto a reflinked copy of the base mem — Firecracker's
// rebase-snap operation.
func (s *Server) materializeMem(ctx context.Context, snap registry.Snapshot) (string, error) {
	if snap.Format != registry.FormatDiff {
		return snap.MemPath, nil
	}
	fullPath := filepath.Join(filepath.Dir(snap.MemPath), "mem.full.bin")
	if _, err := os.Stat(fullPath); err == nil {
		return fullPath, nil
	}

	mu := s.pullLock("mat:" + snap.ID)
	mu.Lock()
	defer mu.Unlock()
	if _, err := os.Stat(fullPath); err == nil {
		return fullPath, nil
	}

	baseMem, _, err := s.ensureBaseLocal(ctx, snap.BaseID)
	if err != nil {
		return "", fmt.Errorf("base template %s: %w", snap.BaseID, err)
	}
	tmp := fullPath + ".tmp"
	if err := provisioner.CloneFile(baseMem, tmp); err != nil {
		return "", fmt.Errorf("clone base mem: %w", err)
	}
	if err := s.cfg.Provisioner.OverlaySparse(snap.MemPath, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("overlay dirty pages: %w", err)
	}
	if err := os.Rename(tmp, fullPath); err != nil {
		return "", err
	}
	return fullPath, nil
}

// deleteSnapshotPayloadObjects removes uncommitted payload after meta.json was
// synchronously deleted. Best-effort: leftovers cannot be restored.
func (s *Server) deleteSnapshotPayloadObjects(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, name := range []string{"mem.sz", "rootfs.sz", "state.sz"} {
		if err := s.blob.Delete(ctx, snapObj(id, name)); err != nil {
			fmt.Fprintf(os.Stderr, "[snapshot %s] gcs delete %s: %v\n", id, name, err)
		}
	}
}

func toBlobRanges(in []provisioner.Range) []gcsblob.Range {
	out := make([]gcsblob.Range, len(in))
	for i, r := range in {
		out[i] = gcsblob.Range{Off: r.Off, Len: r.Len}
	}
	return out
}
