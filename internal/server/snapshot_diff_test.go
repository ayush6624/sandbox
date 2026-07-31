package server

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
)

func TestSnapshotDiffPlanFlattensUserDeltaToGolden(t *testing.T) {
	dir := t.TempDir()
	reg, err := registry.Open(filepath.Join(dir, "registry.db"), registry.Pools{
		TapPrefix: "fc", TapMax: 1,
		GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.10",
		PortMin: 5200, PortMax: 5200,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	p := &provisioner.Provisioner{SnapshotDir: filepath.Join(dir, "snapshots")}
	s := New(Config{Provisioner: p}, reg)
	t.Cleanup(s.pf.CloseAll)

	goldenMem := filepath.Join(dir, "golden.mem")
	goldenRoot := filepath.Join(dir, "golden.ext4")
	parentMem := filepath.Join(dir, "parent.mem")
	for _, path := range []string{goldenMem, goldenRoot, parentMem} {
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	golden := registry.Snapshot{
		ID: "golden", SourceID: "source", MemPath: goldenMem,
		StatePath: filepath.Join(dir, "golden.state"), RootfsPath: goldenRoot,
		SourceRootfsPath: filepath.Join(dir, "source.ext4"), CreatedAt: now,
		Golden: true, Format: registry.FormatFull,
	}
	if err := reg.CreateSnapshot(context.Background(), golden); err != nil {
		t.Fatal(err)
	}
	parent := registry.Snapshot{
		ID: "parent", SourceID: "child", MemPath: parentMem,
		StatePath: filepath.Join(dir, "parent.state"), RootfsPath: filepath.Join(dir, "parent.ext4"),
		SourceRootfsPath: filepath.Join(dir, "child.ext4"), CreatedAt: now,
		Format: registry.FormatDiff, BaseID: golden.ID,
	}
	if err := reg.CreateSnapshot(context.Background(), parent); err != nil {
		t.Fatal(err)
	}

	plan, ok := s.snapshotDiffPlan(context.Background(), parent.ID)
	if !ok {
		t.Fatal("user delta was not accepted as a differential parent")
	}
	if plan.goldenID != golden.ID || plan.goldenMemPath != golden.MemPath {
		t.Fatalf("plan = %+v, want golden=%q memory=%q", plan, golden.ID, golden.MemPath)
	}
	if plan.parent.ID != parent.ID {
		t.Fatalf("parent = %q, want %q", plan.parent.ID, parent.ID)
	}
}

func TestSnapshotDiffPlanRejectsFullUserSnapshot(t *testing.T) {
	dir := t.TempDir()
	reg, err := registry.Open(filepath.Join(dir, "registry.db"), registry.Pools{
		TapPrefix: "fc", TapMax: 1,
		GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.10",
		PortMin: 5200, PortMax: 5200,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	s := New(Config{Provisioner: &provisioner.Provisioner{SnapshotDir: filepath.Join(dir, "snapshots")}}, reg)
	t.Cleanup(s.pf.CloseAll)
	full := registry.Snapshot{
		ID: "full", SourceID: "source", MemPath: filepath.Join(dir, "full.mem"),
		StatePath: filepath.Join(dir, "full.state"), RootfsPath: filepath.Join(dir, "full.ext4"),
		SourceRootfsPath: filepath.Join(dir, "source.ext4"), CreatedAt: time.Now(),
		Format: registry.FormatFull,
	}
	if err := reg.CreateSnapshot(context.Background(), full); err != nil {
		t.Fatal(err)
	}
	if plan, ok := s.snapshotDiffPlan(context.Background(), full.ID); ok {
		t.Fatalf("full user snapshot unexpectedly produced plan %+v", plan)
	}
}

func TestFlattenSnapshotDiffLatestLayerWins(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sparse overlay uses Linux SEEK_DATA/SEEK_HOLE")
	}
	dir := t.TempDir()
	golden := filepath.Join(dir, "golden.mem")
	parent := filepath.Join(dir, "parent.full.mem")
	layer := filepath.Join(dir, "layer.mem")
	const size = 64 << 10
	for _, path := range []string{golden, parent, layer} {
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(size); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		_ = f.Close()
	}
	// Parent is a reflink of golden with one prior changed page.
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.CloneFile(golden, parent); err != nil {
		t.Fatal(err)
	}
	if f, err := os.OpenFile(parent, os.O_WRONLY, 0); err != nil {
		t.Fatal(err)
	} else {
		_, err = f.WriteAt([]byte("parent"), 4<<10)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if f, err := os.OpenFile(layer, os.O_WRONLY, 0); err != nil {
		t.Fatal(err)
	} else {
		_, err = f.WriteAt([]byte("child!"), 4<<10)
		if err == nil {
			_, err = f.WriteAt([]byte("second"), 12<<10)
		}
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{cfg: Config{Provisioner: &provisioner.Provisioner{}}}
	if err := s.flattenSnapshotDiff(parent, golden, layer); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 6)
	f, err := os.Open(layer)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.ReadAt(got, 4<<10); err != nil || string(got) != "child!" {
		t.Fatalf("overridden page = %q err=%v", got, err)
	}
	if _, err := f.ReadAt(got, 12<<10); err != nil || string(got) != "second" {
		t.Fatalf("new page = %q err=%v", got, err)
	}
	full, err := os.Open(filepath.Join(dir, "mem.full.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	if _, err := full.ReadAt(got, 4<<10); err != nil || string(got) != "child!" {
		t.Fatalf("materialized overridden page = %q err=%v", got, err)
	}
	if _, err := full.ReadAt(got, 12<<10); err != nil || string(got) != "second" {
		t.Fatalf("materialized new page = %q err=%v", got, err)
	}
}
