package server

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
)

func waitCreateStage(t *testing.T, s *Server, id string, stage registry.CreateStage) registry.CreateProgress {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		progress, err := s.reg.CreateProgress(ctx, id)
		if err == nil && progress.Current.Stage == stage {
			return progress
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("request %s never reached %s", id, stage)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestCreateProgressAdmissionCancellationAndWarmRetry(t *testing.T) {
	s := createRequestServer(t)
	s.createSem = make(chan struct{}, 1)
	s.createSem <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	member := createops.Member{ID: "admission-progress", Spec: createops.Spec{}}
	command := createops.Command{RegistryID: s.reg.RegistryID(), Members: []createops.Member{member}}
	worker := createops.Worker{RegistryID: s.reg.RegistryID()}
	done := make(chan struct{})
	var executionErr error
	go func() { _, executionErr = s.Execute(ctx, worker, command); close(done) }()
	defer func() { cancel(); <-done }()
	first := waitCreateStage(t, s, member.ID, registry.CreateStageAdmission)
	if first.Attempt != 1 || first.Condition != "active" || first.Outcome != nil {
		t.Fatalf("admission observation: %+v", first)
	}
	if _, err := s.reg.CreateRequest(context.Background(), memberIntent(t, member)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("admission allocated: %v", err)
	}
	cancel()
	<-done
	if !errors.Is(executionErr, context.Canceled) {
		t.Fatalf("cancellation became terminal: %v", executionErr)
	}
	<-s.createSem
	if _, err := s.reg.CreateWarmForTemplate(context.Background(), "warm", "/tmp/unused-progress-disk", "golden", "golden", 1, 128); err != nil {
		t.Fatal(err)
	}
	if err := s.reg.MarkWarmReady(context.Background(), "warm"); err != nil {
		t.Fatal(err)
	}
	s.golden.Store(&registry.Snapshot{ID: "golden", Golden: true})
	outcomes, err := s.Execute(context.Background(), worker, command)
	if err != nil || len(outcomes) != 1 || outcomes[0].Sandbox == nil || outcomes[0].Sandbox.ID != "warm" {
		t.Fatalf("warm retry: %+v %v", outcomes, err)
	}
	ready, err := s.reg.CreateProgress(context.Background(), member.ID)
	if err != nil || ready.Attempt != 2 || ready.Sequence <= first.Sequence || ready.Current.Stage != registry.CreateStageReady || ready.Condition != "succeeded" {
		t.Fatalf("retry progress: %+v %v", ready, err)
	}
	if ready.LastCompleted == nil || ready.LastCompleted.Stage != registry.CreateStageReady || ready.Outcome == nil || ready.Outcome.Sandbox == nil || ready.Outcome.Sandbox.ID != "warm" {
		t.Fatalf("warm result not committed with progress: %+v", ready)
	}
	if _, err := s.Execute(context.Background(), worker, command); err != nil {
		t.Fatal(err)
	}
	replayed, err := s.reg.CreateProgress(context.Background(), member.ID)
	if err != nil || replayed.Sequence != ready.Sequence || replayed.Attempt != 2 {
		t.Fatalf("terminal replay rewrote progress: %+v %v", replayed, err)
	}
}

func TestCreateProgressSourceReadDoesNotWaitForExecutionLock(t *testing.T) {
	s := createRequestServer(t)
	op := s.snapshotLock("missing-source")
	op.Lock()
	locked := true
	ctx, cancel := context.WithCancel(context.Background())
	member := createops.Member{ID: "source-progress", Spec: createops.Spec{Source: createops.Source{Type: "snapshot", ID: "missing-source"}}}
	command := createops.Command{RegistryID: s.reg.RegistryID(), Members: []createops.Member{member}}
	done := make(chan struct{})
	go func() { _, _ = s.Execute(ctx, createops.Worker{RegistryID: s.reg.RegistryID()}, command); close(done) }()
	defer func() {
		cancel()
		if locked {
			op.Unlock()
		}
		<-done
	}()
	progress := waitCreateStage(t, s, member.ID, registry.CreateStageSource)
	if progress.LastCompleted == nil || progress.LastCompleted.Stage != registry.CreateStageAdmission || progress.Outcome != nil {
		t.Fatalf("source progress: %+v", progress)
	}
	select {
	case <-done:
		t.Fatal("create escaped source lock")
	default:
	}
	op.Unlock()
	locked = false
	<-done
	failed, err := s.reg.CreateProgress(context.Background(), member.ID)
	if err != nil || failed.Condition != "failed" || failed.Current.Stage != registry.CreateStageSource || failed.Outcome == nil || failed.Outcome.Code != "source_not_found" {
		t.Fatalf("source failure progress: %+v %v", failed, err)
	}
	if failed.LastCompleted == nil || failed.LastCompleted.Stage != registry.CreateStageAdmission {
		t.Fatalf("failure erased completed stage: %+v", failed)
	}
}
