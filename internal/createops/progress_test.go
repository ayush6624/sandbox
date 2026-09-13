package createops

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/google/uuid"
)

func progressFixture(t *testing.T) (*SQLiteStore, Worker, registry.CreateProgress) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "operations.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	req := request("operation", "key", "hash")
	if _, err := s.Accept(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	worker := Worker{HostID: "host", RegistryID: "registry"}
	if err := s.Assign(context.Background(), req.ID, []string{req.Members[0].ID}, worker); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(req.Members[0].Spec)
	now := time.Now().UTC()
	return s, worker, registry.CreateProgress{ID: req.Members[0].ID, RegistryID: worker.RegistryID, RequestHash: sha256.Sum256(spec), Owner: registry.CreateProgressOwner{CoordinatorID: s.CoordinatorID(), OperationID: req.ID}, Target: registry.CreateProgressLocal, Attempt: 1, Sequence: 1, Condition: "active", Current: registry.CreateStageMark{Stage: registry.CreateStageAdmission, Attempt: 1, StartedAt: now}, ObservedAt: now}
}

func failureProgress(p registry.CreateProgress) registry.CreateProgress {
	p.Sequence++
	p.Condition = "failed"
	p.Outcome = &registry.CreateRequestResult{Phase: "failed", Status: 500, Code: "create_interrupted", Detail: "Interrupted."}
	return p
}

func TestCoordinatorIdentityAndProgressMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ops.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := s.CoordinatorID()
	if _, err := uuid.Parse(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accept(ctx, request("old", "key", "hash")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`ALTER TABLE create_members DROP COLUMN progress`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for i := 0; i < 2; i++ {
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if s.CoordinatorID() != id {
			t.Fatal("coordinator identity changed")
		}
		op, err := s.Get(ctx, "old")
		if err != nil || len(op.Members) != 1 || op.Members[0].Progress != nil {
			t.Fatalf("migration: %+v %v", op, err)
		}
		s.Close()
	}
	other, err := Open(filepath.Join(t.TempDir(), "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.CoordinatorID() == id {
		t.Fatal("new database reused coordinator identity")
	}
}

func TestIngestProgressReorderAndFences(t *testing.T) {
	s, w, p := progressFixture(t)
	ctx := context.Background()
	later := p
	later.Sequence = 4
	later.Attempt = 2
	later.Current.Attempt = 2
	for _, update := range []registry.CreateProgress{p, later, p, later} {
		ack, err := s.IngestProgress(ctx, w, update)
		if err != nil || ack != update.Sequence {
			t.Fatalf("ack=%d err=%v", ack, err)
		}
	}
	cases := []struct {
		name   string
		change func(*Worker, *registry.CreateProgress)
		want   error
	}{
		{"coordinator", func(_ *Worker, p *registry.CreateProgress) { p.Owner.CoordinatorID = "other" }, ErrProgressConflict},
		{"operation", func(_ *Worker, p *registry.CreateProgress) { p.Owner.OperationID = "missing" }, ErrNotFound},
		{"member", func(_ *Worker, p *registry.CreateProgress) { p.ID = "missing" }, ErrNotFound},
		{"host", func(w *Worker, _ *registry.CreateProgress) { w.HostID = "other" }, ErrProgressConflict},
		{"registry", func(w *Worker, _ *registry.CreateProgress) { w.RegistryID = "other" }, ErrProgressConflict},
		{"observation registry", func(_ *Worker, p *registry.CreateProgress) { p.RegistryID = "other" }, ErrProgressConflict},
		{"hash", func(_ *Worker, p *registry.CreateProgress) { p.RequestHash[0]++ }, ErrProgressConflict},
		{"same sequence mutation", func(_ *Worker, p *registry.CreateProgress) { p.Current.Stage = registry.CreateStageSource }, ErrProgressConflict},
		{"attempt regression", func(_ *Worker, p *registry.CreateProgress) { p.Sequence++; p.Attempt = 1; p.Current.Attempt = 1 }, ErrProgressConflict},
		{"target", func(_ *Worker, p *registry.CreateProgress) { p.Target = registry.CreateProgressGateway }, ErrProgressConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			update, worker := later, w
			tc.change(&worker, &update)
			ack, err := s.IngestProgress(ctx, worker, update)
			if ack != 0 || !errors.Is(err, tc.want) {
				t.Fatalf("ack=%d err=%v", ack, err)
			}
		})
	}
	op, err := s.Get(ctx, p.Owner.OperationID)
	if err != nil || op.Members[0].Progress.Sequence != later.Sequence {
		t.Fatalf("retained=%+v %v", op, err)
	}
}

func TestIngestRejectsInvalidObservations(t *testing.T) {
	changes := map[string]func(*registry.CreateProgress){
		"sequence":         func(p *registry.CreateProgress) { p.Sequence = 0 },
		"attempt":          func(p *registry.CreateProgress) { p.Attempt = 0 },
		"target":           func(p *registry.CreateProgress) { p.Target = "unknown" },
		"condition":        func(p *registry.CreateProgress) { p.Condition = "unknown" },
		"stage":            func(p *registry.CreateProgress) { p.Current.Stage = "unknown" },
		"mark attempt":     func(p *registry.CreateProgress) { p.Current.Attempt = 2 },
		"observed":         func(p *registry.CreateProgress) { p.ObservedAt = time.Time{} },
		"last incomplete":  func(p *registry.CreateProgress) { m := p.Current; p.LastCompleted = &m },
		"active outcome":   func(p *registry.CreateProgress) { p.Outcome = &registry.CreateRequestResult{Phase: "failed"} },
		"missing terminal": func(p *registry.CreateProgress) { p.Condition = "failed" },
		"invalid failure":  func(p *registry.CreateProgress) { *p = failureProgress(*p); p.Outcome.Status = 200 },
		"invalid success": func(p *registry.CreateProgress) {
			p.Condition = "succeeded"
			p.Outcome = &registry.CreateRequestResult{Phase: "succeeded"}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, w, p := progressFixture(t)
			change(&p)
			ack, err := s.IngestProgress(context.Background(), w, p)
			if ack != 0 || !errors.Is(err, ErrProgressConflict) {
				t.Fatalf("ack=%d err=%v", ack, err)
			}
		})
	}
}

