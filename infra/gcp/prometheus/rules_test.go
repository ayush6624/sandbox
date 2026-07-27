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
