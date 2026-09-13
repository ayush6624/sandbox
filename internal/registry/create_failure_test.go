package registry

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCreateFailureRetainsFirstCauseUntilDestroy(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	intent := requestIntent("first-cause")
	if _, err := reg.CreateStarting(ctx, "sb", "", "/tmp/disk", nil, "", 0, 1, 128, intent); err != nil {
		t.Fatal(err)
	}
	first := CreateFailure{Status: 504, Code: "agent_readiness_failed", Detail: "The sandbox agent did not become ready."}
	if err := reg.RecordCreateFailure(ctx, "sb", first); err != nil {
		t.Fatal(err)
	}
	if err := reg.RecordCreateFailure(ctx, "sb", CreateFailure{Status: 500, Code: "cleanup_failed", Detail: "Cleanup failed."}); err != nil {
		t.Fatal(err)
	}
	assertResult := func(phase string) {
		t.Helper()
		result, err := reg.CreateRequest(ctx, intent)
		if err != nil || result.Phase != phase || result.Status != first.Status || result.Code != first.Code || result.Detail != first.Detail {
			t.Fatalf("want %s with first cause, got %+v, %v", phase, result, err)
		}
	}
	assertResult("allocated")
	var completed bool
	if err := reg.db.QueryRow(`SELECT completed_at IS NOT NULL FROM create_requests WHERE id=?`, intent.ID).Scan(&completed); err != nil || completed {
		t.Fatalf("pending failure completed: %v, %v", completed, err)
	}
	if _, err := reg.Get(ctx, "sb"); err != nil {
		t.Fatalf("pending failure removed allocation: %v", err)
	}
	if err := reg.Destroy(ctx, "sb"); err != nil {
		t.Fatal(err)
	}
	assertResult("failed")
	if err := reg.RecordCreateFailure(ctx, "sb", CreateFailure{Status: 500, Code: "later", Detail: "Later failure."}); err != nil {
		t.Fatal(err)
	}
	assertResult("failed")
}

func TestCreateFailureSurvivesRegistryReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "registry.db")
	pools := Pools{TapPrefix: "fc", TapMax: 2, GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.11", PortMin: 5200, PortMax: 5201}
	reg, err := Open(path, pools)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()
	intent := requestIntent("persist-cause")
	if _, err := reg.CreateStarting(ctx, "sb", "", "/tmp/disk", nil, "", 0, 1, 128, intent); err != nil {
		t.Fatal(err)
	}
	failure := CreateFailure{Status: 500, Code: "rootfs_prepare_failed", Detail: "The sandbox filesystem could not be prepared."}
	if err := reg.RecordCreateFailure(ctx, "sb", failure); err != nil {
		t.Fatal(err)
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, pools)
	if err != nil {
		t.Fatal(err)
	}
	reg = reopened
	pending, err := reg.CreateRequest(ctx, intent)
	if err != nil || pending.Phase != "allocated" || pending.Code != failure.Code {
		t.Fatalf("reopened pending cause: %+v, %v", pending, err)
	}
	if err := reg.Destroy(ctx, "sb"); err != nil {
		t.Fatal(err)
	}
	result, err := reg.CreateRequest(ctx, intent)
	if err != nil || result.Phase != "failed" || result.Status != failure.Status || result.Code != failure.Code || result.Detail != failure.Detail {
		t.Fatalf("recovered cause: %+v, %v", result, err)
	}
}

