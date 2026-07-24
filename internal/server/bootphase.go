package server

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Boot-phase instrumentation for the autoscale critical path.
//
// Scaling a worker in is ~10s of control-loop decision plus a much larger
// "make this host usable" span (measured ~26s on the stopped-standby path),
// which until now was a single opaque block in the profile: GCE boot, startup
// script, data-disk setup, Nomad join, artifact pull, serve start, golden
// adoption and the first capacity-advertising heartbeat all landed inside it.
// This file stamps each of those boundaries so the span can be attributed.
//
// The phases come from three writers that can't share memory:
//
//   - startup-worker.sh (GCE startup script, pre-Nomad) and the Nomad task's
//     run.sh append "<phase>\t<epoch_ms>" lines to PhaseFilePath.
//   - serve marks its own phases in-process (below).
//   - kernel_boot is read from /proc/stat btime, standing in for "instance
//     RUNNING" without needing a metadata round-trip.
//
// Everything is exported as an ABSOLUTE TIMESTAMP (and an offset from the
// earliest phase), not as a rate or a duration sampled over time. That matters:
// a 10s Prometheus scrape still recovers millisecond-accurate boundaries,
// because the value doesn't decay between scrapes. Profiling a scale-up needs no
// special scrape interval — the normal fleet scrape is enough.
//
// Lifetime note: PhaseFilePath lives on tmpfs (/run), so a STOPPED standby
// worker that boots gets a clean timeline, while a SUSPENDED worker that resumes
// keeps the file from its original boot. That's the honest reading — a resumed
// worker never re-runs the startup script and never restarts serve, so its
// readiness path is genuinely a different (and much shorter) one.
const (
	// PhaseFilePath is the fixed host path the boot scripts append to. Fixed
	// rather than configured because startup-worker.sh runs before any config
	// is read; override with SANDBOX_BOOT_PHASES for tests or a non-fleet host.
	PhaseFilePath = "/run/sandbox/boot-phases"

	// phaseFileEnv overrides PhaseFilePath.
	phaseFileEnv = "SANDBOX_BOOT_PHASES"
)

// Phase names recorded by serve itself. The script-side names
// (startup_script_entered, data_disk_ready, rootfs_staged, nomad_started,
// startup_script_done, serve_task_started) are string literals in
// startup-worker.sh and serve.nomad.hcl — keep them in sync with the docs.
const (
	phaseKernelBoot     = "kernel_boot"
	phaseServeStart     = "serve_process_start"
	phaseReconcileDone  = "reconcile_done"
	phaseGoldenSettled  = "golden_settled"
	phaseFirstHeartbeat = "first_heartbeat_ok"
	phaseCapacityAdv    = "capacity_advertised"
)

// phaseRecorder holds one timestamp per named phase. First write wins: these
// are boot milestones, so a retry (e.g. the heartbeat loop succeeding again)
// must not overwrite the first occurrence.
type phaseRecorder struct {
	mu sync.Mutex
	at map[string]time.Time
}

func newPhaseRecorder() *phaseRecorder {
	return &phaseRecorder{at: map[string]time.Time{}}
}

// mark stamps a phase with the current time, if not already stamped.
func (p *phaseRecorder) mark(name string) { p.markAt(name, time.Now()) }

// markAt stamps a phase with an explicit time, if not already stamped.
func (p *phaseRecorder) markAt(name string, t time.Time) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, seen := p.at[name]; !seen {
		p.at[name] = t
	}
}

// phaseSample is one phase's absolute time plus its offset from the earliest
// recorded phase.
type phaseSample struct {
	Name   string
	At     time.Time
	Offset time.Duration
}

// snapshot returns the recorded phases ordered by time, each carrying its offset
// from the earliest one. An empty recorder yields nil.
func (p *phaseRecorder) snapshot() []phaseSample {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.at) == 0 {
		return nil
	}
	out := make([]phaseSample, 0, len(p.at))
	for name, at := range p.at {
		out = append(out, phaseSample{Name: name, At: at})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].Name < out[j].Name
		}
		return out[i].At.Before(out[j].At)
	})
	base := out[0].At
	for i := range out {
		out[i].Offset = out[i].At.Sub(base)
	}
	return out
}