func TestTerminalProgressCommitAndOutcomeAuthority(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "success"}[success], func(t *testing.T) {
			s, w, p := progressFixture(t)
			ctx := context.Background()
			p = failureProgress(p)
			if success {
				p.Condition = "succeeded"
				p.Outcome = &registry.CreateRequestResult{Phase: "succeeded", Sandbox: &registry.Sandbox{ID: "sandbox"}}
				p.Current.Stage = registry.CreateStageReady
				p.Current.CompletedAt = &p.ObservedAt
				m := p.Current
				p.LastCompleted = &m
			}
			if ack, err := s.IngestProgress(ctx, w, p); err != nil || ack != p.Sequence {
				t.Fatalf("ack=%d err=%v", ack, err)
			}
			op, err := s.Get(ctx, p.Owner.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if success {
				if op.CompletedAt != nil || op.Members[0].Outcome != nil {
					t.Fatal("success prematurely completed member")
				}
				pending, err := s.Pending(ctx)
				if err != nil || len(pending) != 1 {
					t.Fatalf("pending=%+v %v", pending, err)
				}
				op, err = s.Record(ctx, op.ID, []Outcome{{ID: p.ID, Sandbox: p.Outcome.Sandbox, Routable: true}})
				if err != nil {
					t.Fatal(err)
				}
			} else if op.Status != "failed" || op.Members[0].Outcome.Failure.Code != "create_interrupted" {
				t.Fatalf("failure=%+v", op)
			}
			completed := *op.CompletedAt
			if _, err := s.IngestProgress(ctx, w, p); err != nil {
				t.Fatal(err)
			}
			changed := p
			changed.Sequence++
			if _, err := s.IngestProgress(ctx, w, changed); !errors.Is(err, ErrProgressConflict) {
				t.Fatalf("terminal change: %v", err)
			}
			after, err := s.Record(ctx, op.ID, []Outcome{{ID: p.ID, Failure: &Failure{Status: 409, Code: "different"}}})
			if err != nil || !after.CompletedAt.Equal(completed) || after.Status != op.Status {
				t.Fatalf("completion rewritten: %+v %v", after, err)
			}
		})
	}
}

