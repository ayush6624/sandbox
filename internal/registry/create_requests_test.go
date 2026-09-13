package registry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func requestIntent(id string) CreateIntent {
	return CreateIntent{ID: id, Hash: sha256.Sum256([]byte(id)), SourceType: "snapshot", SourceID: "source", Metadata: map[string]string{"run": "replay"}}
}

func TestCreateRequestAtomicAllocationAndPublication(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	intent := requestIntent("create-1")
	if _, err := reg.db.Exec(`CREATE TRIGGER reject_request BEFORE INSERT ON create_requests BEGIN SELECT RAISE(ABORT,'injected ledger failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateStarting(ctx, "allocated", "name", "/tmp/disk", nil, "", 0, 1, 128, intent); err == nil {
		t.Fatal("allocation accepted without request")
	}
	if _, err := reg.Get(ctx, "allocated"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("allocation survived rollback: %v", err)
	}
	if _, err := reg.db.Exec(`DROP TRIGGER reject_request`); err != nil {
		t.Fatal(err)
	}
	sb, err := reg.CreateStarting(ctx, "allocated", "name", "/tmp/disk", nil, "", 0, 1, 128, intent)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Metadata["run"] != "replay" || sb.SourceID != "source" {
		t.Fatalf("missing atomic public fields: %+v", sb)
	}
	if _, err := reg.CreateStarting(ctx, "duplicate", "name", "/tmp/other", nil, "", 0, 1, 128, intent); err == nil {
		t.Fatal("request allocated twice")
	}
	if _, err := reg.Get(ctx, "duplicate"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("duplicate row survived: %v", err)
	}
	if _, err := reg.db.Exec(`CREATE TRIGGER reject_success BEFORE UPDATE ON create_requests WHEN NEW.phase='succeeded' BEGIN SELECT RAISE(ABORT,'injected success failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := reg.MarkRunning(ctx, sb.ID); err == nil {
		t.Fatal("published without result")
	}
	raw, _ := reg.Get(ctx, sb.ID)
	if raw.Status != StatusStarting {
		t.Fatalf("readiness escaped rollback: %s", raw.Status)
	}
	if _, err := reg.db.Exec(`DROP TRIGGER reject_success`); err != nil {
		t.Fatal(err)
	}
	if err := reg.MarkRunning(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	if err := reg.Destroy(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	result, err := reg.CreateRequest(ctx, intent)
	if err != nil || result.Phase != "succeeded" || result.Sandbox.ID != sb.ID || result.Sandbox.Metadata["run"] != "replay" {
		t.Fatalf("historical result lost: %+v, %v", result, err)
	}
	conflict := intent
	conflict.Hash = sha256.Sum256([]byte("different"))
	if _, err := reg.CreateRequest(ctx, conflict); !errors.Is(err, ErrCreateRequestConflict) {
		t.Fatalf("body conflict accepted: %v", err)
	}
}

func TestWarmRequestPromotionRollbackAndReplay(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	intent := requestIntent("warm-1")
	if _, err := reg.CreateWarmForTemplate(ctx, "warm", "/tmp/disk", "base", "template", 1, 128); err != nil {
		t.Fatal(err)
	}
	if err := reg.MarkWarmReady(ctx, "warm"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.db.Exec(`CREATE TRIGGER reject_request BEFORE INSERT ON create_requests BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.ClaimWarmForTemplate(ctx, "template", "claimed", nil, 30, intent); err == nil {
		t.Fatal("promoted without request")
	}
	raw, _ := reg.Get(ctx, "warm")
	if raw.Status != StatusWarming {
		t.Fatalf("promotion escaped rollback: %s", raw.Status)
	}
	if _, err := reg.db.Exec(`DROP TRIGGER reject_request`); err != nil {
		t.Fatal(err)
	}
	sb, err := reg.ClaimWarmForTemplate(ctx, "template", "claimed", nil, 30, intent)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reg.CreateRequest(ctx, intent)
	if err != nil || result.Phase != "succeeded" || result.Sandbox.ID != sb.ID || result.Sandbox.Name != "claimed" || result.Sandbox.Metadata["run"] != "replay" {
		t.Fatalf("claim result: %+v %v", result, err)
	}
}

func TestCreateRequestReopenAndInterruptedDestroy(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "registry.db")
	pools := Pools{TapPrefix: "fc", TapMax: 2, GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.11", PortMin: 5200, PortMax: 5201}
	reg, err := Open(path, pools)
	if err != nil {
		t.Fatal(err)
	}
	identity := reg.RegistryID()
	intent := requestIntent("interrupted")
	if _, err := reg.CreateStarting(ctx, "sb", "", "/tmp/disk", nil, "", 0, 1, 128, intent); err != nil {
		t.Fatal(err)
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	reg, err = Open(path, pools)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if identity == "" || reg.RegistryID() != identity {
		t.Fatal("registry identity did not survive restart")
	}
	if err := reg.Destroy(ctx, "sb"); err != nil {
		t.Fatal(err)
	}
	result, err := reg.CreateRequest(ctx, intent)
	if err != nil || result.Phase != "failed" || result.Code != "create_interrupted" {
		t.Fatalf("interrupted allocation not retained: %+v %v", result, err)
	}
	if _, err := reg.CreateStarting(ctx, "replacement", "", "/tmp/disk2", nil, "", 0, 1, 128, intent); err == nil {
		t.Fatal("interrupted request allocated a replacement")
	}
	other, err := Open(filepath.Join(t.TempDir(), "fresh.db"), pools)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.RegistryID() == identity {
		t.Fatal("fresh registry reused identity")
	}
}