// loadPhaseFile folds the boot scripts' "<phase>\t<epoch_ms>" lines into the
// recorder. A missing file is normal (dev laptop, non-fleet host) and silently
// ignored; malformed lines are skipped rather than failing startup, since this
// is diagnostics and must never keep a worker from serving.
func (p *phaseRecorder) loadPhaseFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, msText, ok := strings.Cut(strings.TrimSpace(sc.Text()), "\t")
		if !ok {
			// Tolerate space-separated too — easy to get wrong from shell.
			name, msText, ok = strings.Cut(strings.TrimSpace(sc.Text()), " ")
			if !ok {
				continue
			}
		}
		name, msText = strings.TrimSpace(name), strings.TrimSpace(msText)
		if name == "" {
			continue
		}
		ms, err := strconv.ParseInt(msText, 10, 64)
		if err != nil || ms <= 0 {
			continue
		}
		p.markAt(name, time.UnixMilli(ms))
	}
}

// kernelBootTime reads btime (seconds since epoch at kernel boot) from
// /proc/stat. This is the closest cheap proxy for "GCE reported the instance
// RUNNING", and it anchors the whole timeline: every later phase is measured as
// an offset from it. Returns zero time when unavailable (non-Linux, or a
// kernel without btime), in which case the anchor becomes the earliest phase we
// did record.
func kernelBootTime() time.Time {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "btime ")
		if !ok {
			continue
		}
		secs, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil || secs <= 0 {
			return time.Time{}
		}
		return time.Unix(secs, 0)
	}
	return time.Time{}
}

// initBootPhases seeds the recorder for this process: the script-written file,
// the kernel boot anchor, and serve's own start. Called once from Serve.
func (s *Server) initBootPhases() {
	path := os.Getenv(phaseFileEnv)
	if path == "" {
		path = PhaseFilePath
	}
	s.phases.loadPhaseFile(path)
	if kb := kernelBootTime(); !kb.IsZero() {
		s.phases.markAt(phaseKernelBoot, kb)
	}
	s.phases.markAt(phaseServeStart, s.startedAt)
}

// writeBootPhaseMetrics renders the boot timeline in Prometheus exposition
// format. Two families per phase — the absolute timestamp (joinable with
// anything else in the TSDB) and the offset from the anchor (the number you
// actually read off a dashboard) — plus sandbox_worker_ready_seconds, the
// headline "kernel boot → this host advertised capacity" figure.
func (s *Server) writeBootPhaseMetrics(b *strings.Builder) {
	samples := s.phases.snapshot()
	if len(samples) == 0 {
		return
	}

	fmt.Fprintf(b, "# HELP sandbox_boot_phase_timestamp_seconds Unix time a worker boot/readiness phase completed (absolute; scrape interval does not affect precision).\n# TYPE sandbox_boot_phase_timestamp_seconds gauge\n")
	for _, s := range samples {
		fmt.Fprintf(b, "sandbox_boot_phase_timestamp_seconds{phase=%q} %.3f\n", s.Name, float64(s.At.UnixMilli())/1000)
	}

	fmt.Fprintf(b, "# HELP sandbox_boot_phase_seconds Seconds from the boot anchor (kernel_boot when available, else the earliest phase) to each phase.\n# TYPE sandbox_boot_phase_seconds gauge\n")
	for _, s := range samples {
		fmt.Fprintf(b, "sandbox_boot_phase_seconds{phase=%q} %.3f\n", s.Name, s.Offset.Seconds())
	}

	// Headline: anchor → capacity advertised. Absent until the host has actually
	// advertised free slots, so it never reads as a spuriously fast 0.
	for _, s := range samples {
		if s.Name == phaseCapacityAdv {
			fmt.Fprintf(b, "# HELP sandbox_worker_ready_seconds Seconds from the boot anchor until this host first advertised free capacity to the gateway.\n# TYPE sandbox_worker_ready_seconds gauge\n")
			fmt.Fprintf(b, "sandbox_worker_ready_seconds %.3f\n", s.Offset.Seconds())
			break
		}
	}
}
