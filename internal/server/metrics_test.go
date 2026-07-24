package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

// parseMetrics turns Prometheus text exposition output into a map keyed by the
// full series identifier (name plus any labels), so a test can assert exact
// values including labeled series like sandbox_pool_used{pool="tap"}.
func parseMetrics(t *testing.T, body string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			t.Fatalf("malformed metric line: %q", line)
		}
		v, err := strconv.ParseInt(line[i+1:], 10, 64)
		if err != nil {
			// The boot-phase families (sandbox_boot_phase_*,
			// sandbox_worker_ready_seconds) are legitimately fractional. Skip
			// well-formed floats so this integer-oriented helper stays usable,
			// but still fail on output that isn't a number at all.
			if _, ferr := strconv.ParseFloat(line[i+1:], 64); ferr == nil {
				continue
			}
			t.Fatalf("value in %q: %v", line, err)
		}
		out[line[:i]] = v
	}
	return out
}

func metricsTestServer(t *testing.T) *Server {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"), registry.Pools{
		TapPrefix:  "fc",
		TapMax:     3,
		GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.0.12",
		PortMin:    5200,
		PortMax:    5202,
	})
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })
	// MemBudgetMIB < 0 disables memory admission, so slots_free is bound purely
	// by the deterministic tap/IP pools rather than the host's real RAM.
	return New(Config{
		VMTemplate:   vm.RunOptions{Vcpus: 2, MemMIB: 1024},
		HotCreate:    false,
		MemBudgetMIB: -1,
	}, reg)
}

// TestHandleMetrics exercises the endpoint end to end against a real registry:
// two running sandboxes, one then hibernated, and asserts the pool accounting
// (tap/IP free on hibernate, explicit ports stay held) plus lifecycle counters.
func TestHandleMetrics(t *testing.T) {
	s := metricsTestServer(t)
	ctx := context.Background()

	if _, err := s.reg.Create(ctx, "sb-a", "", "/tmp/a", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, err := s.reg.Create(ctx, "sb-b", "", "/tmp/b", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create b: %v", err)
	}
	if _, err := s.reg.AddPort(ctx, "sb-b", 3000); err != nil {
		t.Fatalf("expose b: %v", err)
	}
	// Flip one to hibernated directly (no VM needed): the row bookkeeping is
	// what the metric reads. A hibernated sandbox releases its tap/IP but keeps
	// its explicitly exposed port reserved for wake-on-connect.
	if err := s.reg.Hibernate(ctx, "sb-b"); err != nil {
		t.Fatalf("hibernate b: %v", err)
	}

	// Bump lifecycle counters so the counter series are non-zero and exact.
	s.met.createsOK.Add(5)
	s.met.createsErr.Add(1)
	s.met.hibernations.Add(2)
	s.met.wakes.Add(3)
	s.met.wakeFailures.Add(1)

	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 {
		t.Fatalf("metrics: %d (%s)", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain exposition", ct)
	}

	m := parseMetrics(t, w.Body.String())
	want := map[string]int64{
		"sandbox_running":                   1,
		"sandbox_hibernated":                1,
		"sandbox_pool_used{pool=\"tap\"}":   1, // hibernated released its tap
		"sandbox_pool_used{pool=\"ip\"}":    1, // and its IP
		"sandbox_pool_used{pool=\"port\"}":  1, // explicit mapping stays held
		"sandbox_pool_total{pool=\"tap\"}":  3,
		"sandbox_pool_total{pool=\"ip\"}":   3,
		"sandbox_pool_total{pool=\"port\"}": 3,
		"sandbox_slots_free":                2, // 3 taps - 1 running (mem admission off)
		"sandbox_mem_budget_mib":            0, // disabled
		"sandbox_golden_ready":              0,
		"sandbox_creates_ok_total":          5,
		"sandbox_creates_error_total":       1,
		"sandbox_hibernations_total":        2,
		"sandbox_wakes_total":               3,
		"sandbox_wake_failures_total":       1,
	}
	for k, v := range want {
		if got, ok := m[k]; !ok {
			t.Errorf("missing series %q", k)
		} else if got != v {
			t.Errorf("%s = %d, want %d", k, got, v)
		}
	}
	// create_concurrency should reflect the semaphore capacity (defaulted).
	if m["sandbox_create_concurrency"] < 1 {
		t.Errorf("sandbox_create_concurrency = %d, want >= 1", m["sandbox_create_concurrency"])
	}
}
