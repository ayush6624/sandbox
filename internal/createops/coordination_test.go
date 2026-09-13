package createops

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCoordinationLifecycleAndFencing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ops.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	accepted, err := s.Accept(ctx, request("one", "scope", "hash"))
	if err != nil {
		t.Fatal(err)
	}
	initial := accepted.Operation.Members[0].Coordination
	if initial == nil || initial.Phase != "queued" || !initial.UpdatedAt.Equal(accepted.Operation.CreatedAt) {
		t.Fatalf("acceptance coordination: %+v", initial)
	}
	ids := []string{"one-0"}
	for _, phase := range []string{"queued", "assigned", "completed", "transport error", "retrying"} {
		if err := s.SetCoordination(ctx, "one", ids, phase); err == nil {
			t.Fatalf("accepted invalid transition %q", phase)
		}
	}
	if err := s.SetCoordination(ctx, "one", []string{"one-0", "missing"}, "placing"); err == nil {
		t.Fatal("unknown member accepted")
	}
	op, err := s.Get(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	if *op.Members[0].Coordination != *initial {
		t.Fatal("partial phase update escaped rollback")
	}
	if err := s.SetCoordination(ctx, "one", ids, "placing"); err != nil {
		t.Fatal(err)
	}
	if err := s.Assign(ctx, "one", []string{"one-0", "missing"}, Worker{HostID: "h", RegistryID: "r"}); err == nil {
		t.Fatal("invalid assignment accepted")
	}
	op, err = s.Get(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	if op.Members[0].Worker != nil || op.Members[0].Coordination.Phase != "placing" {
		t.Fatal("partial assignment escaped rollback")
	}
	if err := s.Assign(ctx, "one", ids, Worker{HostID: "h", RegistryID: "r"}); err != nil {
		t.Fatal(err)
	}
	op, err = s.Get(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	if op.Members[0].Worker == nil || op.Members[0].Coordination.Phase != "assigned" {
		t.Fatal("assignment did not set coordination")
	}
	if err := s.SetCoordination(ctx, "one", ids, "placing"); err == nil {
		t.Fatal("assigned member allowed placing")
	}
	if err := s.SetCoordination(ctx, "one", ids, "retrying"); err != nil {
		t.Fatal(err)
	}
	op, err = s.Get(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	retry := *op.Members[0].Coordination
	if err := s.SetCoordination(ctx, "one", ids, "retrying"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	op, err = s.Get(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	if *op.Members[0].Coordination != retry {
		t.Fatal("retry timestamp changed or lost across reopen")
	}
	op, err = s.Record(ctx, "one", []Outcome{{ID: "one-0", Failure: &Failure{Code: "failed"}}})
	if err != nil {
		t.Fatal(err)
	}
	completed := *op.Members[0].Coordination
	if completed.Phase != "completed" || op.CompletedAt == nil || !completed.UpdatedAt.Equal(*op.CompletedAt) {
		t.Fatalf("completion not atomic: %+v", op)
	}
	for _, phase := range []string{"placing", "retrying"} {
		if err := s.SetCoordination(ctx, "one", ids, phase); err != nil {
			t.Fatal(err)
		}
	}
	op, err = s.Record(ctx, "one", []Outcome{{ID: "one-0"}})
	if err != nil {
		t.Fatal(err)
	}
	if *op.Members[0].Coordination != completed || op.Members[0].Outcome.Failure == nil {
		t.Fatal("late writer changed terminal observation")
	}
}

func TestCoordinationMigrationRetainsUnknownHistoricalTimes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ops.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accept(ctx, request("old", "scope", "hash")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`ALTER TABLE create_members DROP COLUMN coordination_phase; ALTER TABLE create_members DROP COLUMN coordination_updated_at`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		op, err := s.Get(ctx, "old")
		if err != nil {
			t.Fatal(err)
		}
		if op.Members[0].Coordination != nil {
			t.Fatal("migration invented historical coordination")
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCoordinationWorkerIngestionAndCompletionRollback(t *testing.T) {
	s, w, p := progressFixture(t)
	ctx := context.Background()
	id := p.Owner.OperationID
	if err := s.SetCoordination(ctx, id, []string{p.ID}, "retrying"); err != nil {
		t.Fatal(err)
	}
	before, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	retry := *before.Members[0].Coordination
	if _, err := s.IngestProgress(ctx, w, p); err != nil {
		t.Fatal(err)
	}
	op, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if *op.Members[0].Coordination != retry {
		t.Fatal("worker observation changed coordinator retry")
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_completion BEFORE UPDATE OF completed_at ON create_operations BEGIN SELECT RAISE(ABORT,'injected completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	failed := failureProgress(p)
	if _, err := s.IngestProgress(ctx, w, failed); err == nil {
		t.Fatal("completion fault did not fail ingestion")
	}
	op, err = s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if *op.Members[0].Coordination != retry || op.Members[0].Outcome != nil || op.Members[0].Progress.Sequence != p.Sequence {
		t.Fatal("failed completion partially committed")
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_completion`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestProgress(ctx, w, failed); err != nil {
		t.Fatal(err)
	}
	op, err = s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if op.Members[0].Coordination.Phase != "completed" || op.CompletedAt == nil {
		t.Fatal("worker failure did not complete coordinator")
	}
}
