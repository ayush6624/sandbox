package server

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPhaseRecorderFirstWriteWins(t *testing.T) {
	p := newPhaseRecorder()
	first := time.UnixMilli(1_700_000_000_000)
	p.markAt("boot", first)
	p.markAt("boot", first.Add(30*time.Second))

	got := p.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 sample, got %d", len(got))
	}
	if !got[0].At.Equal(first) {
		// A re-mark must never move a boot milestone: the heartbeat loop calls
		// mark every 5s forever.
		t.Fatalf("phase moved: want %v, got %v", first, got[0].At)
	}
}

func TestPhaseRecorderOrdersAndOffsets(t *testing.T) {
	p := newPhaseRecorder()
	base := time.UnixMilli(1_700_000_000_000)
	// Marked out of order on purpose.
	p.markAt("capacity_advertised", base.Add(42*time.Second))
	p.markAt("kernel_boot", base)
	p.markAt("serve_process_start", base.Add(9*time.Second+500*time.Millisecond))

	got := p.snapshot()
	wantNames := []string{"kernel_boot", "serve_process_start", "capacity_advertised"}
	if len(got) != len(wantNames) {
		t.Fatalf("want %d samples, got %d", len(wantNames), len(got))
	}
	for i, want := range wantNames {
		if got[i].Name != want {
			t.Errorf("sample %d: want %s, got %s", i, want, got[i].Name)
		}
	}
	if got[0].Offset != 0 {
		t.Errorf("anchor offset: want 0, got %v", got[0].Offset)
	}
	if got[1].Offset != 9500*time.Millisecond {
		t.Errorf("offset 1: want 9.5s, got %v", got[1].Offset)
	}
	if got[2].Offset != 42*time.Second {
		t.Errorf("offset 2: want 42s, got %v", got[2].Offset)
	}
}

func TestPhaseRecorderEmpty(t *testing.T) {
	if got := newPhaseRecorder().snapshot(); got != nil {
		t.Fatalf("empty recorder: want nil, got %v", got)
	}
	// A nil recorder must be safe: metrics are scraped on paths that may run
	// before/without initialization.
	var nilRec *phaseRecorder
	nilRec.mark("x")
	if got := nilRec.snapshot(); got != nil {
		t.Fatalf("nil recorder: want nil, got %v", got)
	}
}

func TestLoadPhaseFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "boot-phases")
	// Tab-separated (what the scripts write), space-separated (tolerated), and
	// three malformed lines that must not abort the parse.
	body := strings.Join([]string{
		"startup_script_entered\t1700000000000",
		"data_disk_ready 1700000004000",
		"garbage-with-no-timestamp",
		"bad_ts\tnot-a-number",
		"zero_ts\t0",
		"nomad_started\t1700000009000",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	p := newPhaseRecorder()
	p.loadPhaseFile(path)
	got := p.snapshot()

	if len(got) != 3 {
		t.Fatalf("want 3 valid phases, got %d: %+v", len(got), got)
	}
	if got[0].Name != "startup_script_entered" || got[2].Name != "nomad_started" {
		t.Errorf("unexpected order: %+v", got)
	}
	if got[2].Offset != 9*time.Second {
		t.Errorf("last offset: want 9s, got %v", got[2].Offset)
	}
}

func TestLoadPhaseFileMissingIsNoop(t *testing.T) {
	p := newPhaseRecorder()
	p.loadPhaseFile(filepath.Join(t.TempDir(), "does-not-exist"))
	if got := p.snapshot(); got != nil {
		t.Fatalf("missing file must be a no-op, got %v", got)
	}
}

func TestInitBootPhasesReadsEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "boot-phases")
	// Exactly what startup-worker.sh's phase() emits.
	if err := os.WriteFile(path, []byte("startup_script_entered\t1700000000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(phaseFileEnv, path)

	s := &Server{phases: newPhaseRecorder(), startedAt: time.UnixMilli(1_700_000_020_000)}
	s.initBootPhases()

	got := s.phases.snapshot()
	byName := map[string]time.Duration{}
	for _, p := range got {
		byName[p.Name] = p.Offset
	}
	if _, ok := byName["startup_script_entered"]; !ok {
		t.Fatalf("script phase not loaded: %+v", got)
	}
	if _, ok := byName[phaseServeStart]; !ok {
		t.Fatalf("serve start not marked: %+v", got)
	}
	// On Linux the kernel_boot anchor precedes the script phase, so the offsets
	// shift; assert the invariant that holds everywhere instead: serve start is
	// 20s after the script phase.
	if d := byName[phaseServeStart] - byName["startup_script_entered"]; d != 20*time.Second {
		t.Errorf("serve start relative to script entry: want 20s, got %v", d)
	}
}

func TestWriteBootPhaseMetrics(t *testing.T) {
	s := &Server{phases: newPhaseRecorder()}
	base := time.UnixMilli(1_700_000_000_000)
	s.phases.markAt(phaseKernelBoot, base)
	s.phases.markAt(phaseServeStart, base.Add(12*time.Second))
	s.phases.markAt(phaseCapacityAdv, base.Add(38*time.Second+250*time.Millisecond))

	var b strings.Builder
	s.writeBootPhaseMetrics(&b)
	out := b.String()

	for _, want := range []string{
		`sandbox_boot_phase_seconds{phase="kernel_boot"} 0.000`,
		`sandbox_boot_phase_seconds{phase="serve_process_start"} 12.000`,
		`sandbox_boot_phase_seconds{phase="capacity_advertised"} 38.250`,
		`sandbox_boot_phase_timestamp_seconds{phase="kernel_boot"} 1700000000.000`,
		"sandbox_worker_ready_seconds 38.250",
		"# TYPE sandbox_boot_phase_seconds gauge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestWriteBootPhaseMetricsNoReadyBeforeCapacity(t *testing.T) {
	// A host that has booted but never advertised capacity must NOT export
	// sandbox_worker_ready_seconds — otherwise it reads as a suspiciously fast
	// ready time (or a 0) on the dashboard.
	s := &Server{phases: newPhaseRecorder()}
	s.phases.markAt(phaseKernelBoot, time.UnixMilli(1_700_000_000_000))
	s.phases.markAt(phaseServeStart, time.UnixMilli(1_700_000_012_000))

	var b strings.Builder
	s.writeBootPhaseMetrics(&b)
	if strings.Contains(b.String(), "sandbox_worker_ready_seconds") {
		t.Errorf("ready gauge must be absent until capacity_advertised:\n%s", b.String())
	}
}

// TestHandleMetricsIncludesBootPhases drives the real /metrics endpoint against
// a real registry, so the boot timeline is proven to reach the scrape output the
// gateway federates (and not just the builder in isolation).
func TestHandleMetricsIncludesBootPhases(t *testing.T) {
	s := metricsTestServer(t)
	base := time.UnixMilli(1_700_000_000_000)
	s.phases.markAt(phaseKernelBoot, base)
	s.phases.markAt("startup_script_entered", base.Add(8*time.Second))
	s.phases.markAt(phaseCapacityAdv, base.Add(41*time.Second))

	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 {
		t.Fatalf("metrics: %d (%s)", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{
		`sandbox_boot_phase_seconds{phase="startup_script_entered"} 8.000`,
		`sandbox_boot_phase_seconds{phase="capacity_advertised"} 41.000`,
		"sandbox_worker_ready_seconds 41.000",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in /metrics output", want)
		}
	}
	// The pre-existing integer series must still parse alongside the new floats.
	m := parseMetrics(t, body)
	if _, ok := m["sandbox_slots_free"]; !ok {
		t.Error("sandbox_slots_free missing after adding float families")
	}
}

func TestWriteBootPhaseMetricsEmptyWritesNothing(t *testing.T) {
	s := &Server{phases: newPhaseRecorder()}
	var b strings.Builder
	s.writeBootPhaseMetrics(&b)
	if b.Len() != 0 {
		t.Errorf("want no output for an empty recorder, got:\n%s", b.String())
	}
}
