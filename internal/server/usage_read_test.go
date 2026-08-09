package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

func decodeUsageReport(t *testing.T, body []byte) registry.UsageReport {
	t.Helper()
	var report registry.UsageReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatalf("decode usage report: %v (%s)", err, body)
	}
	return report
}

// The ledger outlives the sandbox: Destroy deletes the sandbox row, and the
// usage it produced is exactly what a bill is made of. A handler that resolved
// the id first would answer 404 for every sandbox worth asking about.
func TestSandboxUsageAnswersForADestroyedSandbox(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()
	s.meterStart(ctx, registry.Sandbox{ID: "sb1", VMID: "vm-1"})
	s.meterStop(ctx, "sb1", registry.EndDestroy)

	// No sandbox row was ever created — the ledger is the only trace, which is
	// the state a destroyed sandbox leaves behind.
	req := httptest.NewRequest(http.MethodGet, "/sandboxes/sb1/usage", nil)
	req.SetPathValue("id", "sb1")
	w := httptest.NewRecorder()
	s.handleSandboxUsage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	report := decodeUsageReport(t, w.Body.Bytes())
	if len(report.Intervals) != 1 || report.Intervals[0].SandboxID != "sb1" {
		t.Fatalf("want the destroyed sandbox's interval, got %+v", report.Intervals)
	}
	if report.Totals.Intervals != 1 || report.Totals.OpenIntervals != 0 {
		t.Fatalf("totals wrong: %+v", report.Totals)
	}
	if report.HostID != "host-test" {
		t.Fatalf("report does not name its host: %+v", report)
	}
	if report.Intervals[0].EndReason != registry.EndDestroy {
		t.Fatalf("end reason lost: %+v", report.Intervals[0])
	}
}

// Effective resources are what gets billed, so the read path must surface the
// resolved values rather than the registry's "0 = template default".
func TestUsageReportsEffectiveResources(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()
	s.meterStart(ctx, registry.Sandbox{ID: "default-res", VMID: "vm-1"})
	s.meterStart(ctx, registry.Sandbox{ID: "override-res", VMID: "vm-2", Vcpus: 4, MemMIB: 4096})

	w := httptest.NewRecorder()
	s.handleUsage(w, httptest.NewRequest(http.MethodGet, "/usage", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	byID := map[string]registry.UsageInterval{}
	for _, iv := range decodeUsageReport(t, w.Body.Bytes()).Intervals {
		byID[iv.SandboxID] = iv
	}
	if got := byID["default-res"]; got.Vcpus != 2 || got.MemMIB != 1024 {
		t.Fatalf("template defaults not reported: %+v", got)
	}
	if got := byID["override-res"]; got.Vcpus != 4 || got.MemMIB != 4096 {
		t.Fatalf("override not reported: %+v", got)
	}
}

// A dropped filter silently changes what the number means, so a malformed
// window is rejected rather than ignored.
func TestUsageRejectsMalformedWindow(t *testing.T) {
	s := testMeteringServer(t)
	for _, query := range []string{
		"?from=yesterday",
		"?to=not-a-time",
		"?from=2026-08-02T00:00:00Z&to=2026-08-01T00:00:00Z",
		"?limit=0",
		"?limit=99999",
	} {
		w := httptest.NewRecorder()
		s.handleUsage(w, httptest.NewRequest(http.MethodGet, "/usage"+query, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d, want 400 (body %s)", query, w.Code, w.Body.String())
		}
	}
}

// A window that excludes everything is a legitimate answer, not an error, and
// it must report zero rather than the unfiltered ledger.
func TestUsageWindowFiltersTheLedger(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()
	s.meterStart(ctx, registry.Sandbox{ID: "sb1", VMID: "vm-1"})
	s.meterStop(ctx, "sb1", registry.EndDestroy)

	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	later := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	w := httptest.NewRecorder()
	s.handleUsage(w, httptest.NewRequest(http.MethodGet, "/usage?from="+future+"&to="+later, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	report := decodeUsageReport(t, w.Body.Bytes())
	if len(report.Intervals) != 0 || report.Totals.Intervals != 0 {
		t.Fatalf("future window matched past usage: %+v", report)
	}
	if report.From == nil || report.To == nil {
		t.Fatalf("report does not echo the window it answered for: %+v", report)
	}
}

// The counters are credited at close, from the closed row. Crediting on open,
// or crediting twice for a duplicate close, would both bill a VM wrongly.
func TestBillableCountersCreditOnceAtClose(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()

	s.meterStart(ctx, registry.Sandbox{ID: "sb1", VMID: "vm-1", Vcpus: 4, MemMIB: 2048})
	if got := s.met.billableVcpuSeconds.Load(); got != 0 {
		t.Fatalf("an open interval credited %d vcpu-seconds; nothing is owed until it closes", got)
	}

	// Interval timestamps have one-second resolution, so give the interval a
	// span to credit. The expectation is derived from the row the ledger
	// actually recorded rather than from the sleep, which keeps the assertion
	// exact instead of racing the second boundary.
	time.Sleep(1100 * time.Millisecond)
	s.meterStop(ctx, "sb1", registry.EndDestroy)

	rows, err := s.reg.UsageForSandbox(ctx, "sb1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("read back interval: %v (%d rows)", err, len(rows))
	}
	seconds := int64(rows[0].Duration() / time.Second)
	if seconds < 1 {
		t.Fatalf("interval has no billable span to credit: %+v", rows[0])
	}
	vcpu, mem := s.met.billableVcpuSeconds.Load(), s.met.billableMemMIBSeconds.Load()
	if vcpu != 4*seconds || mem != 2048*seconds {
		t.Fatalf("credited vcpu=%d mem=%d for %ds, want %d and %d", vcpu, mem, seconds, 4*seconds, 2048*seconds)
	}

	// Several teardown paths call meterStop for one sandbox and can race.
	s.meterStop(ctx, "sb1", registry.EndDestroy)
	if s.met.billableVcpuSeconds.Load() != vcpu || s.met.billableMemMIBSeconds.Load() != mem {
		t.Fatalf("a duplicate close credited the counters again: vcpu=%d mem=%d",
			s.met.billableVcpuSeconds.Load(), s.met.billableMemMIBSeconds.Load())
	}
}
