// Package grafana_test guards the provisioned dashboards against the one failure
// mode they have: a panel that queries a metric nothing exports, or a job label
// nothing scrapes. Both render as an empty panel, and "No data" is
// indistinguishable from "nothing is happening" — which is exactly how a bad
// Prometheus render once held the fleet's whole metrics stack down for three days
// without anyone noticing (see the comment in prometheus.yml.tpl). Grafana never
// validates an expression, so nothing else in the repo can catch a renamed or
// deleted series here.
package grafana_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type dashboard struct {
	UID    string `json:"uid"`
	Title  string `json:"title"`
	Panels []struct {
		ID         int    `json:"id"`
		Type       string `json:"type"`
		Title      string `json:"title"`
		Datasource struct {
			UID string `json:"uid"`
		} `json:"datasource"`
		GridPos struct {
			H, W, X, Y int
		} `json:"gridPos"`
		Targets []struct {
			Expr string `json:"expr"`
		} `json:"targets"`
	} `json:"panels"`
	Templating struct {
		List []struct {
			Name       string `json:"name"`
			Definition string `json:"definition"`
		} `json:"list"`
	} `json:"templating"`
}

func loadDashboards(t *testing.T) map[string]dashboard {
	t.Helper()
	paths, err := filepath.Glob("dashboards/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no dashboards found: %v", err)
	}
	out := map[string]dashboard{}
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var d dashboard
		// Unknown fields are fine — this asserts the parts that can break
		// silently, not the whole Grafana schema.
		if err := json.Unmarshal(body, &d); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if d.UID == "" || d.Title == "" {
			t.Errorf("%s: uid and title are required (provisioning keys on uid)", p)
		}
		out[p] = d
	}
	return out
}

var metricRE = regexp.MustCompile(`\bsandbox[_:][a-zA-Z0-9_:]+`)

// exportedMetrics scans the Go sources for metric names written as literals.
// Every /metrics writer in this repo is hand-rolled Fprintf, so a literal scan
// is a sound oracle — and it deliberately fails when a series is renamed or
// deleted while a panel still asks for it.
func exportedMetrics(t *testing.T) map[string]bool {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "worktrees", "vendor":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range metricRE.FindAllString(string(body), -1) {
			found[m] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) < 20 {
		t.Fatalf("only %d metric literals found; the source scan is broken, not the dashboards", len(found))
	}
	return found
}

// recordedRules returns the recording rules Prometheus synthesizes, which are
// legitimate dashboard targets but appear in no Go source.
func recordedRules(t *testing.T) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "prometheus", "rules.yml.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		if _, name, ok := strings.Cut(strings.TrimSpace(line), "- record:"); ok {
			out[strings.TrimSpace(name)] = true
		}
	}
	return out
}

func TestDashboardMetricsExist(t *testing.T) {
	exported, rules := exportedMetrics(t), recordedRules(t)
	known := func(m string) bool {
		if exported[m] || rules[m] {
			return true
		}
		// Histogram families export only the base name as a literal.
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if strings.HasSuffix(m, suffix) && exported[strings.TrimSuffix(m, suffix)] {
				return true
			}
		}
		return false
	}
	for path, d := range loadDashboards(t) {
		exprs := map[string]string{} // metric -> panel that asks for it
		for _, p := range d.Panels {
			for _, tg := range p.Targets {
				for _, m := range metricRE.FindAllString(tg.Expr, -1) {
					exprs[m] = p.Title
				}
			}
			for _, v := range d.Templating.List {
				for _, m := range metricRE.FindAllString(v.Definition, -1) {
					exprs[m] = "variable " + v.Name
				}
			}
		}
		for m, panel := range exprs {
			if !known(m) {
				t.Errorf("%s: panel %q queries %s, which nothing exports — the panel will read No data",
					path, panel, m)
			}
		}
	}
}

// Only three jobs exist in prometheus.yml.tpl, and a series is only reachable
// under the job that actually scrapes it: worker series arrive relabelled under
// sandbox-hosts, not sandbox-gateway.
func TestDashboardJobLabelsAreScraped(t *testing.T) {
	tpl, err := os.ReadFile(filepath.Join("..", "prometheus", "prometheus.yml.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	jobRE := regexp.MustCompile(`job_name:\s*(\S+)`)
	scraped := map[string]bool{}
	for _, m := range jobRE.FindAllStringSubmatch(string(tpl), -1) {
		scraped[m[1]] = true
	}
	useRE := regexp.MustCompile(`job="([^"]+)"`)
	for path, d := range loadDashboards(t) {
		for _, p := range d.Panels {
			for _, tg := range p.Targets {
				for _, m := range useRE.FindAllStringSubmatch(tg.Expr, -1) {
					if !scraped[m[1]] {
						t.Errorf("%s: panel %q selects job=%q, which no scrape config defines", path, p.Title, m[1])
					}
				}
			}
		}
	}
}

// Provisioning keys on uid, and Grafana keys panel state on panel id: a
// duplicate uid makes one dashboard silently replace another on the control VM,
// and duplicate panel ids break links and edit state within a dashboard.
func TestDashboardIdentifiersAreUnique(t *testing.T) {
	boards := loadDashboards(t)
	uids := map[string]string{}
	for path, d := range boards {
		if prev, dup := uids[d.UID]; dup {
			t.Errorf("%s and %s share uid %q", prev, path, d.UID)
		}
		uids[d.UID] = path

		ids := map[int]string{}
		for _, p := range d.Panels {
			if prev, dup := ids[p.ID]; dup {
				t.Errorf("%s: panels %q and %q share id %d", path, prev, p.Title, p.ID)
			}
			ids[p.ID] = p.Title
			// Rows and text panels query nothing; everything else must point at
			// the one datasource provisioning actually creates.
			if p.Type != "row" && p.Type != "text" && p.Datasource.UID != "sandbox-prom" {
				t.Errorf("%s: panel %q uses datasource %q, want sandbox-prom (the only provisioned one)",
					path, p.Title, p.Datasource.UID)
			}
		}
	}
}

// Grafana silently stacks overlapping panels on top of each other, so a layout
// mistake hides a panel rather than reporting one.
func TestDashboardPanelsDoNotOverlap(t *testing.T) {
	for path, d := range loadDashboards(t) {
		type cell struct{ x, y int }
		owner := map[cell]string{}
		for _, p := range d.Panels {
			if p.GridPos.W <= 0 || p.GridPos.H <= 0 || p.GridPos.X+p.GridPos.W > 24 {
				t.Errorf("%s: panel %q has gridPos %+v, which is off the 24-column grid", path, p.Title, p.GridPos)
				continue
			}
			for y := p.GridPos.Y; y < p.GridPos.Y+p.GridPos.H; y++ {
				for x := p.GridPos.X; x < p.GridPos.X+p.GridPos.W; x++ {
					if prev, taken := owner[cell{x, y}]; taken {
						t.Errorf("%s: panel %q overlaps %q at (%d,%d)", path, p.Title, prev, x, y)
						return // one report is enough; the rest are the same mistake
					}
					owner[cell{x, y}] = p.Title
				}
			}
		}
	}
}
