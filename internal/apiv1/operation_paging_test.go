package apiv1

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/httpapi"
)

func operationPagingFixture(t *testing.T) (http.Handler, *createops.SQLiteStore, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "operations.db")
	store, err := createops.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mux := http.NewServeMux()
	NewWithCreateOperations(http.NotFoundHandler(), store, recoveryExecutor{}).Register(mux)
	return mux, store, db
}

func acceptPagingOperation(t *testing.T, store *createops.SQLiteStore, db *sql.DB, id, created string) {
	t.Helper()
	_, err := store.Accept(context.Background(), createops.AcceptRequest{
		ID: id, Scope: id, BodyHash: []byte(id), Type: "sandbox_batch_create", MaxParallelism: 1,
		Members: []createops.Member{{ID: id + "-member"}}, ResponseStatus: 202, ResponseBody: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE create_operations SET created_at=? WHERE id=?`, created, id); err != nil {
		t.Fatal(err)
	}
}

func readOperationPage(t *testing.T, handler http.Handler, token string) ([]Operation, string) {
	t.Helper()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/operations?page_size=1&page_token="+url.QueryEscape(token), nil))
	var page struct {
		Operations []Operation `json:"operations"`
		Next       string      `json:"next_page_token"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatalf("operation page: %d %s", w.Code, w.Body.String())
	}
	return page.Operations, page.Next
}

func TestOperationPageDoesNotDecodeUnrequestedHistory(t *testing.T) {
	handler, store, db := operationPagingFixture(t)
	acceptPagingOperation(t, store, db, "old", "2026-01-01T00:00:00Z")
	acceptPagingOperation(t, store, db, "new", "2026-01-02T00:00:00Z")
	if _, err := db.Exec(`UPDATE create_members SET spec='invalid-json' WHERE operation_id='old'`); err != nil {
		t.Fatal(err)
	}
	page, token := readOperationPage(t, handler, "")
	if len(page) != 1 || page[0].ID != "new" || token == "" {
		t.Fatalf("page=%+v token=%q", page, token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/operations?page_size=1&page_token="+url.QueryEscape(token), nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("selected corrupt member was hidden: %d %s", w.Code, w.Body.String())
	}
}

func TestOperationPageDoesNotRepeatAfterConcurrentCreate(t *testing.T) {
	handler, store, db := operationPagingFixture(t)
	acceptPagingOperation(t, store, db, "old", "2026-01-01T00:00:00Z")
	acceptPagingOperation(t, store, db, "new", "2026-01-02T00:00:00Z")
	first, token := readOperationPage(t, handler, "")
	if len(first) != 1 || first[0].ID != "new" || token == "" {
		t.Fatalf("first page=%+v token=%q", first, token)
	}
	acceptPagingOperation(t, store, db, "concurrent", "2026-01-03T00:00:00Z")
	second, _ := readOperationPage(t, handler, token)
	if len(second) != 1 || second[0].ID != "old" {
		t.Fatalf("new acceptance shifted the next page: %+v", second)
	}
}

func TestOperationPagesSortFractionalCreationTimes(t *testing.T) {
	handler, store, db := operationPagingFixture(t)
	acceptPagingOperation(t, store, db, "whole-second", "2026-01-01T00:00:00Z")
	acceptPagingOperation(t, store, db, "fraction", "2026-01-01T00:00:00.1Z")
	page, _ := readOperationPage(t, handler, "")
	if len(page) != 1 || page[0].ID != "fraction" {
		t.Fatalf("creation timestamps were sorted lexically: %+v", page)
	}
}

func TestOperationPagesAcceptLegacyOffsetsAndIssueKeysetTokens(t *testing.T) {
	handler, store, db := operationPagingFixture(t)
	for _, id := range []string{"a", "b", "c"} {
		acceptPagingOperation(t, store, db, id, "2026-01-01T00:00:00Z")
	}
	page, token := readOperationPage(t, handler, "1")
	if len(page) != 1 || page[0].ID != "b" || !strings.HasPrefix(token, "op1.") {
		t.Fatalf("legacy offset: %+v, next=%q", page, token)
	}
	page, next := readOperationPage(t, handler, token)
	if len(page) != 1 || page[0].ID != "a" || next != "" {
		t.Fatalf("keyset continuation: %+v, next=%q", page, next)
	}
	page, next = readOperationPage(t, handler, "3")
	if len(page) != 0 || next != "" {
		t.Fatalf("end offset: %+v, next=%q", page, next)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/operations?page_token=4", nil))
	var problem httpapi.Problem
	if w.Code != http.StatusBadRequest || json.Unmarshal(w.Body.Bytes(), &problem) != nil || problem.Code != "invalid_page_token" {
		t.Fatalf("out-of-range offset: %d %s", w.Code, w.Body.String())
	}
}

func TestOperationPageValidationPrecedesHistoryReads(t *testing.T) {
	handler, store, db := operationPagingFixture(t)
	acceptPagingOperation(t, store, db, "corrupt", "2026-01-01T00:00:00Z")
	if _, err := db.Exec(`UPDATE create_members SET spec='invalid-json'`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ key, value, code string }{
		{"page_size", "0", "invalid_page_size"},
		{"page_size", "101", "invalid_page_size"},
		{"page_size", "1.5", "invalid_page_size"},
		{"page_size", "large", "invalid_page_size"},
		{"page_token", "-1", "invalid_page_token"},
		{"page_token", "op1.", "invalid_page_token"},
		{"page_token", "op1.YQ==", "invalid_page_token"},
		{"page_token", "op1.YR", "invalid_page_token"},
		{"page_token", "op2.YQ", "invalid_page_token"},
		{"page_token", "op1." + strings.Repeat("A", 341), "invalid_page_token"},
		{"page_token", "op1." + base64.RawURLEncoding.EncodeToString([]byte("missing")), "invalid_page_token"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/operations?"+url.Values{tc.key: {tc.value}}.Encode(), nil))
			var problem httpapi.Problem
			if w.Code != http.StatusBadRequest || json.Unmarshal(w.Body.Bytes(), &problem) != nil || problem.Code != tc.code {
				t.Fatalf("invalid page consulted unrelated history: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
