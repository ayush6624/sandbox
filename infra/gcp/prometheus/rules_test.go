package prometheus_test

import (
	"os"
	"strings"
	"testing"
)

func TestWorkersDesiredUsesInstantCommittedOccupancy(t *testing.T) {
	body, err := os.ReadFile("rules.yml.tpl")
	if err != nil {
		t.Fatal(err)
	}
	var expr string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "expr:") {
			expr = line
			break
		}
	}
	if expr == "" {
		t.Fatal("workers_desired expression not found")
	}
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
	rules, err := os.ReadFile("rules.yml.tpl")
	if err != nil {
		t.Fatal(err)
	}
	body := string(rules)
	if !strings.Contains(body, "record: sandbox:workers_scale_in_ceiling") {
		t.Fatal("sandbox:workers_scale_in_ceiling recording rule is missing")
	}

	var expr string
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "record: sandbox:workers_scale_in_ceiling") {
			continue
		}
		for _, next := range lines[i+1:] {
			if trimmed := strings.TrimSpace(next); strings.HasPrefix(trimmed, "expr:") {
				expr = trimmed
				break
			}
		}
		break
	}
	if expr == "" {
		t.Fatal("scale-in ceiling expression not found")
	}
	// The ceiling's authority must be the PROVIDER target size. Deriving it from
	// heartbeats let the autoscaler scale out past the cap (from=5 to=6) on
	// 2026-07-28, because hosts_live also counts resumed standby workers that
	// sit outside the MIG target.
	if !strings.HasPrefix(strings.TrimPrefix(expr, "expr: "),
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
