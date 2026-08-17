package prometheus_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ruleExpr returns the expression of a named recording rule.
func ruleExpr(t *testing.T, record string) string {
	t.Helper()
	body, err := os.ReadFile("rules.yml.tpl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "- record: "+record {
			continue
		}
		for _, next := range lines[i+1:] {
			if trimmed := strings.TrimSpace(next); strings.HasPrefix(trimmed, "expr:") {
				return strings.TrimSpace(strings.TrimPrefix(trimmed, "expr:"))
			}
		}
	}
	t.Fatalf("recording rule %q not found", record)
	return ""
}

func TestWorkersDesiredUsesGatewayOwnedMIGTarget(t *testing.T) {
	expr := ruleExpr(t, "sandbox:workers_desired")
	if !strings.Contains(expr, `sandbox_mig_target_size{job="sandbox-gateway"}`) {
		t.Errorf("workers_desired must use the gateway-observed MIG target:\n%s", expr)
	}
	for _, stale := range []string{"sandbox_slots_committed", "sandbox_create_queue_depth", "sandbox_hibernated"} {
		if strings.Contains(expr, stale) {
			t.Errorf("workers_desired must not reconstruct provider intent from %s:\n%s", stale, expr)
		}
	}
}

func TestScrapeLoopFitsHeldBurstLatencyBudget(t *testing.T) {
	prometheusConfig, err := os.ReadFile("prometheus.yml.tpl")
	if err != nil {
		t.Fatal(err)
	}
	config := string(prometheusConfig)
	for _, want := range []string{"scrape_interval: 5s", "evaluation_interval: 5s"} {
		if !strings.Contains(config, want) {
			t.Errorf("Prometheus control loop missing %q", want)
		}
	}
}

// The gateway is the ONLY MIG writer, in both directions. Two controllers
// sizing one group is not a tuning problem: on 2026-08-16 they disagreed by one
// host and the shrinking one (the Nomad autoscaler) deleted the growing one's
// work mid-burst, with live sandboxes on it. It was also reading nothing at all
// — count.original:0 with an empty reason_history — which made it a constant
// "drive to MIG_MIN" loop.
//
// So: no autoscaler policy may come back, and this rule must not become a
// control input again.
func TestNoSecondMIGWriter(t *testing.T) {
	for _, gone := range []string{
		"../nomad/policies/workers.hcl.tpl",
		"../nomad/autoscaler.hcl.tpl",
	} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s is back: the Nomad autoscaler is a second MIG writer", gone)
		}
	}

	install, err := os.ReadFile("../control-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	body := string(install)
	// The gateway must carry both bounds; --direct-scale-min is also what
	// enables scale-in at all.
	for _, want := range []string{"--direct-scale-min", "--direct-scale-max"} {
		if !strings.Contains(body, want) {
			t.Errorf("gateway unit missing %q", want)
		}
	}
	// Removal of the old unit must stay, for hosts upgrading from that layout.
	if !strings.Contains(body, "systemctl disable --now nomad-autoscaler") {
		t.Error("control-install.sh no longer removes a previously installed nomad-autoscaler")
	}
	if strings.Contains(body, "ExecStart=/usr/local/bin/nomad-autoscaler") {
		t.Error("control-install.sh still installs the nomad-autoscaler service")
	}
}

// Structural assertions cannot catch an expression that parses and then
// silently evaluates to a constant, which is how this signal broke twice.
// promtool evaluates the real rules against synthetic series.
func TestRulesEvaluateInPrometheus(t *testing.T) {
	promtool, err := exec.LookPath("promtool")
	if err != nil {
		t.Skip("promtool not installed; skipping PromQL evaluation")
	}
	tpl, err := os.ReadFile("rules.yml.tpl")
	if err != nil {
		t.Fatal(err)
	}
	rendered := strings.NewReplacer(
		"${LEAD_SECONDS}", "90",
		"${HEADROOM_SLOTS}", "48",
		"${SLOTS_PER_HOST}", "48",
		"${RAW_PORT_CAPACITY}", "1000",
	).Replace(string(tpl))
	if strings.Contains(rendered, "${") {
		t.Fatal("rules template has an unsubstituted variable; add it to the replacer")
	}

	cases, err := os.ReadFile("promtool_cases.yml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, content := range map[string][]byte{"rules.yml": []byte(rendered), "cases.yml": cases} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command(promtool, "test", "rules", filepath.Join(dir, "cases.yml")).CombinedOutput()
	if err != nil {
		t.Fatalf("promtool test rules failed: %v\n%s", err, out)
	}
}
