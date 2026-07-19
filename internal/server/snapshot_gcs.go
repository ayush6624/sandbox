package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

// uploadSnapshot ships a freshly created snapshot to GCS in the background:
// (base template if needed) → artifacts → meta.json. Failures are logged and
// leave the snapshot host-local only — the next snapshot retries the base.
func (s *Server) uploadSnapshot(snap registry.Snapshot) {
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()
	t0 := time.Now()

	var rootfsRanges []provisioner.Range
	if snap.Format == registry.FormatDiff {
		base, err := s.reg.GetSnapshot(ctx, snap.BaseID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[snapshot %s] upload aborted: base %s vanished: %v\n", snap.ID, snap.BaseID, err)
			return
		}
		if err := s.ensureBaseUploaded(ctx, base); err != nil {
			fmt.Fprintf(os.Stderr, "[snapshot %s] upload aborted: base template: %v\n", snap.ID, err)
			return
		}
		rootfsRanges, err = s.cfg.Provisioner.DiffExtents(snap.RootfsPath, base.RootfsPath)
		if err != nil {
			// Conservative fallback: encode the whole file as one overlay.
			// Zeros compress away; correctness is unaffected.
			fmt.Fprintf(os.Stderr, "[snapshot %s] extent diff failed (%v); uploading full rootfs range\n", snap.ID, err)
			if fi, serr := os.Stat(snap.RootfsPath); serr == nil {
				rootfsRanges = []provisioner.Range{{Off: 0, Len: fi.Size()}}
			} else {
				fmt.Fprintf(os.Stderr, "[snapshot %s] upload aborted: %v\n", snap.ID, serr)
				return
			}
		}
	}

	var memBytes, rootfsBytes int64
	var err error
	if snap.Format == registry.FormatDiff {
		// The diff mem file is sparse (data = dirty pages) — PutSparse encodes
		// exactly the delta.
		if rootfsBytes, err = s.blob.PutRanges(ctx, snapObj(snap.ID, "rootfs.sz"), snap.RootfsPath, toBlobRanges(rootfsRanges)); err == nil {
			memBytes, err = s.blob.PutSparse(ctx, snapObj(snap.ID, "mem.sz"), snap.MemPath)
		}
	} else {
		if rootfsBytes, err = s.blob.PutSparse(ctx, snapObj(snap.ID, "rootfs.sz"), snap.RootfsPath); err == nil {
			memBytes, err = s.blob.PutSparse(ctx, snapObj(snap.ID, "mem.sz"), snap.MemPath)
		}
	}
	if err == nil {
		_, err = s.blob.PutSparse(ctx, snapObj(snap.ID, "state.sz"), snap.StatePath)
	}
	if err == nil {
		var meta []byte
		if meta, err = json.Marshal(snap); err == nil {
			err = s.blob.PutBytes(ctx, snapObj(snap.ID, "meta.json"), meta)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[snapshot %s] upload failed (snapshot stays host-local): %v\n", snap.ID, err)
		return
	}
	fmt.Fprintf(os.Stderr, "[snapshot %s] uploaded to gs://%s (%s): mem=%dMiB rootfs=%dMiB payload in %s\n",
		snap.ID, s.blob.Bucket(), snap.Format, memBytes>>20, rootfsBytes>>20, time.Since(t0).Round(time.Millisecond))
}

// ensureBaseUploaded uploads a golden snapshot's mem+rootfs as an immutable
// base template, once. The "complete" marker commits it; meta.json of any
// snapshot referencing the base is only uploaded after this returns.
func (s *Server) ensureBaseUploaded(ctx context.Context, base registry.Snapshot) error {
	s.baseUpMu.Lock()
	defer s.baseUpMu.Unlock()
	if s.basesUploaded[base.ID] {
		return nil
	}
	if ok, err := s.blob.Exists(ctx, baseObj(base.ID, "complete")); err != nil {
		return err
	} else if ok {
		s.basesUploaded[base.ID] = true
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
	s.basesUploaded[base.ID] = true
	fmt.Fprintf(os.Stderr, "[base %s] uploaded: mem=%dMiB rootfs=%dMiB payload in %s\n",
		base.ID, memBytes>>20, rootfsBytes>>20, time.Since(t0).Round(time.Millisecond))
	return nil
}

// pullLock returns a mutex dedicated to one pull key (snapshot or base id),
// so concurrent restores of the same id download once while different ids
// proceed in parallel.
func (s *Server) pullLock(key string) *sync.Mutex {
	s.pullMu.Lock()
	defer s.pullMu.Unlock()
	mu, ok := s.pulls[key]
	if !ok {
		mu = &sync.Mutex{}
		s.pulls[key] = mu
	}
	return mu
}

// ensureSnapshotLocal returns the snapshot row, pulling the snapshot down
// from GCS onto this host first when it isn't known locally — the path that
// makes any live host able to restore any snapshot, including ones whose
// creating host is gone.
func (s *Server) ensureSnapshotLocal(ctx context.Context, snapID string) (registry.Snapshot, error) {
	snap, err := s.reg.GetSnapshot(ctx, snapID)
	if err == nil || s.blob == nil {
		return snap, err
	}

	mu := s.pullLock("snap:" + snapID)
	mu.Lock()
	defer mu.Unlock()
	// Another request may have completed the pull while we waited.
	if snap, err := s.reg.GetSnapshot(ctx, snapID); err == nil {
		return snap, nil
	}

	metaBytes, err := s.blob.GetBytes(ctx, snapObj(snapID, "meta.json"))
	if err != nil {
		if errors.Is(err, gcsblob.ErrNotExist) {
			return registry.Snapshot{}, fmt.Errorf("not in local registry or gs://%s", s.blob.Bucket())
		}
		return registry.Snapshot{}, fmt.Errorf("fetch snapshot meta: %w", err)
	}
	var meta registry.Snapshot
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
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

// deleteSnapshotObjects removes a deleted snapshot's GCS objects, meta.json
// first so the snapshot stops being restorable before its data disappears.
// Best-effort: leftovers cost pennies and can't be restored without meta.
func (s *Server) deleteSnapshotObjects(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, name := range []string{"meta.json", "mem.sz", "rootfs.sz", "state.sz"} {
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
