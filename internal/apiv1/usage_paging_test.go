package apiv1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

// ledgerOfSize is a stand-in worker holding `size` intervals, applying the same
// row cap the real one does: an absent `limit` means 1000, the maximum is 5000,
// and a response that was cut short says so. Totals always cover everything,
// which is what makes the truncation invisible to anyone reading only the money.
func ledgerOfSize(t *testing.T, size int) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := 1000
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 5000 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "limit must be between 1 and 5000"})
				return
			}
			limit = n
		}
		started := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
		rows := []registry.UsageInterval{}
		for i := 0; i < size && i < limit; i++ {
			ended := started.Add(time.Duration(i) * time.Second)
			rows = append(rows, registry.UsageInterval{
				ID:        fmt.Sprintf("worker-1:sb%04d:1", i),
				SandboxID: fmt.Sprintf("sb%04d", i),
				Seq:       1, HostID: "worker-1", Vcpus: 2, MemMIB: 1024,
				StartedAt: started, EndedAt: &ended, LastSeenAt: ended,
			})
		}
		_ = json.NewEncoder(w).Encode(registry.UsageReport{
			HostID:    "worker-1",
			Intervals: rows,
			Totals:    registry.UsageTotals{Intervals: int64(size)},
			Truncated: size > limit,
		})
	})
}

// The adapter paginates over the rows it fetched, so it must fetch enough of
// them to serve the page being asked for. Without that, next_page_token walks a
// caller confidently off the end of the ledger's default row cap: the totals
// keep saying there are thousands of intervals while the pages run out at a
// thousand, and the missing ones are indistinguishable from usage that never
// happened.
func TestUsagePagesPastTheLedgerDefaultRowCap(t *testing.T) {
	const size = 2400
	h := testHandler(t, ledgerOfSize(t, size))

	seen := map[string]bool{}
	token := ""
	for pages := 0; pages < 100; pages++ {
		url := "/v1/usage?page_size=100"
		if token != "" {
			url += "&page_token=" + token
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
		if w.Code != 200 {
			t.Fatalf("page %d status=%d body=%s", pages, w.Code, w.Body.String())
		}
		var body usageResponse
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, iv := range body.Intervals {
			if seen[iv.ID] {
				t.Fatalf("interval %s served on two pages", iv.ID)
			}
			seen[iv.ID] = true
		}
		token = body.NextPageToken
		if token == "" {
			break
		}
	}
	if len(seen) != size {
		t.Fatalf("paging reached %d of %d intervals: the rest are invisible to any caller, while totals still count them", len(seen), size)
	}
}

// The row cap is real and must stay honest: past what one response can hold,
// the caller is told the rows were truncated rather than being handed a short
// list that looks complete.
func TestUsageBeyondTheMaximumRowCapReportsTruncation(t *testing.T) {
	h := testHandler(t, ledgerOfSize(t, 6000))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/usage?page_size=1", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body usageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Coverage.Truncated {
		t.Fatal("a ledger larger than the row cap must report coverage.truncated")
	}
	if body.Totals.Intervals != 6000 {
		t.Fatalf("totals.intervals = %d, want 6000: truncation must not change the amount owed", body.Totals.Intervals)
	}
}
