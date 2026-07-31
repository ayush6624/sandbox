package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

func TestIdleHibernateRechecksRecentActivityUnderLock(t *testing.T) {
	s, reg := testLifecycleServer(t)
	s.cfg.HibernateAfter = time.Hour
	sb, err := reg.Create(context.Background(), "idle-race", "", filepath.Join(t.TempDir(), "rootfs"), nil, "", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	s.machines.Store(sb.ID, &vm.Machine{})
	s.act.touch(sb.ID)
	s.act.mu.Lock()
	s.act.entries[sb.ID].last = time.Now().Add(-2 * time.Hour)
	s.act.mu.Unlock()

	// Model the reaper having selected the stale idle row but not yet acquired
	// its lifecycle lock. A request completes while it waits.
	lifecycle := s.wakeLock(sb.ID)
	lifecycle.Lock()
	done := make(chan error, 1)
	go func() { done <- s.hibernateIfIdle(context.Background(), sb.ID) }()
	requestDone := s.act.begin(sb.ID)
	requestDone()
	lifecycle.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := reg.Get(context.Background(), sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != registry.StatusRunning {
		t.Fatalf("recently active sandbox status = %q, want running", got.Status)
	}
}

func TestExpiredSandboxRecheckHonorsExtendedDeadline(t *testing.T) {
	s, reg := testLifecycleServer(t)
	past := time.Now().Add(-time.Minute)
	sb, err := reg.Create(context.Background(), "ttl-race", "", filepath.Join(t.TempDir(), "rootfs"), &past, "", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := reg.SetExpiry(context.Background(), sb.ID, &future); err != nil {
		t.Fatal(err)
	}
	if err := s.destroyExpired(context.Background(), sb.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Get(context.Background(), sb.ID); err != nil {
		t.Fatalf("extended sandbox was destroyed: %v", err)
	}
}

func TestExpiredSnapshotRecheckHonorsExtendedDeadline(t *testing.T) {
	s, reg := testLifecycleServer(t)
	past := time.Now().Add(-time.Minute)
	snap := registry.Snapshot{
		ID: "ttl-snapshot", SourceID: "source", CreatedAt: time.Now(),
		MemPath: "/tmp/mem", StatePath: "/tmp/state", RootfsPath: "/tmp/rootfs",
		ExpiresAt: &past,
	}
	if err := reg.CreateSnapshot(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if _, err := reg.SetSnapshotPublicFields(context.Background(), snap.ID, "", &future); err != nil {
		t.Fatal(err)
	}
	if err := s.deleteExpiredSnapshot(context.Background(), snap.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.GetSnapshot(context.Background(), snap.ID); err != nil {
		t.Fatalf("extended snapshot was deleted: %v", err)
	}
}

func TestShutdownCleansStartingRowMissedByMachineRange(t *testing.T) {
	s, reg := testLifecycleServer(t)
	sb, err := reg.CreateStarting(context.Background(), "late-start", "", filepath.Join(t.TempDir(), "rootfs"), nil, "", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// There is deliberately no machines entry: this is the window before a
	// bring-up publishes its VMM, which sync.Map.Range alone cannot observe.
	s.shutdownAll()
	if _, err := reg.Get(context.Background(), sb.ID); err == nil {
		t.Fatal("shutdown left a starting straggler row behind")
	}
}
