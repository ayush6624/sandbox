package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
)

func TestCreateCommandRetainsRootfsFailureThroughTeardown(t *testing.T) {
	s := createRequestServer(t)
	t.Setenv("PATH", t.TempDir())
	s.cfg.Provisioner.RootfsBase = filepath.Join(t.TempDir(), "private-missing-rootfs")
	s.cfg.Provisioner.RootfsDir = filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(s.cfg.Provisioner.RootfsDir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	command := createops.Command{RegistryID: s.reg.RegistryID(), Members: []createops.Member{{ID: "rootfs-failure", Spec: createops.Spec{}}}}
	for replay := 0; replay < 2; replay++ {
		outcomes, err := s.Execute(context.Background(), createops.Worker{RegistryID: s.reg.RegistryID()}, command)
		if err != nil || len(outcomes) != 1 || outcomes[0].Failure == nil {
			t.Fatalf("execute: %+v %v", outcomes, err)
		}
		failure := outcomes[0].Failure
		if failure.Code != "rootfs_prepare_failed" || !strings.Contains(failure.Detail, "filesystem") {
			t.Fatalf("failure cause lost during teardown: %+v", failure)
		}
		if strings.Contains(failure.Detail, "private-missing-rootfs") {
			t.Fatalf("internal path exposed: %+v", failure)
		}
	}
	rows, err := s.reg.All(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("failed create retained allocation: %+v %v", rows, err)
	}
	progress, err := s.reg.CreateProgress(context.Background(), command.Members[0].ID)
	if err != nil || progress.Condition != "failed" || progress.Current.Stage != registry.CreateStagePreparing || progress.LastCompleted == nil || progress.LastCompleted.Stage != registry.CreateStageAllocated || progress.Outcome == nil || progress.Outcome.Code != "rootfs_prepare_failed" {
		t.Fatalf("rootfs failure lost its stage or cause: %+v %v", progress, err)
	}
}

func TestPendingCreateFailureIsUnresolvedUntilReconciled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, canceled := range []bool{false, true} {
		s := createRequestServer(t)
		ctx, cancel := context.WithCancel(context.Background())
		member := createops.Member{ID: "pending-cause", Spec: createops.Spec{}}
		intent := memberIntent(t, member)
		if _, err := s.reg.CreateStarting(ctx, "allocated", "", filepath.Join(t.TempDir(), "disk"), nil, "", 0, 1, 128, intent); err != nil {
			t.Fatal(err)
		}
		if canceled {
			cancel()
		}
		s.recordCreateFailure(ctx, "allocated", createFailureIdentity)
		cancel()
		pending, err := s.reg.CreateRequest(context.Background(), intent)
		if err != nil {
			t.Fatal(err)
		}
		out, err := s.createOutcome(context.Background(), member.ID, pending)
		if err == nil || out.Failure != nil || out.Sandbox != nil {
			t.Fatalf("unresolved allocation became terminal: %+v %v", out, err)
		}
		s.reconcile(context.Background())
		command := createops.Command{RegistryID: s.reg.RegistryID(), Members: []createops.Member{member}}
		outcomes, err := s.Execute(context.Background(), createops.Worker{RegistryID: s.reg.RegistryID()}, command)
		want := "guest_identity_failed"
		if canceled {
			want = "create_interrupted"
		}
		if err != nil || len(outcomes) != 1 || outcomes[0].Failure == nil || outcomes[0].Failure.Code != want {
			t.Fatalf("canceled=%v: want %s, got %+v %v", canceled, want, outcomes, err)
		}
	}
}
