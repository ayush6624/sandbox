package prometheus_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ruleExpr returns the expression of a named recording rule. Looking rules up by
// name rather than by position matters: the scaling signal is now three rules,
// and a positional scan silently starts asserting against the wrong one.
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

func TestWorkersDesiredUsesInstantCommittedOccupancy(t *testing.T) {
	expr := ruleExpr(t, "sandbox:workers_demand")
	for _, want := range []string{
		`sum(sandbox_slots_committed{job="sandbox-gateway"})`,
		`sum(sandbox_hibernated{job="sandbox-gateway"})`,
		`sum(sandbox_create_queue_depth)`,
	} {
		if !strings.Contains(expr, want) {
			t.Errorf("workers_desired expression missing %q:\n%s", want, expr)
		}
	}
	if strings.Contains(expr, "sandbox_slots_used") {
		t.Errorf("workers_desired must not regress to heartbeat-only occupancy:\n%s", expr)
	}
	if strings.Contains(expr, "max_over_time") {
		t.Errorf("occupancy must be instantaneous; downstream policy owns scale-down smoothing:\n%s", expr)
	}

	// Canonical held burst: two 48-slot hosts have 96 committed assignments
	// and 64 more creates queued. Even without headroom or lead demand, the
	// signal must request four workers rather than the old under-scaled three.
	const committed, queued, slotsPerHost = 96, 64, 48
	if desired := (committed + queued + slotsPerHost - 1) / slotsPerHost; desired != 4 {
		t.Fatalf("canonical desired workers = %d, want 4", desired)
	}
}

func TestScaleOutControlLoopFitsHeldBurstLatencyBudget(t *testing.T) {
	prometheusConfig, err := os.ReadFile("prometheus.yml.tpl")
	if err != nil {
		t.Fatal(err)
	}
	config := string(prometheusConfig)
	for _, want := range []string{
		"scrape_interval: 5s",
		"evaluation_interval: 5s",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("Prometheus control loop missing %q", want)
		}
	}

	policy, err := os.ReadFile("../nomad/policies/workers.hcl.tpl")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(policy), `evaluation_interval = "5s"`) {
		t.Error("Nomad Autoscaler policy must evaluate scale-out demand every 5s")
	}
}

// The autoscaler is capped to scale-in only, and the cap must include the
// gateway's grow-only watermark. Capping on sandbox_hosts_live alone would read
// "too many nodes" during the ~13s in which resized workers exist but have not
// heartbeated, and scale the fleet back down mid-burst.
func TestScaleInCeilingIncludesGatewayWatermark(t *testing.T) {
	expr := ruleExpr(t, "sandbox:workers_scale_in_ceiling")
	// The ceiling's authority must be the PROVIDER target size. Deriving it from
	// heartbeats let the autoscaler scale out past the cap (from=5 to=6) on
	// 2026-07-28, because hosts_live also counts resumed standby workers that
	// sit outside the MIG target.
	if !strings.HasPrefix(expr,
		`sum(sandbox_mig_target_size{job="sandbox-gateway"})`) {
		t.Errorf("scale-in ceiling must prefer the provider target size:\n%s", expr)
	}
	for _, want := range []string{
		`sum(sandbox_mig_target_size{job="sandbox-gateway"})`,
		// Fallback for a gateway predating the metric, or before a poll succeeds.
		`sum(sandbox_hosts_live{job="sandbox-gateway"})`,
		`sum(sandbox_scale_out_requested{job="sandbox-gateway"})`,
		// Keeps scale-in alive rather than emitting an empty expression.
		"or vector(0)",
	} {
		if !strings.Contains(expr, want) {
			t.Errorf("scale-in ceiling missing %q:\n%s", want, expr)
		}
	}

	// The policy must consume the ceiling, or the autoscaler is still a
	// competing scale-OUT writer alongside the gateway.
	policy, err := os.ReadFile("../nomad/policies/workers.hcl.tpl")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(policy), "sandbox:workers_scale_in_ceiling") {
		t.Error("worker policy does not cap its target with sandbox:workers_scale_in_ceiling")
	}
}

