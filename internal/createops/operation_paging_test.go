package createops

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func pagingStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "operations.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func acceptAt(t *testing.T, s *SQLiteStore, id, created string) {
	t.Helper()
	if _, err := s.Accept(context.Background(), request(id, id, id)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE create_operations SET created_at=? WHERE id=?`, created, id); err != nil {
		t.Fatal(err)
	}
}

func TestOperationPageDoesNotHydrateLookaheadOrSkippedMembers(t *testing.T) {
	s := pagingStore(t)
	acceptAt(t, s, "old", "2026-09-13T10:00:00Z")
	acceptAt(t, s, "new", "2026-09-13T10:00:01Z")
	if _, err := s.db.Exec(`UPDATE create_members SET spec='invalid' WHERE operation_id='old'`); err != nil {
		t.Fatal(err)
	}
	page, err := s.List(context.Background(), ListQuery{Limit: 1})
	if err != nil || len(page.Operations) != 1 || page.Operations[0].ID != "new" || page.NextID != "new" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if _, err := s.List(context.Background(), ListQuery{Limit: 1, BeforeID: page.NextID}); err == nil {
		t.Fatal("selected invalid member must fail")
	}
	if _, err := s.db.Exec(`UPDATE create_members SET spec='{}' WHERE operation_id='old'; UPDATE create_members SET spec='invalid' WHERE operation_id='new'`); err != nil {
		t.Fatal(err)
	}
	for _, query := range []ListQuery{{Limit: 1, BeforeID: "new"}, {Limit: 1, Offset: 1}} {
		page, err := s.List(context.Background(), query)
		if err != nil || len(page.Operations) != 1 || page.Operations[0].ID != "old" || page.NextID != "" {
			t.Fatalf("page=%+v err=%v", page, err)
		}
	}
}

func TestOperationPageChronologicalOrderAndInsert(t *testing.T) {
	s := pagingStore(t)
	for _, row := range []struct{ id, created string }{
		{"second", "2026-09-13T10:00:00Z"},
		{"tenth", "2026-09-13T10:00:00.1Z"},
		{"nano", "2026-09-13T10:00:00.100000001Z"},
		{"tie-a", "2026-09-13T10:00:00.11Z"},
		{"tie-b", "2026-09-13T10:00:00.11Z"},
	} {
		acceptAt(t, s, row.id, row.created)
	}
	var got []string
	cursor := ""
	for n := 0; n < 10; n++ {
		page, err := s.List(context.Background(), ListQuery{Limit: 1, BeforeID: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range page.Operations {
			got = append(got, op.ID)
		}
		if n == 0 {
			acceptAt(t, s, "newer", "2026-09-13T10:00:01Z")
		}
		if page.NextID == "" {
			break
		}
		cursor = page.NextID
	}
	want := []string{"tie-b", "tie-a", "nano", "tenth", "second"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestOperationPageBounds(t *testing.T) {
	s := pagingStore(t)
	ctx := context.Background()
	for _, q := range []ListQuery{{}, {Limit: -1}, {Limit: 101}, {Limit: 1, Offset: -1}, {Limit: 1, Offset: 1, BeforeID: "id"}, {Limit: 1, BeforeID: "missing"}, {Limit: 1, Offset: 1}} {
		if _, err := s.List(ctx, q); !errors.Is(err, ErrInvalidPage) {
			t.Fatalf("query=%+v err=%v", q, err)
		}
	}
	page, err := s.List(ctx, ListQuery{Limit: 100})
	if err != nil || page.Operations == nil || len(page.Operations) != 0 || page.NextID != "" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	acceptAt(t, s, "one", "2026-09-13T10:00:00Z")
	for _, q := range []ListQuery{{Limit: 1, Offset: 1}, {Limit: 1, BeforeID: "one"}} {
		page, err = s.List(ctx, q)
		if err != nil || page.Operations == nil || len(page.Operations) != 0 || page.NextID != "" {
			t.Fatalf("page=%+v err=%v", page, err)
		}
	}
	if _, err := s.List(ctx, ListQuery{Limit: 1, Offset: 2}); !errors.Is(err, ErrInvalidPage) {
		t.Fatal(err)
	}
}

func TestOperationPageHistoryIndexSeeksBothKeys(t *testing.T) {
	s := pagingStore(t)
	for i := 0; i < 105; i++ {
		acceptAt(t, s, fmt.Sprintf("id-%03d", i), "2026-09-13T10:00:00Z")
	}
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT id FROM create_operations WHERE (created_at_order,id)<(?,?) ORDER BY created_at_order DESC,id DESC LIMIT ?`, "2026-09-13T10:00:00.000000000Z", "id-099", 2)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "SEARCH create_operations USING INDEX create_operations_history ((created_at_order,id)<(?,?))") || strings.Contains(joined, "TEMP B-TREE") {
		t.Fatalf("query plan: %s", joined)
	}
	page, err := s.List(context.Background(), ListQuery{Limit: 100})
	if err != nil || len(page.Operations) != 100 || page.NextID != "id-005" {
		t.Fatalf("page count=%d next=%q err=%v", len(page.Operations), page.NextID, err)
	}
	tail, err := s.List(context.Background(), ListQuery{Limit: 100, BeforeID: page.NextID})
	if err != nil || len(tail.Operations) != 5 || tail.Operations[0].ID != "id-004" || tail.NextID != "" {
		t.Fatalf("tail=%+v err=%v", tail, err)
	}
}

func TestOperationPageMigrationPreservesStoredReceipts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operations.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	acceptAt(t, s, "old", "2026-09-13T10:00:00Z")
	acceptAt(t, s, "new", "2026-09-13T10:00:00.1Z")
	const accepted = "{  \"accepted\": true }\n"
	const final = "{  \"result\": \"original\" }\n"
	if _, err := s.db.Exec(`UPDATE create_operations SET accepted_body=?,final_body=?,final_status=201 WHERE id='old'; DROP INDEX create_operations_history; DROP INDEX create_operations_pending_order; ALTER TABLE create_operations DROP COLUMN created_at_order`, []byte(accepted), []byte(final)); err != nil {
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
		var created string
		var gotAccepted, gotFinal []byte
		if err := s.db.QueryRow(`SELECT created_at,accepted_body,final_body FROM create_operations WHERE id='old'`).Scan(&created, &gotAccepted, &gotFinal); err != nil {
			t.Fatal(err)
		}
		if created != "2026-09-13T10:00:00Z" || string(gotAccepted) != accepted || string(gotFinal) != final {
			t.Fatalf("stored values changed: %q %q %q", created, gotAccepted, gotFinal)
		}
		page, err := s.List(context.Background(), ListQuery{Limit: 1, BeforeID: "new"})
		if err != nil || len(page.Operations) != 1 || page.Operations[0].ID != "old" {
			t.Fatalf("page=%+v err=%v", page, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPendingChronologicalOrder(t *testing.T) {
	s := pagingStore(t)
	for _, row := range []struct{ id, created string }{
		{"tie-b", "2026-09-13T10:00:00.11Z"},
		{"tenth", "2026-09-13T10:00:00.1Z"},
		{"second", "2026-09-13T10:00:00Z"},
		{"tie-a", "2026-09-13T10:00:00.11Z"},
		{"nano", "2026-09-13T10:00:00.100000001Z"},
	} {
		acceptAt(t, s, row.id, row.created)
	}
	pending, err := s.Pending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, op := range pending {
		got = append(got, op.ID)
	}
	want := []string{"second", "tenth", "nano", "tie-a", "tie-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pending order=%v want %v", got, want)
	}
}
