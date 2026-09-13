package registry

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func storeInlineProgress(t *testing.T, reg *Registry, p CreateProgress) []byte {
	t.Helper()
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.db.Exec(`UPDATE create_progress SET snapshot=? WHERE id=?`, body, p.ID); err != nil {
		t.Fatal(err)
	}
	return body
}

func storedProgressBytes(t *testing.T, reg *Registry, id string) []byte {
	t.Helper()
	var body []byte
	if err := reg.rdb.QueryRow(`SELECT snapshot FROM create_progress WHERE id=?`, id).Scan(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func inlineFailedProgress(t *testing.T, reg *Registry, id string, owned bool) (CreateIntent, CreateProgress) {
	t.Helper()
	var intent CreateIntent
	if owned {
		intent = beginOwnedProgress(t, reg, id, CreateProgressGateway)
	} else {
		intent = beginProgress(t, reg, id)
	}
	advanceProgress(t, reg, intent, CreateStageSource)
	if err := reg.FailCreateRequest(context.Background(), intent, 404, "missing_source", strings.Repeat("historical failure ", 1024)); err != nil {
		t.Fatal(err)
	}
	p := getProgress(t, reg, id)
	storeInlineProgress(t, reg, p)
	return intent, p
}

func TestCompactCreateProgressPreservesPendingWireAndReplayAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "registry.db")
	pools := Pools{TapPrefix: "fc", TapMax: 2, GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.11", PortMin: 5200, PortMax: 5201}
	reg, err := Open(path, pools)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()
	intent, before := inlineFailedProgress(t, reg, "old-inline", true)
	wireBefore, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	bytesBefore := len(storedProgressBytes(t, reg, intent.ID))
	next, err := reg.CompactCreateProgress(ctx, "", 1)
	if err != nil || next != intent.ID {
		t.Fatalf("compact: cursor=%q, %v", next, err)
	}
	if after := getProgress(t, reg, intent.ID); !reflect.DeepEqual(after, before) {
		t.Fatalf("compaction changed observation: %+v", after)
	}
	if bytesAfter := len(storedProgressBytes(t, reg, intent.ID)); bytesAfter >= bytesBefore/2 {
		t.Fatalf("duplicate result retained: before=%d after=%d", bytesBefore, bytesAfter)
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	reg, err = Open(path, pools)
	if err != nil {
		t.Fatal(err)
	}
	for _, pending := range []func() ([]CreateProgress, error){
		func() ([]CreateProgress, error) { return reg.PendingCreateProgress(ctx, 10) },
		func() ([]CreateProgress, error) { return reg.PendingOwnedCreateProgress(ctx, "", 10) },
	} {
		page, err := pending()
		if err != nil || len(page) != 1 {
			t.Fatalf("pending: %+v, %v", page, err)
		}
		wireAfter, err := json.Marshal(page[0])
		if err != nil || !bytes.Equal(wireAfter, wireBefore) {
			t.Fatalf("same-sequence delivery changed: %s, %v", wireAfter, err)
		}
	}
	replay, err := reg.CreateRequest(ctx, intent)
	if err != nil || !reflect.DeepEqual(replay, *before.Outcome) {
		t.Fatalf("replay changed: %+v, %v", replay, err)
	}
	if err := reg.AcknowledgeCreateProgress(ctx, intent.ID, 1); err != nil {
		t.Fatal(err)
	}
	if pending, err := reg.PendingOwnedCreateProgress(ctx, "", 10); err != nil || len(pending) != 1 || pending[0].Sequence != before.Sequence {
		t.Fatalf("stale acknowledgement suppressed terminal delivery: %+v, %v", pending, err)
	}
	for _, sequence := range []int64{before.Sequence, 1, before.Sequence} {
		if err := reg.AcknowledgeCreateProgress(ctx, intent.ID, sequence); err != nil {
			t.Fatalf("ack %d: %v", sequence, err)
		}
	}
	if err := reg.AcknowledgeCreateProgress(ctx, intent.ID, before.Sequence+1); err == nil {
		t.Fatal("accepted future acknowledgement")
	}
	if err := reg.AcknowledgeCreateProgress(ctx, intent.ID, 0); err == nil {
		t.Fatal("accepted zero acknowledgement")
	}
	if err := reg.AcknowledgeCreateProgress(ctx, "missing", 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown ack: %v", err)
	}
	if got := getProgress(t, reg, intent.ID); !reflect.DeepEqual(got, before) {
		t.Fatal("acknowledgement changed historical observation")
	}
	if pending, err := reg.PendingOwnedCreateProgress(ctx, "", 10); err != nil || len(pending) != 0 {
		t.Fatalf("acknowledged delivery remains pending: %+v, %v", pending, err)
	}
	if next, err := reg.CompactCreateProgress(ctx, "", 1); err != nil || next != "" {
		t.Fatalf("repeated compaction: %q, %v", next, err)
	}
}

func TestCompactCreateProgressPreservesAnonymousAndUnfinishedRequests(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	anonymous, terminal := inlineFailedProgress(t, reg, "anonymous", false)
	active := beginOwnedProgress(t, reg, "active", CreateProgressLocal)
	activeBefore := storedProgressBytes(t, reg, active.ID)
	if err := reg.AcknowledgeCreateProgress(ctx, active.ID, 1); err != nil {
		t.Fatal(err)
	}
	allocated := beginOwnedProgress(t, reg, "allocated", CreateProgressLocal)
	advanceProgress(t, reg, allocated, CreateStageSource)
	allocateProgress(t, reg, allocated)
	allocatedBefore := storedProgressBytes(t, reg, allocated.ID)
	legacy := requestIntent("legacy")
	if err := reg.FailCreateRequest(ctx, legacy, 500, "legacy", "Legacy failure."); err != nil {
		t.Fatal(err)
	}
	next, err := reg.CompactCreateProgress(ctx, "", 100)
	if err != nil || next != anonymous.ID {
		t.Fatalf("compact: %q, %v", next, err)
	}
	if got := getProgress(t, reg, anonymous.ID); !reflect.DeepEqual(got, terminal) {
		t.Fatal("anonymous terminal progress changed")
	}
	if got, err := reg.CreateRequest(ctx, anonymous); err != nil || !reflect.DeepEqual(got, *terminal.Outcome) {
		t.Fatalf("anonymous replay: %+v, %v", got, err)
	}
	if !bytes.Equal(activeBefore, storedProgressBytes(t, reg, active.ID)) || !bytes.Equal(allocatedBefore, storedProgressBytes(t, reg, allocated.ID)) {
		t.Fatal("compaction touched unfinished progress")
	}
	if _, err := reg.CreateProgress(ctx, legacy.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("invented legacy progress: %v", err)
	}
	retry, err := reg.BeginCreateProgress(ctx, active)
	if err != nil || retry.Attempt != active.ProgressAttempt+1 {
		t.Fatalf("preallocation retry: %+v, %v", retry, err)
	}
	if err := reg.FailCreateRequest(ctx, active, 500, "stale", "Stale attempt."); !errors.Is(err, ErrCreateProgressStale) {
		t.Fatalf("old attempt accepted: %v", err)
	}
}

func TestCompactCreateProgressMismatchDoesNotStarveLaterRows(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	_, bad := inlineFailedProgress(t, reg, "a-mismatch", true)
	_, good := inlineFailedProgress(t, reg, "b-valid", true)
	bad.Outcome.Detail = "This is not the historical ledger result."
	badBytes := storeInlineProgress(t, reg, bad)
	goodBytes := storedProgressBytes(t, reg, good.ID)
	next, err := reg.CompactCreateProgress(ctx, "", 1)
	if err == nil || next != bad.ID {
		t.Fatalf("anomaly must report error and advance cursor: %q, %v", next, err)
	}
	if !bytes.Equal(badBytes, storedProgressBytes(t, reg, bad.ID)) {
		t.Fatal("mismatching historical value was discarded")
	}
	if !bytes.Equal(goodBytes, storedProgressBytes(t, reg, good.ID)) {
		t.Fatal("page exceeded requested limit")
	}
	next, err = reg.CompactCreateProgress(ctx, next, 1)
	if err != nil || next != good.ID {
		t.Fatalf("later row starved: %q, %v", next, err)
	}
	if bytes.Equal(goodBytes, storedProgressBytes(t, reg, good.ID)) {
		t.Fatal("later valid row was not compacted")
	}
	if got := getProgress(t, reg, bad.ID); !reflect.DeepEqual(got, bad) {
		t.Fatal("inline outcome stopped being authoritative")
	}
	next, err = reg.CompactCreateProgress(ctx, next, 1)
	if err != nil || next != "" {
		t.Fatalf("cursor did not wrap: %q, %v", next, err)
	}
	for _, limit := range []int{-1, 0, 1001} {
		if _, err := reg.CompactCreateProgress(ctx, "", limit); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
}

func TestCompactCreateProgressUpdateFailureCanRetry(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	_, p := inlineFailedProgress(t, reg, "rollback", true)
	before := storedProgressBytes(t, reg, p.ID)
	if _, err := reg.db.Exec(`CREATE TRIGGER reject_compaction BEFORE UPDATE OF snapshot ON create_progress BEGIN SELECT RAISE(ABORT,'injected compaction failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CompactCreateProgress(ctx, "", 1); err == nil {
		t.Fatal("ignored failing update")
	}
	if !bytes.Equal(before, storedProgressBytes(t, reg, p.ID)) {
		t.Fatal("failed transaction changed stored progress")
	}
	if got := getProgress(t, reg, p.ID); !reflect.DeepEqual(got, p) {
		t.Fatal("failed transaction changed materialized progress")
	}
	if _, err := reg.db.Exec(`DROP TRIGGER reject_compaction`); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CompactCreateProgress(ctx, "", 1); err != nil {
		t.Fatal(err)
	}
	if got := getProgress(t, reg, p.ID); !reflect.DeepEqual(got, p) {
		t.Fatal("retry changed materialized progress")
	}
}

func TestCompactCreateProgressUsesSelectiveOrderedIndex(t *testing.T) {
	reg := testRegistry(t)
	inlineFailedProgress(t, reg, "old-inline", true)
	beginProgress(t, reg, "active")
	for _, tt := range []struct{ name, query, index string }{
		{"conversion", `SELECT id FROM create_progress WHERE ` + inlineTerminalCreateProgressPredicate + ` AND id>? ORDER BY id LIMIT ?`, "create_progress_inline_terminal"},
		{"joined delivery", createProgressSelect + ` WHERE ` + ownedCreateProgressPredicate + ` AND p.id>? ORDER BY p.id LIMIT ?`, "create_progress_owned_pending"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := reg.rdb.Query(`EXPLAIN QUERY PLAN `+tt.query, "", 32)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				details = append(details, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			plan := strings.Join(details, "\n")
			if !strings.Contains(plan, tt.index) || strings.Contains(plan, "TEMP B-TREE") {
				t.Fatalf("query must seek its partial index without sorting: %s", plan)
			}
		})
	}
}

func TestCompactCreateProgressRejectsInvalidHistoricalLedger(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct{ name, mutation string }{
		{"missing", `DELETE FROM create_requests WHERE id=?`},
		{"nonterminal", `UPDATE create_requests SET phase='allocated',sandbox_id='unrelated' WHERE id=?`},
		{"hash mismatch", `UPDATE create_requests SET request_hash=zeroblob(32) WHERE id=?`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := testRegistry(t)
			intent, _ := inlineFailedProgress(t, reg, "corrupt-ledger", true)
			if _, err := reg.CompactCreateProgress(ctx, "", 1); err != nil {
				t.Fatal(err)
			}
			if _, err := reg.db.Exec(tt.mutation, intent.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := reg.CreateProgress(ctx, intent.ID); err == nil {
				t.Fatal("materialized terminal progress without matching ledger")
			}
			if _, err := reg.PendingCreateProgress(ctx, 10); err == nil {
				t.Fatal("general pending reader accepted invalid ledger")
			}
			if _, err := reg.PendingOwnedCreateProgress(ctx, "", 10); err == nil {
				t.Fatal("owned pending reader accepted invalid ledger")
			}
		})
	}
}

func TestCompactCreateProgressDoesNotHydrateNonterminalObservation(t *testing.T) {
	ctx := context.Background()
	for _, condition := range []string{"active", "tearing_down"} {
		t.Run(condition, func(t *testing.T) {
			reg := testRegistry(t)
			intent, p := inlineFailedProgress(t, reg, "nonterminal-observation", true)
			p.Condition = condition
			p.Outcome = nil
			body := storeInlineProgress(t, reg, p)
			if got := getProgress(t, reg, intent.ID); !reflect.DeepEqual(got, p) {
				t.Fatalf("attached terminal ledger to %s observation: %+v", condition, got)
			}
			pending, err := reg.PendingOwnedCreateProgress(ctx, "", 10)
			if err != nil || len(pending) != 1 || !reflect.DeepEqual(pending[0], p) {
				t.Fatalf("pending observation gained terminal outcome: %+v, %v", pending, err)
			}
			if next, err := reg.CompactCreateProgress(ctx, "", 10); err != nil || next != "" {
				t.Fatalf("nonterminal snapshot selected for conversion: %q, %v", next, err)
			}
			if !bytes.Equal(body, storedProgressBytes(t, reg, intent.ID)) {
				t.Fatal("conversion changed nonterminal snapshot")
			}
		})
	}
}
