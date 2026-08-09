package apiv1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

func usageLegacy(t *testing.T, wantPath string, report registry.UsageReport, status int) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath {
			t.Errorf("legacy path=%q, want %q", r.URL.Path, wantPath)
			http.NotFound(w, r)
			return
		}
		// Pagination is applied by the adapter over the rows it gets back, so
		// page_size/page_token must never reach the ledger, where they are not
		// parameters at all.
		for _, forbidden := range []string{"page_size", "page_token"} {
			if r.URL.Query().Has(forbidden) {
				t.Errorf("%s leaked to the legacy route: %s", forbidden, r.URL.RawQuery)
			}
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "nope"})
			return
		}
		_ = json.NewEncoder(w).Encode(report)
	})
}

func sampleReport() registry.UsageReport {
	started := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	ended := started.Add(10 * time.Minute)
	return registry.UsageReport{
		HostID: "worker-1",
		Intervals: []registry.UsageInterval{
			{
				ID: "worker-1:sb1:2", SandboxID: "sb1", Seq: 2, HostID: "worker-1",
				Vcpus: 2, MemMIB: 1024,
				StartedAt: started, EndedAt: &ended, LastSeenAt: ended,
				CPUUsec: 90_000_000, EndReason: registry.EndHibernate,
				Metadata: map[string]string{"team": "core"},
			},
			{
				ID: "worker-1:sb2:1", SandboxID: "sb2", Seq: 1, HostID: "worker-1",
				Vcpus: 4, MemMIB: 4096,
				StartedAt: started, LastSeenAt: started.Add(5 * time.Minute),
			},
		},
		Totals: registry.UsageTotals{
			Intervals: 2, OpenIntervals: 1, DurationSeconds: 900,
			VcpuSeconds: 2400, MemMIBSeconds: 1843200, CPUSeconds: 90,
		},
	}
}

func decodeUsage(t *testing.T, body []byte) usageResponse {
	t.Helper()
	var out usageResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return out
}

// The v1 shape must separate what is billed from what is merely recorded, and
// say what the window selected — a total whose basis is invisible is a number
// nobody can check an invoice against.
func TestUsageSeparatesBilledFromRecorded(t *testing.T) {
	h := testHandler(t, usageLegacy(t, "/usage", sampleReport(), http.StatusOK))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/usage", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	got := decodeUsage(t, w.Body.Bytes())
	if len(got.Intervals) != 2 {
		t.Fatalf("intervals=%d, want 2", len(got.Intervals))
	}
	closed := got.Intervals[0]
	if closed.State != "closed" || closed.EndedAt == nil {
		t.Fatalf("closed interval reported as %+v", closed)
	}
	if closed.Resources.VCPU != 2 || closed.Resources.MemoryMIB != 1024 {
		t.Fatalf("resources not surfaced: %+v", closed.Resources)
	}
	if closed.DurationSeconds != 600 || closed.VcpuSeconds != 1200 || closed.MemoryMIBSeconds != 614400 {
		t.Fatalf("billed quantities wrong: %+v", closed)
	}
	if closed.CPUSeconds != 90 {
		t.Fatalf("consumed cpu wrong: %v", closed.CPUSeconds)
	}
	if got.Intervals[1].State != "open" || got.Intervals[1].EndedAt != nil {
		t.Fatalf("open interval reported as %+v", got.Intervals[1])
	}
	// An open interval is measured to its last heartbeat, never to now.
	if got.Intervals[1].DurationSeconds != 300 {
		t.Fatalf("open interval measured to %v seconds, want 300 (its last heartbeat)", got.Intervals[1].DurationSeconds)
	}
	if got.Window.Selection != "overlap" {
		t.Fatalf("window selection not stated: %+v", got.Window)
	}
	if got.Coverage.Scope != "live_hosts" || len(got.Coverage.Hosts) != 1 || got.Coverage.Hosts[0] != "worker-1" {
		t.Fatalf("coverage caveat missing or wrong: %+v", got.Coverage)
	}
	if got.Totals.VcpuSeconds != 2400 || got.Totals.OpenIntervals != 1 {
		t.Fatalf("totals not passed through: %+v", got.Totals)
	}
}

// Paging must not shrink the totals: the first page of a busy fleet still has
// to say what the whole window costs.
func TestUsagePaginatesRowsWithoutShrinkingTotals(t *testing.T) {
	h := testHandler(t, usageLegacy(t, "/usage", sampleReport(), http.StatusOK))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/usage?page_size=1", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got := decodeUsage(t, w.Body.Bytes())
	if len(got.Intervals) != 1 || got.NextPageToken == "" {
		t.Fatalf("page_size ignored: %d intervals, next=%q", len(got.Intervals), got.NextPageToken)
	}
	if got.Totals.Intervals != 2 || got.Totals.VcpuSeconds != 2400 {
		t.Fatalf("totals shrank with the page: %+v", got.Totals)
	}
}

func TestSandboxUsageRoutesByIDAndForwardsTheWindow(t *testing.T) {
	from := "2026-08-01T00:00:00Z"
	legacy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sandboxes/sb1/usage" {
			t.Errorf("path=%q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("from") != from {
			t.Errorf("window not forwarded: %q", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(sampleReport())
	})
	h := testHandler(t, legacy)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb1/usage?from="+from, nil))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// A destroyed sandbox no longer routes, and its usage is the usage most likely
// to be asked for. The 404 has to point at the endpoint that can still answer,
// or a caller concludes the billing data is gone.
func TestSandboxUsage404PointsAtTheFleetEndpoint(t *testing.T) {
	h := testHandler(t, usageLegacy(t, "/sandboxes/sb1/usage", registry.UsageReport{}, http.StatusNotFound))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb1/usage", nil))
	if w.Code != 404 {
		t.Fatalf("status=%d, want 404", w.Code)
	}
	var problem httpapiProblem
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v (%s)", err, w.Body.String())
	}
	if !strings.Contains(problem.Detail, "/v1/usage?sandbox_id=") {
		t.Fatalf("404 does not point at the fleet-wide ledger: %q", problem.Detail)
	}
}

func TestUsageRejectsMalformedWindow(t *testing.T) {
	h := testHandler(t, usageLegacy(t, "/usage", sampleReport(), http.StatusOK))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/usage?from=yesterday", nil))
	if w.Code != 400 {
		t.Fatalf("status=%d, want 400 (body %s)", w.Code, w.Body.String())
	}
}

type httpapiProblem struct {
	Detail string `json:"detail"`
}