func TestIngestFailureRollsBackProgressAndCompletion(t *testing.T) {
	s, w, p := progressFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER reject_completion BEFORE UPDATE OF completed_at ON create_operations BEGIN SELECT RAISE(ABORT,'injected completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	p = failureProgress(p)
	if ack, err := s.IngestProgress(ctx, w, p); err == nil || ack != 0 {
		t.Fatalf("ack=%d err=%v", ack, err)
	}
	op, err := s.Get(ctx, p.Owner.OperationID)
	if err != nil || op.CompletedAt != nil || op.Members[0].Outcome != nil || op.Members[0].Progress != nil {
		t.Fatalf("partial commit: %+v %v", op, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_completion`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestProgress(ctx, w, p); err != nil {
		t.Fatal(err)
	}
}

func TestDelayedProgressPreservesRecordedOutcome(t *testing.T) {
	s, w, p := progressFixture(t)
	ctx := context.Background()
	first, err := s.Record(ctx, p.Owner.OperationID, []Outcome{{ID: p.ID, Sandbox: &registry.Sandbox{ID: "retained"}, Routable: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestProgress(ctx, w, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestProgress(ctx, w, failureProgress(p)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, first.ID)
	if err != nil || got.Members[0].Progress.Condition != "failed" || got.Members[0].Outcome.Sandbox.ID != "retained" || !got.CompletedAt.Equal(*first.CompletedAt) || got.Status != "succeeded" {
		t.Fatalf("recorded outcome changed: %+v %v", got, err)
	}
}

func TestProgressProjectionSurvivesReopen(t *testing.T) {
	s, w, p := progressFixture(t)
	ctx := context.Background()
	if _, err := s.IngestProgress(ctx, w, p); err != nil {
		t.Fatal(err)
	}
	var path string
	var seq int
	var name string
	if err := s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	id := s.CoordinatorID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.CoordinatorID() != id {
		t.Fatal("identity changed")
	}
	op, err := reopened.Get(ctx, p.Owner.OperationID)
	if err != nil || op.Members[0].Progress == nil || op.Members[0].Progress.Sequence != p.Sequence {
		t.Fatalf("progress lost: %+v %v", op, err)
	}
	if ack, err := reopened.IngestProgress(ctx, w, p); err != nil || ack != p.Sequence {
		t.Fatalf("reopen replay: %d %v", ack, err)
	}
}

func TestUnassignedMemberRejectsProgress(t *testing.T) {
	s, w, p := progressFixture(t)
	ctx := context.Background()
	req := request("unassigned", "other-key", "hash")
	if _, err := s.Accept(ctx, req); err != nil {
		t.Fatal(err)
	}
	p.Owner.OperationID = req.ID
	p.ID = req.Members[0].ID
	if ack, err := s.IngestProgress(ctx, w, p); ack != 0 || !errors.Is(err, ErrProgressConflict) {
		t.Fatalf("unassigned ack=%d err=%v", ack, err)
	}
}

func TestIngestRealWorkerTeardownObservation(t *testing.T) {
	ctx := context.Background()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "worker.db"), registry.Pools{TapPrefix: "fc", TapMax: 1, GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.10", PortMin: 5200, PortMax: 5200})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	s, err := Open(filepath.Join(t.TempDir(), "ops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	req := request("teardown", "teardown-key", "hash")
	if _, err := s.Accept(ctx, req); err != nil {
		t.Fatal(err)
	}
	worker := Worker{HostID: "host", RegistryID: reg.RegistryID()}
	member := req.Members[0]
	if err := s.Assign(ctx, req.ID, []string{member.ID}, worker); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(member.Spec)
	intent := registry.CreateIntent{ID: member.ID, Hash: sha256.Sum256(spec), SourceType: "default", ProgressOwner: registry.CreateProgressOwner{CoordinatorID: s.CoordinatorID(), OperationID: req.ID}, ProgressTarget: registry.CreateProgressLocal}
	first, err := reg.BeginCreateProgress(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	intent.ProgressAttempt = first.Attempt
	if err := reg.AdvanceCreateProgress(ctx, intent, registry.CreateStageSource); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateStarting(ctx, "sandbox", "", "/tmp/disk", nil, "", 0, 1, 128, intent); err != nil {
		t.Fatal(err)
	}
	if err := reg.RecordCreateFailure(ctx, "sandbox", registry.CreateFailure{Status: 500, Code: "create_failed", Detail: "Guest failed."}); err != nil {
		t.Fatal(err)
	}
	p, err := reg.CreateProgress(ctx, member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Condition != "tearing_down" {
		t.Fatalf("worker observation=%+v", p)
	}
	if ack, err := s.IngestProgress(ctx, worker, p); err != nil || ack != p.Sequence {
		t.Fatalf("teardown ack=%d err=%v", ack, err)
	}
	op, err := s.Get(ctx, req.ID)
	if err != nil || op.Members[0].Progress.Condition != "tearing_down" || op.Members[0].Outcome != nil || op.CompletedAt != nil {
		t.Fatalf("teardown completed member: %+v %v", op, err)
	}
}

func TestIngestProgressUsesSequenceDespiteClockSteps(t *testing.T) {
	s, w, p := progressFixture(t)
	ctx := context.Background()
	if _, err := s.IngestProgress(ctx, w, p); err != nil {
		t.Fatal(err)
	}
	p.Sequence++
	p.Current.Stage = registry.CreateStageSource
	completed := p.Current.StartedAt.Add(-time.Hour)
	previous := registry.CreateStageMark{Stage: registry.CreateStageAdmission, Attempt: p.Attempt, StartedAt: p.Current.StartedAt, CompletedAt: &completed}
	p.LastCompleted = &previous
	p.ObservedAt = p.ObservedAt.Add(-2 * time.Hour)
	if ack, err := s.IngestProgress(ctx, w, p); err != nil || ack != p.Sequence {
		t.Fatalf("clock step ack=%d err=%v", ack, err)
	}
	op, err := s.Get(ctx, p.Owner.OperationID)
	if err != nil || op.Members[0].Progress.Sequence != p.Sequence {
		t.Fatalf("sequence not retained: %+v %v", op, err)
	}
	invalid := p
	invalid.Sequence++
	zero := time.Time{}
	mark := *invalid.LastCompleted
	mark.CompletedAt = &zero
	invalid.LastCompleted = &mark
	if _, err := s.IngestProgress(ctx, w, invalid); !errors.Is(err, ErrProgressConflict) {
		t.Fatalf("zero completion accepted: %v", err)
	}
}

func TestWorkerProgressCapabilityPersistsAtAssignment(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ops.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	req := request("owned", "key", "hash")
	if _, err := s.Accept(ctx, req); err != nil {
		t.Fatal(err)
	}
	worker := Worker{HostID: "host", RegistryID: "registry", CreateProgress: true}
	if err := s.Assign(ctx, req.ID, []string{req.Members[0].ID}, worker); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	accepted, err := s.Accept(ctx, req)
	if err != nil || accepted.Created || accepted.Operation.Members[0].Worker == nil || !accepted.Operation.Members[0].Worker.CreateProgress {
		t.Fatalf("replay capability: %+v %v", accepted, err)
	}
	worker.CreateProgress = false
	if err := s.Assign(ctx, req.ID, []string{req.Members[0].ID}, worker); err == nil {
		t.Fatal("assignment replay changed capability")
	}
	op, err := s.Get(ctx, req.ID)
	if err != nil || !op.Members[0].Worker.CreateProgress {
		t.Fatalf("capability changed: %+v %v", op, err)
	}
	if _, err := s.db.Exec(`ALTER TABLE create_members DROP COLUMN worker_progress`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	op, err = s.Get(ctx, req.ID)
	if err != nil || op.Members[0].Worker == nil || op.Members[0].Worker.CreateProgress {
		t.Fatalf("legacy assignment acquired capability: %+v %v", op, err)
	}
	worker.CreateProgress = true
	if err := s.Assign(ctx, req.ID, []string{req.Members[0].ID}, worker); err == nil {
		t.Fatal("legacy assignment replay changed capability")
	}
	accepted, err = s.Accept(ctx, req)
	if err != nil || accepted.Operation.Members[0].Worker.CreateProgress {
		t.Fatalf("legacy replay acquired capability: %+v %v", accepted, err)
	}
}

func TestIngestProgressDoesNotFenceOnAdvertisedCapability(t *testing.T) {
	s, w, p := progressFixture(t)
	w.CreateProgress = true
	if ack, err := s.IngestProgress(context.Background(), w, p); err != nil || ack != p.Sequence {
		t.Fatalf("advertised capability fenced observation: %d %v", ack, err)
	}
}
