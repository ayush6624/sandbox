//go:build linux

package server

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

func TestReconstructHibArtifactsRejectsChunkMemoryMismatch(t *testing.T) {
	s, _ := testLifecycleServer(t)
	s.cfg.Provisioner.RootfsDir = t.TempDir()
	store := newUploadTestStore(t)
	s.blob = store.client
	s.cfg.UFFDRestore = true
	ctx := context.Background()
	mem, state, rootfs, err := s.cfg.Provisioner.SnapshotPaths(hibID("lazy"))
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 2<<20)
	data[0] = 1
	if err := os.WriteFile(mem, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs, []byte("rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.uploadMemChunks(ctx, "lazy", mem, 2<<20, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.blob.PutSparse(ctx, hibStateObj("lazy"), state); err != nil {
		t.Fatal(err)
	}
	if _, err := s.blob.PutSparse(ctx, hibRootfsObj("lazy"), rootfs); err != nil {
		t.Fatal(err)
	}
	_, err = s.reconstructHibArtifacts(ctx, &hibRecord{ID: "lazy", MemForm: memFormChunked, MemMIB: 1, RootfsForm: rootfsFormFull}, s.cfg.Provisioner.RootfsPathFor("lazy"))
	if err == nil {
		t.Fatal("chunk source accepted record with mismatched memory")
	}
}

