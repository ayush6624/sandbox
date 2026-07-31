package server

import (
	"testing"
	"time"
)

func TestSnapshotStageLockSerializesSameBakedPath(t *testing.T) {
	s := &Server{}
	first := s.snapshotStageLock("/rootfs/source.ext4")
	if first != s.snapshotStageLock("/rootfs/source.ext4") {
		t.Fatal("same baked path returned different locks")
	}
	if first == s.snapshotStageLock("/rootfs/other.ext4") {
		t.Fatal("different baked paths unexpectedly share a lock")
	}

	first.Lock()
	acquired := make(chan struct{})
	go func() {
		s.snapshotStageLock("/rootfs/source.ext4").Lock()
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("concurrent load acquired the same baked path before release")
	case <-time.After(25 * time.Millisecond):
	}
	first.Unlock()

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("concurrent load did not acquire the baked path after release")
	}
}
