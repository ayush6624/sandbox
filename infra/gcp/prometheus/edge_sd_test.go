package prometheus_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The edge scrape job shipped broken twice in one commit (24ee226), in two ways
// with very different blast radii, and neither is caught by reading the YAML:
//
//  1. a gce_sd_config with no `zone`. `zone` is REQUIRED and single-valued, so a
//     regional MIG needs one entry per zone. Omitting it is a FATAL config parse
//     error: Prometheus exits 2, systemd crash-loops it, and the entire fleet's
//     metrics stack goes dark (it did, for three days). `promtool check config`
//     in control-install.sh now fails the deploy on this, and this test pins the
//     generator so the check is never the only thing standing between a bad
//     render and production.
//
//  2. a filter using `=` against `tags.items`. `tags` is a REPEATED field and the
//     GCE list-filter grammar only accepts the `:` "has" operator against one;
//     every `=` spelling returns 400 "Invalid list filter expression". That is
//     merely a per-job DISCOVERY error, so Prometheus stays up and healthy-looking
//     while the edge job quietly discovers zero targets — which promtool cannot
//     catch and nothing else would surface. Note gcloud's own --filter language
//     DOES accept `=` here; that mismatch is what made the broken expression look
//     verified when tested by hand.
func TestEdgeGCEServiceDiscoveryIsValid(t *testing.T) {
	body, err := os.ReadFile("../control-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)

	// Locate the generated gce_sd_configs entry (the heredoc-ish block that the
	// installer accumulates into $EDGE_GCE_SD).
	entry := regexp.MustCompile(`(?s)EDGE_GCE_SD}      - project: [^\n]*\n(.*?)\n"\n`).FindStringSubmatch(script)
	if entry == nil {
		t.Fatal("could not find the EDGE_GCE_SD gce_sd_configs entry in control-install.sh; " +
			"if it moved, update this test rather than deleting it")
	}
	block := entry[1]

	if !strings.Contains(block, "zone: ") {
		t.Errorf("generated gce_sd_config has no `zone`; Prometheus rejects the whole config "+
			"file with \"GCE SD configuration requires a zone\" and exits 2, crash-looping the "+
			"service and blanking every dashboard. Got:\n%s", block)
	}

	// Comment lines are skipped: the doc comment above the generator quotes the
	// broken `=` spellings on purpose, and matching those would be self-defeating.
	badOp := regexp.MustCompile(`tags\.items\s*=`)
	for i, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if badOp.MatchString(line) {
			t.Errorf("control-install.sh:%d uses `=` against tags.items, a REPEATED field: the "+
				"GCE list-filter grammar rejects it with 400 \"Invalid list filter expression\". "+
				"That is only a discovery error, so the job silently finds zero targets. Use the "+
				"`:` has-operator: filter: 'tags.items:\"sandbox-edge\"'. Line: %s", i+1, line)
		}
	}
	if !strings.Contains(block, `tags.items:"sandbox-edge"`) {
		t.Errorf("edge filter is not the verified `tags.items:\"sandbox-edge\"` has-expression; got:\n%s", block)
	}
}

// control-install.sh must validate the rendered config BEFORE the restart at the
// end of the script. `systemctl restart` is not a check: for a Type=simple unit
// it returns 0 as soon as the process forks, so a config Prometheus rejects at
// startup exits milliseconds later and Restart=always crash-loops it behind a
// green deploy.
func TestControlInstallValidatesPrometheusConfig(t *testing.T) {
	body, err := os.ReadFile("../control-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)

	check := strings.Index(script, "promtool check config")
	if check < 0 {
		t.Fatal("control-install.sh does not run `promtool check config`; a bad render would " +
			"crash-loop prometheus behind a successful-looking deploy")
	}
	if !strings.Contains(script, `install -m 0755 "$tmp/promtool"`) {
		t.Error("promtool is never installed, so the `promtool check config` gate cannot run")
	}
	if restart := strings.Index(script, "systemctl restart nomad-server"); restart < 0 {
		t.Error("could not find the service restart line")
	} else if check > restart {
		t.Error("`promtool check config` runs AFTER the restart; it must gate the restart, not follow it")
	}
}
