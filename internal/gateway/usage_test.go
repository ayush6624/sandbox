package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

// usageHost stands up a worker that answers GET /usage with a fixed report.
func usageHost(t *testing.T, id string, report registry.UsageReport, fail bool) *host {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if r.URL.Path != "/usage" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(report)
	}))
	t.Cleanup(srv.Close)
	return &host{id: id, addr: srv.URL, token: "tok", slotsTotal: 4}
}

func closedInterval(id, sandbox, hostID string, started time.Time, seconds int64, vcpus, mem int64) registry.UsageInterval {
	ended := started.Add(time.Duration(seconds) * time.Second)
	return registry.UsageInterval{
		ID: id, SandboxID: sandbox, Seq: 1, HostID: hostID,
		Vcpus: vcpus, MemMIB: mem,
		StartedAt: started, EndedAt: &ended, LastSeenAt: ended,
		CPUUsec: seconds * 500_000, EndReason: registry.EndDestroy,
	}
}

// A host with no sandboxes is exactly the host whose ledger matters most: the
// sandboxes it billed for are the ones already destroyed. handleList skips
// empty hosts as an optimization; doing that here would drop finished usage.
func TestUsageAsksHostsWithNoSandboxes(t *testing.T) {
	started := time.Now().UTC().Add(-time.Hour)
	empty := usageHost(t, "empty", registry.UsageReport{
		HostID:    "empty",
		Intervals: []registry.UsageInterval{closedInterval("empty:sb1:1", "sb1", "empty", started, 300, 2, 1024)},
		Totals:    registry.UsageTotals{Intervals: 1, DurationSeconds: 300, VcpuSeconds: 600, MemMIBSeconds: 307200, CPUSeconds: 150},
	}, false)
	// Zero slots used, zero hibernated, no routes: invisible to handleList.
	empty.slotsUsed, empty.hibernated = 0, 0
	g := liveGateway(empty)

	w := httptest.NewRecorder()
	g.handleUsage(w, httptest.NewRequest(http.MethodGet, "/usage", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got registry.UsageReport
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Totals.Intervals != 1 || got.Totals.VcpuSeconds != 600 {
		t.Fatalf("usage from an empty host was dropped: %+v", got.Totals)
	}
}

// Totals come from each host's own SQL aggregate, so a host that truncated its
// rows still contributes its full amount. Summing the merged rows instead would
// under-report exactly the busiest hosts.
func TestUsageTotalsSurviveTruncatedHostPages(t *testing.T) {
	started := time.Now().UTC().Add(-time.Hour)
	a := usageHost(t, "a", registry.UsageReport{
		HostID:    "a",
		Intervals: []registry.UsageInterval{closedInterval("a:sb1:1", "sb1", "a", started, 60, 2, 1024)},
		Totals:    registry.UsageTotals{Intervals: 50, DurationSeconds: 3000, VcpuSeconds: 6000, MemMIBSeconds: 3072000, CPUSeconds: 1500},
		Truncated: true,
	}, false)
	b := usageHost(t, "b", registry.UsageReport{
		HostID:    "b",
		Intervals: []registry.UsageInterval{closedInterval("b:sb2:1", "sb2", "b", started.Add(time.Minute), 120, 4, 2048)},
		Totals:    registry.UsageTotals{Intervals: 1, DurationSeconds: 120, VcpuSeconds: 480, MemMIBSeconds: 245760, CPUSeconds: 60},
	}, false)

	w := httptest.NewRecorder()
	liveGateway(a, b).handleUsage(w, httptest.NewRequest(http.MethodGet, "/usage", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got registry.UsageReport
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Totals.Intervals != 51 || got.Totals.VcpuSeconds != 6480 {
		t.Fatalf("totals folded from rows instead of host totals: %+v", got.Totals)
	}
	if !got.Truncated {
		t.Fatalf("a truncated host page was not reported as truncated: %+v", got)
	}
	if len(got.Hosts) != 2 {
		t.Fatalf("response does not name the hosts it covers: %+v", got.Hosts)
	}
	// Newest first, so a dashboard's first page is the recent activity.
	if len(got.Intervals) != 2 || got.Intervals[0].SandboxID != "sb2" {
		t.Fatalf("merged intervals not newest-first: %+v", got.Intervals)
	}
}

// A partial ledger is a smaller bill, silently and in the customer's favour.
// Fail closed instead, exactly as the sandbox list does.
func TestUsageFailsClosedWhenAHostIsUnreachable(t *testing.T) {
	ok := usageHost(t, "ok", registry.UsageReport{HostID: "ok", Totals: registry.UsageTotals{Intervals: 1}}, false)
	broken := usageHost(t, "broken", registry.UsageReport{}, true)

	w := httptest.NewRecorder()
	liveGateway(ok, broken).handleUsage(w, httptest.NewRequest(http.MethodGet, "/usage", nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 (body %s)", w.Code, w.Body.String())
	}
}

// Validating before the fan-out costs one 400 instead of N host round trips
// that would each reject the same window.
func TestUsageRejectsMalformedWindowBeforeFanout(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(registry.UsageReport{HostID: "a"})
	}))
	t.Cleanup(srv.Close)

	g := liveGateway(&host{id: "a", addr: srv.URL, token: "tok", slotsTotal: 4})
	w := httptest.NewRecorder()
	g.handleUsage(w, httptest.NewRequest(http.MethodGet, "/usage?from=nope", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Code)
	}
	if hits.Load() != 0 {
		t.Fatalf("fanned out to %d host(s) despite an invalid window", hits.Load())
	}
}
