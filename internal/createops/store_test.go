package createops

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ayush6624/sandbox/internal/registry"
)

func request(id, scope, hash string) AcceptRequest {
	return AcceptRequest{ID: id, Scope: scope, BodyHash: []byte(hash), Type: "sandbox_batch_create", MaxParallelism: 2, Members: []Member{{ID: id + "-0", Index: 0, Spec: Spec{Name: "n"}}}, ResponseStatus: 202, ResponseBody: []byte(`{"id":"` + id + `"}`)}
}

func TestSQLiteStoreReopenAcceptConflictAndOutcomes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ops.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Accept(ctx, request("one", "POST /v1/sandbox-batches key", "body-a"))
	if err != nil || !first.Created {
		t.Fatalf("accept=%+v err=%v", first, err)
	}
	if _, err = s.Accept(ctx, request("other", "POST /v1/sandbox-batches key", "body-b")); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict err=%v", err)
	}
	if err = s.Assign(ctx, "one", []string{"one-0"}, Worker{HostID: "host", RegistryID: "registry"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Record(ctx, "one", []Outcome{{ID: "one-0", Sandbox: &registry.Sandbox{ID: "sandbox", Status: registry.StatusRunning}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	replay, err := s.Accept(ctx, request("ignored", "POST /v1/sandbox-batches key", "body-a"))
	if err != nil || replay.Created {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if replay.ResponseStatus != first.ResponseStatus || !bytes.Equal(replay.ResponseBody, first.ResponseBody) {
		t.Fatal("reopen changed the original acceptance response")
	}
	op, err := s.Get(ctx, "one")
	if err != nil || op.CompletedAt == nil || op.Members[0].Worker.RegistryID != "registry" || op.Members[0].Outcome.Sandbox.ID != "sandbox" {
		t.Fatalf("op=%+v err=%v", op, err)
	}
}

func TestSQLiteStorePendingIncludesUnfinalizedTerminalSingle(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "ops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.Accept(ctx, AcceptRequest{ID: "single", Scope: "single", BodyHash: []byte("x"), Type: "sandbox_create", MaxParallelism: 1, Members: []Member{{ID: "member"}}, ResponseStatus: 202, ResponseBody: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Assign(ctx, "single", []string{"member"}, Worker{HostID: "h", RegistryID: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Record(ctx, "single", []Outcome{{ID: "member", Failure: &Failure{Status: 409, Code: "denied"}}}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.Pending(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != "single" {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	if err = s.SetFinalResponse(ctx, "single", 409, []byte(`{"code":"denied"}`)); err != nil {
		t.Fatal(err)
	}
	pending, err = s.Pending(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
}

func TestAcceptanceAndAssignmentRollbackTogether(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "operations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`CREATE TRIGGER reject_second_member BEFORE INSERT ON create_members WHEN NEW.item_index=1 BEGIN SELECT RAISE(ABORT,'injected member write failure'); END`); err != nil {
		t.Fatal(err)
	}
	input := request("operation", "key", "hash")
	input.Members = append(input.Members, Member{ID: "second", Index: 1})
	if _, err := s.Accept(ctx, input); err == nil {
		t.Fatal("fault did not abort acceptance")
	}
	for _, table := range []string{"create_operations", "create_members"} {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial acceptance in %s: count=%d err=%v", table, count, err)
		}
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_second_member`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accept(ctx, input); err != nil {
		t.Fatal(err)
	}
	worker := Worker{HostID: "host", RegistryID: "registry"}
	if err := s.Assign(ctx, input.ID, []string{input.Members[0].ID, "nonexistent"}, worker); err == nil {
		t.Fatal("fault did not abort assignment")
	}
	op, err := s.Get(ctx, input.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range op.Members {
		if member.Worker != nil {
			t.Fatal("partial assignment survived transaction rollback")
		}
	}
}

func TestRequestIDMigrationPreservesExistingOperations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "operations.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accept(ctx, request("old", "old-key", "body")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`ALTER TABLE create_operations DROP COLUMN request_id`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for reopen := 0; reopen < 2; reopen++ {
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		old, err := s.Get(ctx, "old")
		if err != nil || old.RequestID != "" || len(old.Members) != 1 {
			t.Fatalf("legacy operation changed: %+v %v", old, err)
		}
		req := request("new", "new-key", "body")
		req.RequestID = "original-request"
		if reopen > 0 {
			req.RequestID = "replay-request"
		}
		accepted, err := s.Accept(ctx, req)
		if err != nil || accepted.Operation.RequestID != "original-request" {
			t.Fatalf("original request ID lost: %+v %v", accepted, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
