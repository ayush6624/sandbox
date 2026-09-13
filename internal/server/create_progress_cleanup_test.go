package server

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

func TestCreateProgressCloneWriteFailureRetainsUnstoppedVM(t *testing.T) {
	s := createRequestServer(t)
	t.Setenv("PATH", t.TempDir())
	ctx := context.Background()
	intent := memberIntent(t, createops.Member{ID: "clone-write-failure"})
	p, err := s.reg.BeginCreateProgress(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	intent.ProgressAttempt = p.Attempt
	if err := s.reg.AdvanceCreateProgress(ctx, intent, registry.CreateStageSource); err != nil {
		t.Fatal(err)
	}
	sb, err := s.reg.CreateStarting(ctx, "clone", "", filepath.Join(t.TempDir(), "disk"), nil, "", 0, 1, 128, intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reg.AdvanceCreateProgress(ctx, intent, registry.CreateStageVM); err != nil {
		t.Fatal(err)
	}
	ageLedger(t, s.reg.Path(), `CREATE TRIGGER reject_network_progress BEFORE UPDATE ON create_progress WHEN json_extract(NEW.snapshot, '$.Current.Stage') = 'guest_network' BEGIN SELECT RAISE(ABORT, 'injected network progress failure'); END`)
	m := &vm.Machine{}
	stopErr := errors.New("injected stop failure")
	stops := 0
	s.stopMachineFn = func(got *vm.Machine) error {
		stops++
		if got != m {
			t.Fatal("stopped a different machine")
		}
		return stopErr
	}
	c := &clone{sb: sb, m: m, progressIntent: intent}
	if err := s.finishClone(ctx, c); err == nil || !strings.Contains(err.Error(), "injected network progress failure") {
		t.Fatalf("finish: %v", err)
	}
	if err := s.destroy(ctx, sb.ID); !errors.Is(err, stopErr) {
		t.Fatalf("destroy did not retain stop failure: %v", err)
	}
	if stops != 1 {
		t.Fatalf("stop calls: %d", stops)
	}
	if got, ok := s.machines.Load(sb.ID); !ok || got != m {
		t.Fatal("lost live machine")
	}
	if _, err := s.reg.Get(ctx, sb.ID); err != nil {
		t.Fatalf("deleted unresolved sandbox: %v", err)
	}
	result, err := s.reg.CreateRequest(ctx, intent)
	if err != nil || result.Phase != "allocated" {
		t.Fatalf("terminalized unstopped clone: %+v %v", result, err)
	}
	p, err = s.reg.CreateProgress(ctx, intent.ID)
	if err != nil || p.Outcome != nil || p.Current.Stage != registry.CreateStageVM {
		t.Fatalf("invented network progress: %+v %v", p, err)
	}
	s.stopMachineFn = func(*vm.Machine) error { return nil }
	if err := s.destroy(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	result, err = s.reg.CreateRequest(ctx, intent)
	if err != nil || result.Phase != "failed" {
		t.Fatalf("confirmed teardown: %+v %v", result, err)
	}
}

func TestCreateProgressWarmWriteFailureStopsBeforeClone(t *testing.T) {
	s := createRequestServer(t)
	ctx := context.Background()
	if _, err := s.reg.CreateWarmForTemplate(ctx, "warm", filepath.Join(t.TempDir(), "disk"), "golden", "golden", 1, 128); err != nil {
		t.Fatal(err)
	}
	if err := s.reg.MarkWarmReady(ctx, "warm"); err != nil {
		t.Fatal(err)
	}
	s.golden.Store(&registry.Snapshot{ID: "golden", Golden: true})
	intent := memberIntent(t, createops.Member{ID: "warm-write-failure"})
	p, err := s.reg.BeginCreateProgress(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	intent.ProgressAttempt = p.Attempt
	ageLedger(t, s.reg.Path(), `CREATE TRIGGER reject_ready_progress BEFORE UPDATE ON create_progress WHEN json_extract(NEW.snapshot, '$.Condition') = 'succeeded' BEGIN SELECT RAISE(ABORT, 'injected ready progress failure'); END`)
	_, err = s.createRequested(ctx, createops.Spec{}, intent, "")
	if err == nil || !strings.Contains(err.Error(), "injected ready progress failure") {
		t.Fatalf("warm write failure fell through to clone preparation: %v", err)
	}
	rows, err := s.reg.All(ctx)
	if err != nil || len(rows) != 1 || rows[0].ID != "warm" || rows[0].Status != registry.StatusWarming {
		t.Fatalf("warm transaction leaked allocation: %+v %v", rows, err)
	}
	if _, err := s.reg.CreateRequest(ctx, intent); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed claim allocated: %v", err)
	}
	p, err = s.reg.CreateProgress(ctx, intent.ID)
	if err != nil || p.Current.Stage != registry.CreateStageSource || p.Outcome != nil {
		t.Fatalf("failed claim advanced: %+v %v", p, err)
	}
	if s.met.warmMisses.Load() != 0 {
		t.Fatal("database failure counted as an empty pool")
	}
}
