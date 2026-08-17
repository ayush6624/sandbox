package server

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

// fanoutRequest drives handleFanout for one snapshot id and reports when it
// returned. The bring-up itself cannot succeed in a unit test (no KVM), which is
// fine: every assertion here is about whether the handler got PAST the snapshot
// lock, not about clones coming up.
func fanoutRequest(s *Server, snapID string, count string) func() {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/snapshots/"+snapID+"/fanout", strings.NewReader(`{"count": `+count+`}`))
	r.SetPathValue("id", snapID)
	return func() { s.handleFanout(w, r) }
}

// Creates from one snapshot must not exclude each other. They used to: the
// snapshot lock was exclusive and held across the whole bring-up, so N creates
// from one snapshot ran strictly one at a time (measured dead-linear at ~756 ms
// per sandbox). The only thing that actually needed guarding was the staged
// rootfs path, which is permanent now — so consumers hold the lock SHARED.
func TestSnapshotConsumersDoNotExcludeEachOther(t *testing.T) {
	s, _ := capacityTestServer(t)
	if err := s.reg.CreateSnapshot(context.Background(),
		registry.Snapshot{ID: "snap-1", SourceID: "gone", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	// Hold the same snapshot in shared mode, as another in-flight restore would.
	held := s.snapshotLock("snap-1")
	held.RLock()
	defer held.RUnlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		fanoutRequest(s, "snap-1", "1")()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fanout blocked behind another reader of the same snapshot — the snapshot lock is still exclusive")
	}
}

// The shared mode must still fence a writer: a delete or metadata write may not
// proceed while a restore is reading the snapshot, and vice versa.
func TestSnapshotWriterExcludesConsumers(t *testing.T) {
	s, _ := capacityTestServer(t)
	if err := s.reg.CreateSnapshot(context.Background(),
		registry.Snapshot{ID: "snap-1", SourceID: "gone", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	held := s.snapshotLock("snap-1")
	held.Lock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		fanoutRequest(s, "snap-1", "1")()
	}()
	select {
	case <-done:
		held.Unlock()
		t.Fatal("fanout ran while an exclusive holder (delete/metadata write) held the snapshot")
	case <-time.After(50 * time.Millisecond):
	}
	held.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fanout never proceeded after the exclusive holder released")
	}
}

// A different snapshot id must never be involved.
func TestSnapshotLocksAreIndependentPerID(t *testing.T) {
	s, _ := capacityTestServer(t)
	held := s.snapshotLock("snap-1")
	held.Lock()
	defer held.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		fanoutRequest(s, "snap-2", "1")()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a fanout of snap-2 blocked on snap-1's lock")
	}
}

// ensureStagedRootfs must LEAVE the staged file behind — that permanence is what
// lets the snapshot lock be shared. Restore and fanout used to stage it per call
// and unlink it afterwards, making the path shared mutable state.
func TestStagedRootfsSurvivesAndIsSingleFlighted(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("staging uses `cp --reflink`, which is GNU coreutils only")
	}
	s, _ := capacityTestServer(t)
	dir := t.TempDir()
	snap := registry.Snapshot{
		ID:               "snap-1",
		RootfsPath:       filepath.Join(dir, "snapshot-rootfs.ext4"),
		SourceRootfsPath: filepath.Join(dir, "baked-rootfs.ext4"),
	}
	want := []byte("frozen rootfs contents")
	if err := os.WriteFile(snap.RootfsPath, want, 0o644); err != nil {
		t.Fatalf("write snapshot rootfs: %v", err)
	}

	// Concurrent stagers must not tear the destination: the stat+copy is guarded.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.ensureStagedRootfs(snap); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("ensureStagedRootfs: %v", err)
	}

	got, err := os.ReadFile(snap.SourceRootfsPath)
	if err != nil {
		t.Fatalf("staged rootfs must persist after staging: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("staged rootfs = %q, want %q", got, want)
	}
}

// removeStagedRootfs is the cleanup that permanence now requires: the baked path
// lives outside SnapshotDir, so CleanupSnapshot never covered it. It must not
// touch a LIVE source sandbox's rootfs, which is the same path when the sandbox
// the snapshot was taken from is still running.
func TestRemoveStagedRootfsSpareLiveSourceSandbox(t *testing.T) {
	s, reg := capacityTestServer(t)
	ctx := context.Background()
	dir := t.TempDir()
	staged := filepath.Join(dir, "rootfs-src.ext4")
	if err := os.WriteFile(staged, []byte("x"), 0o644); err != nil {
		t.Fatalf("write staged: %v", err)
	}
	sb, err := reg.Create(ctx, "src", "", staged, nil, "", 0, 0, 0)
	if err != nil {
		t.Fatalf("create source sandbox: %v", err)
	}
	snap := registry.Snapshot{ID: "snap-1", SourceID: sb.ID, SourceRootfsPath: staged}

	s.removeStagedRootfs(ctx, snap)
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("removed a live sandbox's own rootfs: %v", err)
	}

	if err := reg.Destroy(ctx, sb.ID); err != nil {
		t.Fatalf("destroy source sandbox: %v", err)
	}
	s.removeStagedRootfs(ctx, snap)
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged rootfs leaked after the source sandbox was gone (err=%v)", err)
	}
}

// The golden snapshot's staged file is deliberately permanent for the life of
// the host — every create clones it.
func TestRemoveStagedRootfsSparesGolden(t *testing.T) {
	s, _ := capacityTestServer(t)
	dir := t.TempDir()
	staged := filepath.Join(dir, "golden-baked.ext4")
	if err := os.WriteFile(staged, []byte("x"), 0o644); err != nil {
		t.Fatalf("write staged: %v", err)
	}
	s.removeStagedRootfs(context.Background(), registry.Snapshot{
		ID: "golden", SourceID: "gone", SourceRootfsPath: staged, Golden: true,
	})
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("golden staged rootfs must not be removed: %v", err)
	}
}