func TestCreateFailureDestroyRollback(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	intent := requestIntent("rollback-cause")
	if _, err := reg.CreateStarting(ctx, "sb", "", "/tmp/disk", nil, "", 0, 1, 128, intent); err != nil {
		t.Fatal(err)
	}
	failure := CreateFailure{Status: 500, Code: "identity_initialization_failed", Detail: "The sandbox identity could not be initialized."}
	if err := reg.RecordCreateFailure(ctx, "sb", failure); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.db.Exec(`CREATE TRIGGER reject_destroy BEFORE DELETE ON sandboxes BEGIN SELECT RAISE(ABORT,'injected delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := reg.Destroy(ctx, "sb"); err == nil {
		t.Fatal("destroy ignored injected failure")
	}
	result, err := reg.CreateRequest(ctx, intent)
	if err != nil || result.Phase != "allocated" || result.Status != failure.Status || result.Code != failure.Code || result.Detail != failure.Detail {
		t.Fatalf("failed destroy changed pending cause: %+v, %v", result, err)
	}
	if _, err := reg.Get(ctx, "sb"); err != nil {
		t.Fatalf("failed destroy lost allocation: %v", err)
	}
	var completed bool
	if err := reg.db.QueryRow(`SELECT completed_at IS NOT NULL FROM create_requests WHERE id=?`, intent.ID).Scan(&completed); err != nil || completed {
		t.Fatalf("failed destroy completed: %v, %v", completed, err)
	}
	if _, err := reg.db.Exec(`DROP TRIGGER reject_destroy`); err != nil {
		t.Fatal(err)
	}
	if err := reg.Destroy(ctx, "sb"); err != nil {
		t.Fatal(err)
	}
	result, err = reg.CreateRequest(ctx, intent)
	if err != nil || result.Phase != "failed" || result.Code != failure.Code {
		t.Fatalf("destroy retry: %+v, %v", result, err)
	}
}

func TestCreateFailureIgnoresSuccessAndUnlinkedSandbox(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	intent := requestIntent("successful")
	if _, err := reg.CreateStarting(ctx, "sb", "", "/tmp/disk", nil, "", 0, 1, 128, intent); err != nil {
		t.Fatal(err)
	}
	if err := reg.MarkRunning(ctx, "sb"); err != nil {
		t.Fatal(err)
	}
	failure := CreateFailure{Status: 500, Code: "late_failure", Detail: "A late failure occurred."}
	if err := reg.RecordCreateFailure(ctx, "sb", failure); err != nil {
		t.Fatal(err)
	}
	if err := reg.Destroy(ctx, "sb"); err != nil {
		t.Fatal(err)
	}
	result, err := reg.CreateRequest(ctx, intent)
	if err != nil || result.Phase != "succeeded" || result.Status != 0 || result.Code != "" || result.Detail != "" || result.Sandbox.ID != "sb" {
		t.Fatalf("success overwritten: %+v, %v", result, err)
	}
	if _, err := reg.CreateStarting(ctx, "unlinked", "", "/tmp/disk", nil, "", 0, 1, 128); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"unlinked", "absent"} {
		if err := reg.RecordCreateFailure(ctx, id, failure); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	var count int
	if err := reg.db.QueryRow(`SELECT COUNT(*) FROM create_requests`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failure created ledger rows: %d, %v", count, err)
	}
}

func TestCreateFailureValidationAndGenericFallback(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	intent := requestIntent("generic")
	if _, err := reg.CreateStarting(ctx, "sb", "", "/tmp/disk", nil, "", 0, 1, 128, intent); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []CreateFailure{
		{Status: 499, Code: "invalid", Detail: "Invalid."},
		{Status: 600, Code: "invalid", Detail: "Invalid."},
		{Status: 500, Detail: "Invalid."},
		{Status: 500, Code: "invalid"},
	} {
		if err := reg.RecordCreateFailure(ctx, "sb", failure); err == nil {
			t.Fatalf("accepted invalid cause: %+v", failure)
		}
	}
	pending, err := reg.CreateRequest(ctx, intent)
	if err != nil || pending.Phase != "allocated" || pending.Status != 0 || pending.Code != "" || pending.Detail != "" {
		t.Fatalf("invalid cause changed ledger: %+v, %v", pending, err)
	}
	if err := reg.Destroy(ctx, "sb"); err != nil {
		t.Fatal(err)
	}
	result, err := reg.CreateRequest(ctx, intent)
	if err != nil || result.Phase != "failed" || result.Status != 500 || result.Code != "create_interrupted" || result.Detail != "Sandbox creation was interrupted." {
		t.Fatalf("generic fallback: %+v, %v", result, err)
	}
}