func TestReconstructChunkedHibernationDefersMissingRAMChunk(t *testing.T) {
	s, _ := testLifecycleServer(t)
	s.cfg.Provisioner.RootfsDir = t.TempDir()
	store := newUploadTestStore(t)
	s.blob = store.client
	s.cfg.UFFDRestore = true
	ctx := context.Background()
	mem, state, rootfs, err := s.cfg.Provisioner.SnapshotPaths(hibID("lazy-ok"))
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 2<<20)
	data[0] = 1
	if err = os.WriteFile(mem, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(state, []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(rootfs, []byte("root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = s.uploadMemChunks(ctx, "lazy-ok", mem, 2<<20, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.blob.PutSparse(ctx, hibStateObj("lazy-ok"), state); err != nil {
		t.Fatal(err)
	}
	if _, err = s.blob.PutSparse(ctx, hibRootfsObj("lazy-ok"), rootfs); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	for key := range store.objects {
		if len(key) > 7 && key[:7] == "chunks/" {
			delete(store.objects, key)
		}
	}
	store.mu.Unlock()
	_ = os.Remove(mem)
	staged, err := s.reconstructHibArtifacts(ctx, &hibRecord{ID: "lazy-ok", MemForm: memFormChunked, MemMIB: 2, RootfsForm: rootfsFormFull}, s.cfg.Provisioner.RootfsPathFor("lazy-ok"))
	if err != nil || staged.Chunks == nil || staged.MemPath != "" {
		t.Fatalf("staged=%+v err=%v", staged, err)
	}
	if _, err := staged.Chunks.Load(0); err == nil {
		t.Fatal("missing chunk loaded during or after staging")
	}
}

func TestReconstructOldChunkedRecordInfersZeroMemMIB(t *testing.T) {
	s, _ := testLifecycleServer(t)
	s.cfg.Provisioner.RootfsDir = t.TempDir()
	store := newUploadTestStore(t)
	s.blob = store.client
	s.cfg.UFFDRestore = true
	ctx := context.Background()
	mem, state, root, err := s.cfg.Provisioner.SnapshotPaths(hibID("old-zero"))
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 2<<20)
	data[0] = 1
	if err = os.WriteFile(mem, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(state, []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(root, []byte("root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = s.uploadMemChunks(ctx, "old-zero", mem, 2<<20, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.blob.PutSparse(ctx, hibStateObj("old-zero"), state); err != nil {
		t.Fatal(err)
	}
	if _, err = s.blob.PutSparse(ctx, hibRootfsObj("old-zero"), root); err != nil {
		t.Fatal(err)
	}
	rec := &hibRecord{ID: "old-zero", MemForm: memFormChunked, RootfsForm: rootfsFormFull}
	if _, err = s.reconstructHibArtifacts(ctx, rec, s.cfg.Provisioner.RootfsPathFor("old-zero")); err != nil {
		t.Fatal(err)
	}
	if rec.MemMIB != 2 {
		t.Fatalf("MemMIB=%d", rec.MemMIB)
	}
}

func TestEnsureBaseRootfsLocalDoesNotFetchBaseMemory(t *testing.T) {
	s, _ := testLifecycleServer(t)
	s.cfg.Provisioner.RootfsDir = t.TempDir()
	store := newUploadTestStore(t)
	s.blob = store.client
	root := t.TempDir() + "/base-rootfs"
	if err := os.WriteFile(root, []byte("base rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.blob.PutSparse(context.Background(), baseObj("cold-base", "rootfs.sz"), root); err != nil {
		t.Fatal(err)
	}
	got, err := s.ensureBaseRootfsLocal(context.Background(), "cold-base")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(got); err != nil || string(data) != "base rootfs" {
		t.Fatalf("rootfs=%q err=%v", data, err)
	}
	mem, _ := s.baseCachePaths("cold-base")
	if _, err := os.Stat(mem); !os.IsNotExist(err) {
		t.Fatalf("base memory cache exists or stat failed: %v", err)
	}
	store.mu.Lock()
	_, fetchedMem := store.objects[baseObj("cold-base", "mem.sz")]
	store.mu.Unlock()
	if fetchedMem {
		t.Fatal("test fixture unexpectedly contained base memory")
	}
}

func TestNormalizedDiffHibernationPublishesFullChunkManifest(t *testing.T) {
	s, reg := testLifecycleServer(t)
	s.cfg.Provisioner.RootfsDir = t.TempDir()
	store := newUploadTestStore(t)
	s.blob = store.client
	ctx := context.Background()
	baseMem, _, baseRoot, err := s.cfg.Provisioner.SnapshotPaths("base")
	if err != nil {
		t.Fatal(err)
	}
	base := make([]byte, 2<<20)
	copy(base[:], []byte("unchanged-base"))
	if err = os.WriteFile(baseMem, base, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(baseRoot, []byte("root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = reg.CreateSnapshot(ctx, registry.Snapshot{ID: "base", MemPath: baseMem, RootfsPath: baseRoot, Golden: true, Format: registry.FormatFull}); err != nil {
		t.Fatal(err)
	}
	mem, state, root, err := s.cfg.Provisioner.SnapshotPaths(hibID("diff"))
	if err != nil {
		t.Fatal(err)
	}
	diffFile, err := os.Create(mem)
	if err != nil {
		t.Fatal(err)
	}
	if err = diffFile.Truncate(2 << 20); err == nil {
		_, err = diffFile.WriteAt([]byte("dirty"), 4096)
	}
	if closeErr := diffFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(state, []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(root, []byte("root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(hibDiffMarker(mem), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := reg.CreateStarting(ctx, "diff", "", s.cfg.Provisioner.RootfsPathFor("diff"), nil, "base", 0, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	s.uploadHibernation(ctx, "diff", sb, mem, state, root, vm.SnapshotDiff, "base", nil)
	rec, err := s.fetchHibRecord(ctx, "diff")
	if err != nil || rec.MemForm != memFormChunked {
		t.Fatalf("record=%+v err=%v", rec, err)
	}
	if _, err = os.Stat(hibDiffMarker(mem)); err != nil {
		t.Fatalf("diff marker removed: %v", err)
	}
	src, err := s.loadHibChunkSource(ctx, "diff")
	if err != nil {
		t.Fatal(err)
	}
	got, err := src.Load(0)
	if err != nil {
		t.Fatal(err)
	}
	expected := append([]byte(nil), base...)
	copy(expected[4096:], []byte("dirty"))
	if !bytes.Equal(got[:len(base)], expected) {
		t.Fatal("manifest did not contain base plus dirty overlay")
	}
}
