package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

func seedHibernationHandoff(t *testing.T, r *Registry, id string) HibernationHandoff {
	t.Helper()
	ctx := context.Background()
	if _, err := r.Create(ctx, id, "", "/tmp/"+id, nil, "", 0, 1, 128); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddPort(ctx, id, 8080); err != nil {
		t.Fatal(err)
	}
	if err := r.Hibernate(ctx, id); err != nil {
		t.Fatal(err)
	}
	return HibernationHandoff{Generation: "generation-" + id, SandboxID: id, Offer: []byte(`{"generation":"fixture"}`), ExpectedRevision: 7}
}

func TestHibernationHandoffCommitIsAtomic(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	job := seedHibernationHandoff(t, r, "source")
	if _, err := r.db.Exec(`CREATE TRIGGER reject_handoff_delete BEFORE DELETE ON sandboxes BEGIN SELECT RAISE(ABORT,'injected row deletion failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitHibernationHandoff(ctx, job); err == nil {
		t.Fatal("committed despite injected row failure")
	}
	if _, err := r.GetHibernationHandoff(ctx, job.Generation); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("journal escaped rollback: %v", err)
	}
	if row, err := r.Get(ctx, job.SandboxID); err != nil || row.Status != StatusHibernated {
		t.Fatalf("source row changed: %+v %v", row, err)
	}
	if ports, err := r.Ports(ctx, job.SandboxID); err != nil || len(ports) != 1 {
		t.Fatalf("ports escaped rollback: %+v %v", ports, err)
	}
	if _, err := r.db.Exec(`DROP TRIGGER reject_handoff_delete`); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitHibernationHandoff(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, job.SandboxID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("source still serving: %v", err)
	}
	if ports, err := r.Ports(ctx, job.SandboxID); err != nil || len(ports) != 0 {
		t.Fatalf("ports not removed: %+v %v", ports, err)
	}
	if jobs, err := r.ListHibernationHandoffs(ctx); err != nil || len(jobs) != 1 {
		t.Fatalf("journal missing: %+v %v", jobs, err)
	}
}

func TestHibernationHandoffReplayPreservesReturnedRow(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	job := seedHibernationHandoff(t, r, "returned")
	if err := r.CommitHibernationHandoff(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(ctx, job.SandboxID, "new owner", "/tmp/new", nil, "", 0, 1, 128); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitHibernationHandoff(ctx, job); err != nil {
		t.Fatal(err)
	}
	row, err := r.Get(ctx, job.SandboxID)
	if err != nil || row.Name != "new owner" {
		t.Fatalf("replay deleted returned sandbox: %+v %v", row, err)
	}
	changed := job
	changed.Offer = []byte(`{"generation":"different"}`)
	if err := r.CommitHibernationHandoff(ctx, changed); err == nil {
		t.Fatal("changed replay accepted")
	}
	if _, err := r.Get(ctx, job.SandboxID); err != nil {
		t.Fatal(err)
	}
}

func TestHibernationHandoffPersistenceAndIndependentAcknowledgment(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	job := seedHibernationHandoff(t, r, "restart")
	if err := r.CommitHibernationHandoff(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkHibernationHandoffPublished(ctx, job.Generation); err != nil {
		t.Fatal(err)
	}
	if err := r.AckHibernationHandoffCache(ctx, job.Generation); err != nil {
		t.Fatal(err)
	}
	if err := r.WithHibernationHandoff(ctx, job.Generation, func(a *HandoffAttempt) error { return a.Remove(ctx) }); err == nil {
		t.Fatal("ack deleted pending backup obligation")
	}
	path, pools := r.path, r.pools
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, pools)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := reopened.GetHibernationHandoff(ctx, job.Generation)
	if err != nil || !stored.Published || !stored.CacheReady || stored.BackupComplete || string(stored.Offer) != string(job.Offer) {
		t.Fatalf("journal not restored: %+v %v", stored, err)
	}
	if err := reopened.WithHibernationHandoff(ctx, job.Generation, func(a *HandoffAttempt) error { return a.CompleteBackup(ctx) }); err != nil {
		t.Fatal(err)
	}
	if err := reopened.WithHibernationHandoff(ctx, job.Generation, func(a *HandoffAttempt) error { return a.Remove(ctx) }); err != nil {
		t.Fatal(err)
	}
	if err := reopened.WithHibernationHandoff(ctx, job.Generation, func(a *HandoffAttempt) error { return a.Remove(ctx) }); err != nil {
		t.Fatal("cleanup not idempotent", err)
	}
	if jobs, err := reopened.ListHibernationHandoffs(ctx); err != nil || len(jobs) != 0 {
		t.Fatalf("completed journal remains: %+v %v", jobs, err)
	}
}

func TestHibernationHandoffRejectsRunningRow(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	job := seedHibernationHandoff(t, r, "running")
	if _, _, err := r.Wake(ctx, job.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitHibernationHandoff(ctx, job); err == nil {
		t.Fatal("released running row")
	}
	if _, err := r.Get(ctx, job.SandboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetHibernationHandoff(ctx, job.Generation); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("running release made journal: %v", err)
	}
}

func TestHibernationHandoffLimitIsTransactional(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	for i := 0; i < MaxPendingHibernationHandoffs-1; i++ {
		if _, err := r.db.Exec(`INSERT INTO hibernation_handoffs(generation,sandbox_id,offer,expected_revision) VALUES(?,?,?,0)`, fmt.Sprintf("existing-%d", i), fmt.Sprintf("old-%d", i), []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	first, second := seedHibernationHandoff(t, r, "first"), seedHibernationHandoff(t, r, "second")
	results := make(chan error, 2)
	go func() { results <- r.CommitHibernationHandoff(ctx, first) }()
	go func() { results <- r.CommitHibernationHandoff(ctx, second) }()
	success, rejected := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrHibernationHandoffLimit) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || rejected != 1 {
		t.Fatalf("admitted=%d rejected=%d", success, rejected)
	}
	jobs, err := r.ListHibernationHandoffs(ctx)
	if err != nil || len(jobs) != MaxPendingHibernationHandoffs {
		t.Fatalf("pending count=%d: %v", len(jobs), err)
	}
	present := 0
	for _, id := range []string{first.SandboxID, second.SandboxID} {
		if row, err := r.Get(ctx, id); err == nil {
			present++
			if row.Status != StatusHibernated {
				t.Fatal("rejected source changed status")
			}
		}
	}
	if present != 1 {
		t.Fatalf("kept %d source rows", present)
	}
}
