package registry

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func ownedProgressIntent(id string, target CreateProgressTarget) CreateIntent {
	intent := requestIntent(id)
	intent.ProgressOwner = CreateProgressOwner{CoordinatorID: "coordinator", OperationID: "operation"}
	intent.ProgressTarget = target
	return intent
}

func beginOwnedProgress(t *testing.T, reg *Registry, id string, target CreateProgressTarget) CreateIntent {
	t.Helper()
	intent := ownedProgressIntent(id, target)
	p, err := reg.BeginCreateProgress(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	intent.ProgressAttempt = p.Attempt
	return intent
}

func TestCreateProgressOwnershipFencesEveryIntentMutation(t *testing.T) {
	ctx := context.Background()
	for _, target := range []CreateProgressTarget{CreateProgressLocal, CreateProgressGateway} {
		t.Run(string(target), func(t *testing.T) {
			reg := testRegistry(t)
			intent := beginOwnedProgress(t, reg, "owned", target)
			advanceProgress(t, reg, intent, CreateStageSource)
			before := getProgress(t, reg, intent.ID)
			for _, change := range []struct {
				name  string
				apply func(*CreateIntent)
			}{
				{"coordinator", func(i *CreateIntent) { i.ProgressOwner.CoordinatorID = "other" }},
				{"operation", func(i *CreateIntent) { i.ProgressOwner.OperationID = "other" }},
				{"target", func(i *CreateIntent) {
					if target == CreateProgressLocal {
						i.ProgressTarget = CreateProgressGateway
					} else {
						i.ProgressTarget = CreateProgressLocal
					}
				}},
				{"anonymous", func(i *CreateIntent) { i.ProgressOwner = CreateProgressOwner{}; i.ProgressTarget = "" }},
			} {
				t.Run(change.name, func(t *testing.T) {
					other := intent
					change.apply(&other)
					if _, err := reg.BeginCreateProgress(ctx, other); !errors.Is(err, ErrCreateRequestConflict) {
						t.Fatalf("begin: %v", err)
					}
					if err := reg.AdvanceCreateProgress(ctx, other, CreateStageSource); !errors.Is(err, ErrCreateRequestConflict) {
						t.Fatalf("advance: %v", err)
					}
					if _, err := reg.CreateStarting(ctx, "conflict", "", "/tmp/disk", nil, "", 0, 1, 128, other); !errors.Is(err, ErrCreateRequestConflict) {
						t.Fatalf("allocate: %v", err)
					}
					if err := reg.FailCreateRequest(ctx, other, 500, "failed", "Failed."); !errors.Is(err, ErrCreateRequestConflict) {
						t.Fatalf("failure: %v", err)
					}
					other.ProgressAttempt = 0
					if err := reg.AdvanceCreateProgress(ctx, other, CreateStageSource); !errors.Is(err, ErrCreateRequestConflict) {
						t.Fatalf("advance without attempt: %v", err)
					}
				})
			}
			if got := getProgress(t, reg, intent.ID); !reflect.DeepEqual(got, before) {
				t.Fatal("rejected ownership changed observation")
			}
			retry, err := reg.BeginCreateProgress(ctx, intent)
			if err != nil {
				t.Fatal(err)
			}
			if retry.Owner != intent.ProgressOwner || retry.Target != target || retry.Attempt != 2 {
				t.Fatalf("retry: %+v", retry)
			}
			intent.ProgressAttempt = retry.Attempt
			advanceProgress(t, reg, intent, CreateStageSource)
			allocateProgress(t, reg, intent)
			if err := reg.RecordCreateFailure(ctx, "sb-"+intent.ID, CreateFailure{Status: 500, Code: "failed", Detail: "Failed."}); err != nil {
				t.Fatal(err)
			}
			if err := reg.Destroy(ctx, "sb-"+intent.ID); err != nil {
				t.Fatal(err)
			}
			final := getProgress(t, reg, intent.ID)
			if final.Owner != intent.ProgressOwner || final.Target != target || final.Outcome == nil || final.Outcome.Phase != "failed" {
				t.Fatalf("terminal owner lost: %+v", final)
			}
		})
	}
}

func TestCreateProgressOwnershipFencesTerminalReplay(t *testing.T) {
	ctx := context.Background()
	for _, success := range []bool{false, true} {
		reg := testRegistry(t)
		intent := beginOwnedProgress(t, reg, "terminal", CreateProgressLocal)
		if success {
			advanceProgress(t, reg, intent, CreateStageSource)
			allocateProgress(t, reg, intent)
			if err := reg.MarkRunning(ctx, "sb-"+intent.ID); err != nil {
				t.Fatal(err)
			}
		} else if err := reg.FailCreateRequest(ctx, intent, 404, "missing", "Missing."); err != nil {
			t.Fatal(err)
		}
		before := getProgress(t, reg, intent.ID)
		for _, other := range []CreateIntent{
			func() CreateIntent { i := intent; i.ProgressOwner.CoordinatorID = "other"; return i }(),
			func() CreateIntent { i := intent; i.ProgressOwner.OperationID = "other"; return i }(),
			func() CreateIntent { i := intent; i.ProgressTarget = CreateProgressGateway; return i }(),
			requestIntent(intent.ID),
		} {
			if _, err := reg.CreateRequest(ctx, other); !errors.Is(err, ErrCreateRequestConflict) {
				t.Fatalf("terminal ledger replay: %v", err)
			}
			if _, err := reg.BeginCreateProgress(ctx, other); !errors.Is(err, ErrCreateRequestConflict) {
				t.Fatalf("terminal begin replay: %v", err)
			}
			other.ProgressAttempt = intent.ProgressAttempt
			if err := reg.FailCreateRequest(ctx, other, 500, "different", "Different."); !errors.Is(err, ErrCreateRequestConflict) {
				t.Fatalf("terminal failure replay: %v", err)
			}
		}
		replay := intent
		replay.ProgressAttempt = 0
		if _, err := reg.CreateRequest(ctx, replay); err != nil {
			t.Fatalf("matching owner replay: %v", err)
		}
		if got := getProgress(t, reg, intent.ID); !reflect.DeepEqual(got, before) {
			t.Fatal("replay changed terminal observation")
		}
	}
}

func TestCreateProgressOwnerValidationAndAnonymousBinding(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	for _, i := range []CreateIntent{
		{ProgressOwner: CreateProgressOwner{CoordinatorID: "c"}},
		{ProgressOwner: CreateProgressOwner{OperationID: "o"}},
		{ProgressOwner: CreateProgressOwner{CoordinatorID: "c", OperationID: "o"}},
		{ProgressTarget: CreateProgressLocal},
		{ProgressOwner: CreateProgressOwner{CoordinatorID: "c", OperationID: "o"}, ProgressTarget: "unknown"},
	} {
		i.ID = "invalid"
		i.Hash = requestIntent(i.ID).Hash
		if _, err := reg.BeginCreateProgress(ctx, i); err == nil {
			t.Fatalf("accepted partial owner: %+v", i)
		}
		if _, err := reg.CreateRequest(ctx, i); err == nil {
			t.Fatal("replay accepted partial owner")
		}
		if err := reg.AdvanceCreateProgress(ctx, i, CreateStageSource); err == nil {
			t.Fatal("advance accepted partial owner")
		}
		if err := reg.FailCreateRequest(ctx, i, 500, "failed", "Failed."); err == nil {
			t.Fatal("failure accepted partial owner")
		}
	}
	legacy := beginProgress(t, reg, "anonymous")
	owned := ownedProgressIntent(legacy.ID, CreateProgressLocal)
	if _, err := reg.BeginCreateProgress(ctx, owned); !errors.Is(err, ErrCreateRequestConflict) {
		t.Fatalf("inferred owner at retry: %v", err)
	}
	if err := reg.FailCreateRequest(ctx, legacy, 500, "failed", "Failed."); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateRequest(ctx, owned); !errors.Is(err, ErrCreateRequestConflict) {
		t.Fatalf("inferred owner at replay: %v", err)
	}
	unobserved := requestIntent("old-ledger")
	if err := reg.FailCreateRequest(ctx, unobserved, 500, "failed", "Failed."); err != nil {
		t.Fatal(err)
	}
	owned = ownedProgressIntent(unobserved.ID, CreateProgressLocal)
	if _, err := reg.CreateRequest(ctx, owned); !errors.Is(err, ErrCreateRequestConflict) {
		t.Fatalf("inferred owner on old ledger: %v", err)
	}
	if pending, err := reg.PendingOwnedCreateProgress(ctx, "", 100); err != nil || len(pending) != 0 {
		t.Fatalf("anonymous delivery: %+v, %v", pending, err)
	}
}

func TestPendingOwnedCreateProgressFairCursorAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "registry.db")
	pools := Pools{TapPrefix: "fc", TapMax: 2, GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.11", PortMin: 5200, PortMax: 5201}
	reg, err := Open(path, pools)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()
	legacy := beginProgress(t, reg, "0-legacy")
	a := beginOwnedProgress(t, reg, "a", CreateProgressGateway)
	advanceProgress(t, reg, a, CreateStageSource)
	beginOwnedProgress(t, reg, "b", CreateProgressLocal)
	beginOwnedProgress(t, reg, "c", CreateProgressGateway)
	page := func(after string, limit int) []CreateProgress {
		t.Helper()
		ps, err := reg.PendingOwnedCreateProgress(ctx, after, limit)
		if err != nil {
			t.Fatal(err)
		}
		return ps
	}
	first := page("", 1)
	if len(first) != 1 || first[0].ID != "a" {
		t.Fatalf("ID ordering follows sequence: %+v", first)
	}
	// Leave a unacknowledged and add lower-ID traffic while walking the cursor.
	beginOwnedProgress(t, reg, "0-new", CreateProgressLocal)
	second := page(first[0].ID, 1)
	if len(second) != 1 || second[0].ID != "b" {
		t.Fatalf("blocked on unacknowledged row: %+v", second)
	}
	third := page(second[0].ID, 1)
	if len(third) != 1 || third[0].ID != "c" {
		t.Fatalf("starved later row: %+v", third)
	}
	if got := page(third[0].ID, 1); len(got) != 0 {
		t.Fatalf("cursor didn't end: %+v", got)
	}
	if got := page("", 1); len(got) != 1 || got[0].ID != "0-new" {
		t.Fatalf("wrap missed new row: %+v", got)
	}
	if err := reg.AcknowledgeCreateProgress(ctx, "b", second[0].Sequence); err != nil {
		t.Fatal(err)
	}
	if got := page("a", 10); len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("acknowledged row pending: %+v", got)
	}
	for _, limit := range []int{-1, 0, 1001} {
		if _, err := reg.PendingOwnedCreateProgress(ctx, "", limit); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
	if _, err := reg.db.Exec(`DROP INDEX create_progress_owned_pending`); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.db.Exec(`UPDATE create_progress SET snapshot=json_remove(snapshot,'$.Owner','$.Target') WHERE id=?`, legacy.ID); err != nil {
		t.Fatal(err)
	}
	before := page("", 100)
	legacyBefore := getProgress(t, reg, legacy.ID)
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	reg, err = Open(path, pools)
	if err != nil {
		t.Fatal(err)
	}
	if got := page("", 100); !reflect.DeepEqual(got, before) {
		t.Fatalf("reopen changed owned delivery: %+v", got)
	}
	if got := getProgress(t, reg, legacy.ID); !reflect.DeepEqual(got, legacyBefore) {
		t.Fatalf("migration changed legacy: %+v", got)
	}
	var plan string
	var id, parent, unused int
	if err := reg.rdb.QueryRowContext(ctx, `EXPLAIN QUERY PLAN SELECT snapshot FROM create_progress WHERE `+ownedCreateProgressPredicate+` AND id>? ORDER BY id LIMIT ?`, "a", 10).Scan(&id, &parent, &unused, &plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "create_progress_owned_pending") {
		t.Fatalf("cursor scan does not use owned index: %s", plan)
	}
}
