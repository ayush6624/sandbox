package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
)

func testLifecycleServer(t *testing.T) (*Server, *registry.Registry) {
	t.Helper()
	dir := t.TempDir()
	reg, err := registry.Open(filepath.Join(dir, "registry.db"), registry.Pools{
		TapPrefix: "fc", TapMax: 2,
		GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.11",
		PortMin: 5200, PortMax: 5201,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	s := New(Config{Provisioner: &provisioner.Provisioner{
		SnapshotDir: filepath.Join(dir, "snapshots"),
	}}, reg)
	t.Cleanup(s.pf.CloseAll)
	return s, reg
}

func cancellableTestUpload(t *testing.T) (*backgroundUpload, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	up := &backgroundUpload{cancel: cancel, done: make(chan struct{})}
	go func() {
		<-ctx.Done()
		close(stopped)
		close(up.done)
	}()
	return up, stopped
}

func TestDeleteSnapshotCancelsUploaderBeforeArtifacts(t *testing.T) {
	s, reg := testLifecycleServer(t)
	memPath, statePath, rootfsPath, err := s.cfg.Provisioner.SnapshotPaths("snap")
	if err != nil {
		t.Fatal(err)
	}
	snap := registry.Snapshot{
		ID: "snap", SourceID: "sb", MemPath: memPath,
		StatePath: statePath, RootfsPath: rootfsPath,
		CreatedAt: time.Now(),
	}
	for _, path := range []string{snap.MemPath, snap.StatePath, snap.RootfsPath} {
		if err := os.WriteFile(path, []byte("artifact"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.CreateSnapshot(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	up, stopped := cancellableTestUpload(t)
	s.snapshotUpMu.Lock()
	s.snapshotUploads[snap.ID] = up
	s.snapshotUpMu.Unlock()

	if err := s.deleteSnapshot(context.Background(), snap.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("delete returned before snapshot uploader stopped")
	}
	if _, err := reg.GetSnapshot(context.Background(), snap.ID); err == nil {
		t.Fatal("snapshot row survived delete")
	}
	for _, path := range []string{snap.MemPath, snap.StatePath, snap.RootfsPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("artifact %s survived delete: %v", path, err)
		}
	}
}

func TestDestroyHibernatedCancelsUploaderBeforeCleanup(t *testing.T) {
	s, reg := testLifecycleServer(t)
	rootfs := filepath.Join(t.TempDir(), "rootfs")
	if err := os.WriteFile(rootfs, []byte("rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(context.Background(), "sb", "", rootfs, nil, "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := reg.Hibernate(context.Background(), "sb"); err != nil {
		t.Fatal(err)
	}
	up, stopped := cancellableTestUpload(t)
	s.hibUpMu.Lock()
	s.hibUploads["sb"] = up
	s.hibUpMu.Unlock()

	if err := s.destroy(context.Background(), "sb"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("destroy returned before hibernation uploader stopped")
	}
	if _, err := reg.Get(context.Background(), "sb"); err == nil {
		t.Fatal("sandbox row survived destroy")
	}
}
