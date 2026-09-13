package registry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func beginProgress(t *testing.T, reg *Registry, id string) CreateIntent {
	t.Helper()
	intent := requestIntent(id)
	p, err := reg.BeginCreateProgress(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	intent.ProgressAttempt = p.Attempt
	return intent
}

func advanceProgress(t *testing.T, reg *Registry, intent CreateIntent, stages ...CreateStage) {
	t.Helper()
	for _, stage := range stages {
		if err := reg.AdvanceCreateProgress(context.Background(), intent, stage); err != nil {
			t.Fatalf("advance %s: %v", stage, err)
		}
	}
}

func getProgress(t *testing.T, reg *Registry, id string) CreateProgress {
	t.Helper()
	p, err := reg.CreateProgress(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func allocateProgress(t *testing.T, reg *Registry, intent CreateIntent) {
	t.Helper()
	if _, err := reg.CreateStarting(context.Background(), "sb-"+intent.ID, "", "/tmp/disk", nil, "", 0, 1, 128, intent); err != nil {
		t.Fatal(err)
	}
}

func TestCreateProgressPreallocationRetryFencing(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	intent := beginProgress(t, reg, "retry")
	first := getProgress(t, reg, intent.ID)
	if first.RegistryID != reg.RegistryID() || first.Sequence != 1 || first.Attempt != 1 || first.Current.Stage != CreateStageAdmission || first.LastCompleted != nil || first.Outcome != nil {
		t.Fatalf("initial progress: %+v", first)
	}
	if _, err := reg.CreateRequest(ctx, intent); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("begin created ledger: %v", err)
	}
	rows, err := reg.All(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("begin allocated sandbox: %+v, %v", rows, err)
	}
	advanceProgress(t, reg, intent, CreateStageSource)
	previous := getProgress(t, reg, intent.ID)
	second, err := reg.BeginCreateProgress(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	if second.Attempt != 2 || second.Sequence != previous.Sequence+1 || second.Current.Stage != CreateStageAdmission || !reflect.DeepEqual(second.LastCompleted, previous.LastCompleted) || second.LastCompleted.Attempt != 1 {
		t.Fatalf("retry lost attempt history: %+v", second)
	}
	if err := reg.AdvanceCreateProgress(ctx, intent, CreateStageSource); !errors.Is(err, ErrCreateProgressStale) {
		t.Fatalf("stale advance: %v", err)
	}
	if _, err := reg.CreateStarting(ctx, "stale", "", "/tmp/disk", nil, "", 0, 1, 128, intent); !errors.Is(err, ErrCreateProgressStale) {
		t.Fatalf("stale allocation: %v", err)
	}
	if err := reg.FailCreateRequest(ctx, intent, 500, "old_attempt", "Old attempt failed."); !errors.Is(err, ErrCreateProgressStale) {
		t.Fatalf("stale preallocation failure: %v", err)
	}
	if _, err := reg.Get(ctx, "stale"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale row survived: %v", err)
	}
	conflict := intent
	conflict.Hash = sha256.Sum256([]byte("changed"))
	if _, err := reg.BeginCreateProgress(ctx, conflict); !errors.Is(err, ErrCreateRequestConflict) {
		t.Fatalf("begin hash conflict: %v", err)
	}
	conflict.ProgressAttempt = second.Attempt
	if err := reg.AdvanceCreateProgress(ctx, conflict, CreateStageSource); !errors.Is(err, ErrCreateRequestConflict) {
		t.Fatalf("advance hash conflict: %v", err)
	}
	intent.ProgressAttempt = second.Attempt
	advanceProgress(t, reg, intent, CreateStageSource)
	allocateProgress(t, reg, intent)
	p := getProgress(t, reg, intent.ID)
	if p.Current.Stage != CreateStagePreparing || p.LastCompleted.Stage != CreateStageAllocated || p.LastCompleted.CompletedAt == nil || p.LastCompleted.Attempt != 2 {
		t.Fatalf("allocated progress: %+v", p)
	}
	if _, err := reg.BeginCreateProgress(ctx, intent); !errors.Is(err, ErrCreateProgressClosed) {
		t.Fatalf("allocated request restarted: %v", err)
	}
}

func TestCreateProgressStageTransitionsAndPublication(t *testing.T) {
	for _, clone := range []bool{false, true} {
		name := "cold"
		if clone {
			name = "clone"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			reg := testRegistry(t)
			intent := beginProgress(t, reg, name)
			if err := reg.AdvanceCreateProgress(ctx, intent, CreateStageReady); err == nil {
				t.Fatal("invented ready stage")
			}
			advanceProgress(t, reg, intent, CreateStageSource)
			if err := reg.AdvanceCreateProgress(ctx, intent, CreateStagePreparing); err == nil {
				t.Fatal("preparing without allocation")
			}
			allocateProgress(t, reg, intent)
			advanceProgress(t, reg, intent, CreateStageVM)
			if clone {
				advanceProgress(t, reg, intent, CreateStageNetwork)
			}
			advanceProgress(t, reg, intent, CreateStageAgent)
			before := getProgress(t, reg, intent.ID)
			advanceProgress(t, reg, intent, CreateStageAgent)
			if after := getProgress(t, reg, intent.ID); !reflect.DeepEqual(before, after) {
				t.Fatal("same stage created another event")
			}
			if err := reg.AdvanceCreateProgress(ctx, intent, CreateStageVM); err == nil {
				t.Fatal("stage moved backward")
			}
			advanceProgress(t, reg, intent, CreateStageIdentity)
			if err := reg.MarkRunning(ctx, "sb-"+intent.ID); err != nil {
				t.Fatal(err)
			}
			p := getProgress(t, reg, intent.ID)
			if p.Condition != "succeeded" || p.Outcome == nil || p.Outcome.Phase != "succeeded" || p.Outcome.Sandbox.ID != "sb-"+intent.ID || p.Current.Stage != CreateStageReady || p.Current.CompletedAt == nil || p.LastCompleted.Stage != CreateStageReady {
				t.Fatalf("published progress: %+v", p)
			}
			if err := reg.AdvanceCreateProgress(ctx, intent, CreateStageIdentity); !errors.Is(err, ErrCreateProgressClosed) {
				t.Fatalf("terminal advance: %v", err)
			}
			if err := reg.Destroy(ctx, "sb-"+intent.ID); err != nil {
				t.Fatal(err)
			}
			if after := getProgress(t, reg, intent.ID); !reflect.DeepEqual(p, after) {
				t.Fatal("destroy changed historical success")
			}
		})
	}
}

func TestCreateProgressWarmClaimSkipsGuestStages(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	if _, err := reg.CreateWarmForTemplate(ctx, "warm", "/tmp/disk", "base", "template", 1, 128); err != nil {
		t.Fatal(err)
	}
	if err := reg.MarkWarmReady(ctx, "warm"); err != nil {
		t.Fatal(err)
	}
	intent := beginProgress(t, reg, "warm-request")
	advanceProgress(t, reg, intent, CreateStageSource)
	if _, err := reg.ClaimWarmForTemplate(ctx, "template", "name", nil, 0, intent); err != nil {
		t.Fatal(err)
	}
	p := getProgress(t, reg, intent.ID)
	if p.Condition != "succeeded" || p.Current.Stage != CreateStageReady || p.LastCompleted.Stage != CreateStageReady || p.Outcome == nil || p.Outcome.Sandbox.ID != "warm" {
		t.Fatalf("warm claim: %+v", p)
	}
	if _, err := reg.BeginCreateProgress(ctx, intent); !errors.Is(err, ErrCreateProgressClosed) {
		t.Fatalf("warm claim restarted: %v", err)
	}
}

func TestCreateProgressRestartAndAcknowledgements(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "registry.db")
	pools := Pools{TapPrefix: "fc", TapMax: 2, GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.11", PortMin: 5200, PortMax: 5201}
	reg, err := Open(path, pools)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()
	intent := beginProgress(t, reg, "outbox")
	first := getProgress(t, reg, intent.ID)
	advanceProgress(t, reg, intent, CreateStageSource)
	second := getProgress(t, reg, intent.ID)
	if err := reg.AcknowledgeCreateProgress(ctx, intent.ID, first.Sequence); err != nil {
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
	p := getProgress(t, reg, intent.ID)
	if !reflect.DeepEqual(p, second) {
		t.Fatalf("restart changed progress: %+v", p)
	}
	pending, err := reg.PendingCreateProgress(ctx, 1)
	if err != nil || len(pending) != 1 || pending[0].Sequence != second.Sequence {
		t.Fatalf("old ack erased newer observation: %+v, %v", pending, err)
	}
	if err := reg.AcknowledgeCreateProgress(ctx, intent.ID, second.Sequence+1); err == nil {
		t.Fatal("acknowledged future observation")
	}
	if err := reg.AcknowledgeCreateProgress(ctx, intent.ID, second.Sequence); err != nil {
		t.Fatal(err)
	}
	if err := reg.AcknowledgeCreateProgress(ctx, intent.ID, first.Sequence); err != nil {
		t.Fatal(err)
	}
	pending, err = reg.PendingCreateProgress(ctx, 1)
	if err != nil || len(pending) != 0 {
		t.Fatalf("ack went backward: %+v, %v", pending, err)
	}
	allocateProgress(t, reg, intent)
	pending, err = reg.PendingCreateProgress(ctx, 1)
	if err != nil || len(pending) != 1 || pending[0].Current.Stage != CreateStagePreparing {
		t.Fatalf("new observation not pending: %+v, %v", pending, err)
	}
	if _, err := reg.CreateProgress(ctx, "unknown"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown progress: %v", err)
	}
	if err := reg.AcknowledgeCreateProgress(ctx, intent.ID, 0); err == nil {
		t.Fatal("acknowledged zero")
	}
}

func TestCreateProgressReadsUseIndependentConnection(t *testing.T) {
	reg := testRegistry(t)
	intent := beginProgress(t, reg, "independent-read")
	tx, err := reg.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// The writer connection remains occupied. Reads must use the WAL reader.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := reg.CreateProgress(ctx, intent.ID); err != nil {
		t.Fatalf("progress read waited for writer: %v", err)
	}
	if pending, err := reg.PendingCreateProgress(ctx, 1); err != nil || len(pending) != 1 {
		t.Fatalf("pending read waited for writer: %+v, %v", pending, err)
	}
}

func TestCreateProgressFailureAndDestroyRollback(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	intent := beginProgress(t, reg, "failure")
	advanceProgress(t, reg, intent, CreateStageSource)
	allocateProgress(t, reg, intent)
	advanceProgress(t, reg, intent, CreateStageVM, CreateStageAgent)
	before := getProgress(t, reg, intent.ID)
	cause := CreateFailure{Status: 500, Code: "agent_readiness_failed", Detail: "The sandbox agent did not become ready."}
	if err := reg.RecordCreateFailure(ctx, "sb-"+intent.ID, cause); err != nil {
		t.Fatal(err)
	}
	pending := getProgress(t, reg, intent.ID)
	if pending.Condition != "tearing_down" || pending.Outcome != nil || !reflect.DeepEqual(pending.Current, before.Current) || !reflect.DeepEqual(pending.LastCompleted, before.LastCompleted) {
		t.Fatalf("pending failure invented progress: %+v", pending)
	}
	ledger, err := reg.CreateRequest(ctx, intent)
	if err != nil || ledger.Phase != "allocated" {
		t.Fatalf("pending failure terminalized ledger: %+v, %v", ledger, err)
	}
	if err := reg.RecordCreateFailure(ctx, "sb-"+intent.ID, CreateFailure{Status: 500, Code: "later", Detail: "Later failure."}); err != nil {
		t.Fatal(err)
	}
	if after := getProgress(t, reg, intent.ID); !reflect.DeepEqual(pending, after) {
		t.Fatal("second cause changed pending observation")
	}
	if err := reg.AdvanceCreateProgress(ctx, intent, CreateStageIdentity); !errors.Is(err, ErrCreateProgressClosed) {
		t.Fatalf("advanced during teardown: %v", err)
	}
	if err := reg.MarkRunning(ctx, "sb-"+intent.ID); !errors.Is(err, ErrCreateProgressClosed) {
		t.Fatalf("published during teardown: %v", err)
	}
	if _, err := reg.db.Exec(`CREATE TRIGGER reject_progress_destroy BEFORE DELETE ON sandboxes BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := reg.Destroy(ctx, "sb-"+intent.ID); err == nil {
		t.Fatal("destroy ignored failure")
	}
	if after := getProgress(t, reg, intent.ID); !reflect.DeepEqual(pending, after) {
		t.Fatal("failed destroy published progress")
	}
	ledger, err = reg.CreateRequest(ctx, intent)
	if err != nil || ledger.Phase != "allocated" || ledger.Code != cause.Code {
		t.Fatalf("failed destroy changed ledger: %+v, %v", ledger, err)
	}
	if _, err := reg.db.Exec(`DROP TRIGGER reject_progress_destroy`); err != nil {
		t.Fatal(err)
	}
	if err := reg.Destroy(ctx, "sb-"+intent.ID); err != nil {
		t.Fatal(err)
	}
	final := getProgress(t, reg, intent.ID)
	if final.Condition != "failed" || final.Outcome == nil || final.Outcome.Code != cause.Code || !reflect.DeepEqual(final.Current, pending.Current) || !reflect.DeepEqual(final.LastCompleted, pending.LastCompleted) {
		t.Fatalf("terminal failure lost stage/cause: %+v", final)
	}
}

func TestCreateProgressTransactionFailuresRollbackLedger(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	intent := beginProgress(t, reg, "atomic")
	advanceProgress(t, reg, intent, CreateStageSource)
	before := getProgress(t, reg, intent.ID)
	reject := func() {
		t.Helper()
		if _, err := reg.db.Exec(`CREATE TRIGGER reject_progress BEFORE UPDATE ON create_progress BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
			t.Fatal(err)
		}
	}
	allow := func() {
		t.Helper()
		if _, err := reg.db.Exec(`DROP TRIGGER reject_progress`); err != nil {
			t.Fatal(err)
		}
	}
	reject()
	if _, err := reg.CreateStarting(ctx, "sb-"+intent.ID, "", "/tmp/disk", nil, "", 0, 1, 128, intent); err == nil {
		t.Fatal("allocation ignored progress write failure")
	}
	if _, err := reg.CreateRequest(ctx, intent); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("allocation ledger survived: %v", err)
	}
	if _, err := reg.Get(ctx, "sb-"+intent.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("allocation row survived: %v", err)
	}
	if after := getProgress(t, reg, intent.ID); !reflect.DeepEqual(before, after) {
		t.Fatal("allocation failure changed progress")
	}
	allow()
	allocateProgress(t, reg, intent)
	advanceProgress(t, reg, intent, CreateStageVM, CreateStageAgent, CreateStageIdentity)
	before = getProgress(t, reg, intent.ID)
	reject()
	if err := reg.MarkRunning(ctx, "sb-"+intent.ID); err == nil {
		t.Fatal("publication ignored progress failure")
	}
	ledger, err := reg.CreateRequest(ctx, intent)
	if err != nil || ledger.Phase != "allocated" {
		t.Fatalf("publication ledger survived: %+v, %v", ledger, err)
	}
	sb, err := reg.Get(ctx, "sb-"+intent.ID)
	if err != nil || sb.Status != StatusStarting {
		t.Fatalf("publication row survived: %+v, %v", sb, err)
	}
	if err := reg.RecordCreateFailure(ctx, sb.ID, CreateFailure{Status: 500, Code: "identity_failed", Detail: "Identity failed."}); err == nil {
		t.Fatal("failure recording ignored progress failure")
	}
	ledger, err = reg.CreateRequest(ctx, intent)
	if err != nil || ledger.Code != "" {
		t.Fatalf("pending cause survived rollback: %+v, %v", ledger, err)
	}
	if after := getProgress(t, reg, intent.ID); !reflect.DeepEqual(before, after) {
		t.Fatal("failed transaction changed progress")
	}
	allow()
	if err := reg.Destroy(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	final := getProgress(t, reg, intent.ID)
	if final.Condition != "failed" || final.Outcome.Code != "create_interrupted" || final.Current.Stage != CreateStageIdentity {
		t.Fatalf("interrupted stage: %+v", final)
	}
}

func TestCreateProgressPreallocationFailureAndLegacy(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	intent := beginProgress(t, reg, "missing-source")
	advanceProgress(t, reg, intent, CreateStageSource)
	before := getProgress(t, reg, intent.ID)
	if _, err := reg.db.Exec(`CREATE TRIGGER reject_preallocation_progress BEFORE UPDATE ON create_progress BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := reg.FailCreateRequest(ctx, intent, 404, "source_not_found", "The source was not found."); err == nil {
		t.Fatal("failure ignored progress write error")
	}
	if _, err := reg.CreateRequest(ctx, intent); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("preallocation failure ledger survived: %v", err)
	}
	if _, err := reg.db.Exec(`DROP TRIGGER reject_preallocation_progress`); err != nil {
		t.Fatal(err)
	}
	if err := reg.FailCreateRequest(ctx, intent, 404, "source_not_found", "The source was not found."); err != nil {
		t.Fatal(err)
	}
	after := getProgress(t, reg, intent.ID)
	if after.Condition != "failed" || after.Outcome.Status != 404 || after.Outcome.Code != "source_not_found" || !reflect.DeepEqual(before.Current, after.Current) {
		t.Fatalf("preallocation failure: %+v", after)
	}
	if err := reg.FailCreateRequest(ctx, intent, 500, "different", "Different."); err != nil {
		t.Fatal(err)
	}
	if replay := getProgress(t, reg, intent.ID); !reflect.DeepEqual(after, replay) {
		t.Fatal("failure replay changed terminal observation")
	}
	legacy := requestIntent("legacy")
	if err := reg.AdvanceCreateProgress(ctx, legacy, CreateStageReady); err != nil {
		t.Fatal(err)
	}
	if err := reg.FailCreateRequest(ctx, legacy, 500, "legacy_error", "Legacy error."); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateProgress(ctx, legacy.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("invented legacy observation: %v", err)
	}
}

func TestCreateProgressPendingBoundsAndOrder(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	b := beginProgress(t, reg, "b")
	beginProgress(t, reg, "a")
	pending, err := reg.PendingCreateProgress(ctx, 1)
	if err != nil || len(pending) != 1 || pending[0].ID != "a" {
		t.Fatalf("pending tie ordering: %+v, %v", pending, err)
	}
	advanceProgress(t, reg, b, CreateStageSource)
	pending, err = reg.PendingCreateProgress(ctx, 2)
	if err != nil || len(pending) != 2 || pending[0].ID != "a" || pending[1].ID != "b" || pending[0].Sequence >= pending[1].Sequence {
		t.Fatalf("pending sequence ordering: %+v, %v", pending, err)
	}
	for _, limit := range []int{0, -1, 1001} {
		if _, err := reg.PendingCreateProgress(ctx, limit); err == nil {
			t.Fatalf("accepted unbounded limit %d", limit)
		}
	}
	for _, intent := range []CreateIntent{{ID: "empty-hash"}, {Hash: sha256.Sum256([]byte("no-id"))}} {
		if _, err := reg.BeginCreateProgress(ctx, intent); err == nil {
			t.Fatalf("accepted invalid intent: %+v", intent)
		}
	}
}