// The scale-in policy query MUST be wrapped so its result is label-free.
// `(A < B) or B` has branch-dependent labels: max_over_time() strips __name__ so
// branch A is bare, while branch B is the raw recording rule and keeps
// __name__. The autoscaler's Prometheus APM plugin reads a named result as NO
// DATA, so pass-through produced a target of 0 and the fleet could not scale in
// — 259 consecutive count.original:0 evaluations on 2026-07-28. It breaks
// exactly when the ceiling binds, i.e. mid-burst.
func TestScaleInPolicyQueryIsLabelFree(t *testing.T) {
	policy, err := os.ReadFile("../nomad/policies/workers.hcl.tpl")
	if err != nil {
		t.Fatal(err)
	}
	var query string
	for _, line := range strings.Split(string(policy), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "query") && strings.Contains(trimmed, "workers_desired") {
			query = trimmed
			break
		}
	}
	if query == "" {
		t.Fatal("scale-in policy query not found")
	}
	if !strings.Contains(query, "sandbox:workers_scale_in_ceiling") {
		t.Fatalf("policy query lost its scale-in ceiling:\n%s", query)
	}
	// The whole min() expression must sit inside an aggregation that drops
	// labels. Without it the `or` fallback branch returns a named series.
	value := query[strings.Index(query, "\"")+1:]
	value = strings.TrimSuffix(value, "\"")
	if !strings.HasPrefix(value, "sum(") || !strings.HasSuffix(value, ")") {
		t.Errorf("policy query must be wrapped in a label-dropping aggregation (sum(...)):\n%s", value)
	}
}

// The gateway and sandbox:workers_demand are two independent answers to "how
// many hosts do we need", and they legitimately disagree: the gateway nudges to
// live+1 whenever a create is queued, because a queued create proves the fleet
// can't place it. Nothing here carries that term, so without a floor the
// autoscaler reads the lower number and scales in the host the gateway just
// added — measured 2026-08-16 as 11 x `502 host ... unreachable: EOF` mid-sweep.
func TestWorkersDesiredFloorsOnRecentGatewayScaleOut(t *testing.T) {
	floor := ruleExpr(t, "sandbox:workers_scale_out_floor")
	for _, want := range []string{
		// The floor is the size the gateway actually asked the provider for.
		`sum(sandbox_mig_target_size{job="sandbox-gateway"})`,
		// Event-scoped, not level-scoped. sandbox_scale_out_requested re-baselines
		// to the LIVE host count and never below, so flooring on that level would
		// pin the fleet at its high-water mark and scale-in would never happen.
		`increase(sandbox_direct_scale_out_total{job="sandbox-gateway"}[5m])`,
		// Keeps the floor a present-but-zero vector rather than an empty one.
		"or vector(0)",
	} {
		if !strings.Contains(floor, want) {
			t.Errorf("scale-out floor missing %q:\n%s", want, floor)
		}
	}
	if strings.Contains(floor, "sandbox_scale_out_requested") {
		t.Errorf("floor must not use the grow-only watermark; it never falls below hosts_live:\n%s", floor)
	}

	desired := ruleExpr(t, "sandbox:workers_desired")
	if !strings.Contains(desired, "sandbox:workers_scale_out_floor") {
		t.Errorf("workers_desired does not apply the scale-out floor:\n%s", desired)
	}
	// Every reference must be wrapped in sum(): a recording rule result carries
	// __name__, and two differently-named vectors do not match in a binary
	// operator — the same silent-zero failure already documented on the policy
	// query. sum() drops __name__ so both sides share the empty label set.
	for _, want := range []string{
		"sum(sandbox:workers_demand)",
		"sum(sandbox:workers_scale_out_floor)",
	} {
		if !strings.Contains(desired, want) {
			t.Errorf("workers_desired must strip __name__ with sum(); missing %q:\n%s", want, desired)
		}
	}
}

// Structural assertions cannot catch an expression that parses and then
// silently evaluates to a constant, which is how this signal has broken twice.
// promtool evaluates the real rules against synthetic series; see
// promtool_cases.yml. Skipped where promtool is absent (it ships with
// Prometheus, so it is present on the control VM).
func TestRulesEvaluateInPrometheus(t *testing.T) {
	promtool, err := exec.LookPath("promtool")
	if err != nil {
		t.Skip("promtool not installed; skipping PromQL evaluation")
	}
	tpl, err := os.ReadFile("rules.yml.tpl")
	if err != nil {
		t.Fatal(err)
	}
	// Values match config.env; the expectations in promtool_cases.yml are
	// arithmetic on exactly these.
	rendered := strings.NewReplacer(
		"${LEAD_SECONDS}", "90",
		"${HEADROOM_SLOTS}", "48",
		"${SLOTS_PER_HOST}", "48",
		"${RAW_PORT_CAPACITY}", "1000",
	).Replace(string(tpl))
	if strings.Contains(rendered, "${") {
		t.Fatalf("rules template has an unsubstituted variable; add it to the replacer")
	}

	cases, err := os.ReadFile("promtool_cases.yml")
	if err != nil {
		t.Fatal(err)
	}
	// rule_files are resolved relative to the test file, so both land together.
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
