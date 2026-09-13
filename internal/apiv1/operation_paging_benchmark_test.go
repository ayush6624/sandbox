package apiv1

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ayush6624/sandbox/internal/createops"
)

func BenchmarkOperationFirstPage(b *testing.B) {
	for _, history := range []int{100, 10000} {
		b.Run(fmt.Sprintf("history-%d", history), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "operations.db")
			store, err := createops.Open(path)
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()
			db, err := sql.Open("sqlite", path)
			if err != nil {
				b.Fatal(err)
			}
			_, err = db.Exec(`WITH RECURSIVE history(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM history WHERE n<?)
INSERT INTO create_operations(id,scope,body_hash,type,max_parallelism,status,accepted_status,accepted_body,created_at,completed_at)
SELECT printf('op-%05d',n),printf('scope-%05d',n),X'01','sandbox_batch_create',1,'failed',202,X'7B7D',
'2026-01-01T00:00:00Z','2026-01-01T00:00:01Z' FROM history`, history)
			if err == nil {
				_, err = db.Exec(`INSERT INTO create_members(operation_id,member_id,item_index,spec,outcome)
SELECT id,id||'-member',0,json_object('Metadata',json_object('retained',printf('%01024d',0))),
json_object('ID',id||'-member','Failure',json_object('Status',404,'Code','missing','Detail','Source unavailable.')) FROM create_operations`)
			}
			db.Close()
			if err != nil {
				b.Fatal(err)
			}
			mux := http.NewServeMux()
			NewWithCreateOperations(http.NotFoundHandler(), store, recoveryExecutor{}).Register(mux)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/operations?page_size=1", nil))
				if w.Code != http.StatusOK {
					b.Fatalf("list: %d %s", w.Code, w.Body.String())
				}
			}
		})
	}
}
