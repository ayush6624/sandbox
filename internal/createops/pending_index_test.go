package createops

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ayush6624/sandbox/internal/registry"
)

func TestPendingIndexMigrationAndRecoveryMembership(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "operations.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.db.Exec(`DROP INDEX create_operations_pending_order;
CREATE INDEX create_operations_pending ON create_operations(created_at,id) WHERE ` + pendingOperationsPredicate + `;
WITH RECURSIVE history(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM history WHERE n<2000)
INSERT INTO create_operations(id,scope,body_hash,type,max_parallelism,status,accepted_status,accepted_body,final_status,final_body,created_at,completed_at)
SELECT 'history-'||n,'history-'||n,X'01',CASE WHEN n%2=0 THEN 'sandbox_create' ELSE 'sandbox_batch_create' END,
1,'succeeded',202,X'7B7D',CASE WHEN n%2=0 THEN 201 END,CASE WHEN n%2=0 THEN X'7B7D' END,
'2026-01-01T00:00:00Z','2026-01-01T00:00:01Z' FROM history`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pending-single", "pending-batch", "running-single", "running-batch", "terminal-success", "terminal-failure"} {
		req := request(id, id, "body")
		if !strings.HasSuffix(id, "batch") {
			req.Type = "sandbox_create"
		}
		if _, err := s.Accept(ctx, req); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(id, "pending") {
			continue
		}
		if err := s.Assign(ctx, id, []string{id + "-0"}, Worker{HostID: "host", RegistryID: "registry"}); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(id, "running") {
			continue
		}
		outcome := Outcome{ID: id + "-0", Sandbox: &registry.Sandbox{ID: "sandbox", Status: registry.StatusRunning}}
		if id == "terminal-failure" {
			outcome.Sandbox = nil
			outcome.Failure = &Failure{Status: 409, Code: "denied"}
		}
		if _, err := s.Record(ctx, id, []Outcome{outcome}); err != nil {
			t.Fatal(err)
		}
	}
	for reopen := 0; reopen < 2; reopen++ {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var legacyIndex int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='create_operations_pending'`).Scan(&legacyIndex); err != nil || legacyIndex != 0 {
			t.Fatalf("legacy pending index count=%d err=%v", legacyIndex, err)
		}
		rows, err := s.db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT id FROM create_operations WHERE `+pendingOperationsPredicate+` ORDER BY created_at_order ASC,id ASC`)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
		joined := strings.Join(plan, "\n")
		if !strings.Contains(joined, "USING INDEX create_operations_pending_order") || strings.Contains(joined, "TEMP B-TREE") || slices.Contains(plan, "SCAN create_operations") {
			t.Fatalf("pending query scans retained history or sorts: %s", joined)
		}
		pending, err := s.Pending(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, op := range pending {
			got = append(got, op.ID)
		}
		slices.Sort(got)
		want := []string{"pending-batch", "pending-single", "running-batch", "running-single"}
		if reopen == 0 {
			want = append(want, "terminal-failure", "terminal-success")
		}
		if !slices.Equal(got, want) {
			t.Fatalf("pending = %v, want %v", got, want)
		}
		for _, id := range []string{"terminal-success", "terminal-failure"} {
			wantStatus := 201
			if id == "terminal-failure" {
				wantStatus = 409
			}
			if reopen == 0 {
				if err := s.SetFinalResponse(ctx, id, wantStatus, []byte(`{"retained":true}`)); err != nil {
					t.Fatal(err)
				}
			}
			status, body, ok, err := s.FinalResponse(ctx, id)
			if err != nil || !ok || status != wantStatus || string(body) != `{"retained":true}` {
				t.Fatalf("final response changed: %d %s %v %v", status, body, ok, err)
			}
		}
		var retained int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM create_operations WHERE id LIKE 'history-%'`).Scan(&retained); err != nil || retained != 2000 {
			t.Fatalf("retained history count=%d err=%v", retained, err)
		}
	}
}
