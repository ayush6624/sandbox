package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

func TestSnapshotPinsActivityBeforeLifecycleLock(t *testing.T) {
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"), registry.Pools{
		TapPrefix: "fc", TapMax: 1,
		GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.10",
		PortMin: 5200, PortMax: 5200,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	s := New(Config{}, reg)
	t.Cleanup(s.pf.CloseAll)
	if _, err := reg.Create(context.Background(), "sb", "", "/tmp/rootfs", nil, "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}

	// Model hibernate already owning the per-sandbox lifecycle lock. A
	// snapshot request must mark the sandbox busy and then wait, rather than
	// racing the same Firecracker process.
	lifecycle := s.wakeLock("sb")
	lifecycle.Lock()
	result := make(chan int, 1)
	go func() {
		_, status, _ := s.snapshotSandbox(context.Background(), "sb", false, "", nil)
		result <- status
	}()

	deadline := time.Now().Add(time.Second)
	for {
		if _, busy, ok := s.act.idleFor("sb"); ok && busy {
			break
		}
		if time.Now().After(deadline) {
			lifecycle.Unlock()
			t.Fatal("snapshot did not pin activity before waiting for lifecycle lock")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case status := <-result:
		lifecycle.Unlock()
		t.Fatalf("snapshot returned %d while lifecycle lock was held", status)
	default:
	}

	lifecycle.Unlock()
	if status := <-result; status != 409 {
		t.Fatalf("snapshot status = %d, want 409 without a live Machine", status)
	}
	if _, busy, _ := s.act.idleFor("sb"); busy {
		t.Fatal("snapshot left sandbox pinned after returning")
	}
}

func TestSnapshotRejectsHibernatedRowBeforeMachineAccess(t *testing.T) {
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"), registry.Pools{
		TapPrefix: "fc", TapMax: 1,
		GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.10",
		PortMin: 5200, PortMax: 5200,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	s := New(Config{}, reg)
	t.Cleanup(s.pf.CloseAll)
	if _, err := reg.Create(context.Background(), "sb", "", "/tmp/rootfs", nil, "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := reg.Hibernate(context.Background(), "sb"); err != nil {
		t.Fatal(err)
	}

	_, status, err := s.snapshotSandbox(context.Background(), "sb", false, "", nil)
	if status != 409 || err == nil {
		t.Fatalf("snapshot hibernated row = status %d err %v, want 409", status, err)
	}
}
