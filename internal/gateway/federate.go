package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ayush6624/sandbox/internal/client"
)

// hostScrapeClient scrapes host /metrics over the same tuned transport the
// reverse proxy uses. A short timeout keeps one wedged host from stalling the
// whole federation scrape.
var hostScrapeClient = &http.Client{Transport: client.SharedTransport()}

// handleHostMetrics federates the per-host /metrics endpoints into one
// exposition. It scatter-gathers every live host's /metrics (concurrently,
// per-host timeout), injects a host="<id>" label into every series, and
// re-emits them grouped by metric family so the output is valid Prometheus text
// (a family's HELP/TYPE must appear once, before its samples).
//
// A synthetic sandbox_host_scrape_ok{host} gauge (1/0) marks which hosts
// answered, so a silently-unreachable worker is visible rather than just
// missing from the other series.
func (g *Gateway) handleHostMetrics(w http.ResponseWriter, r *http.Request) {
	g.mu.RLock()
	type target struct{ id, addr, token string }
	var live []target
	for _, h := range g.hosts {
		if time.Since(h.lastSeen) <= g.ttl {
			live = append(live, target{h.id, h.addr, h.token})
		}
	}
	g.mu.RUnlock()

	type scraped struct {
		id   string
		text string
		ok   bool
	}
	results := make([]scraped, len(live))
	var wg sync.WaitGroup
	for i, t := range live {
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()
			results[i].id = t.id
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			body, err := scrapeHost(ctx, t.addr, t.token)
			if err != nil {
				fmt.Fprintf(os.Stderr, "gateway: scrape metrics from host %s: %v\n", t.id, err)
				return
			}
			results[i].text, results[i].ok = body, true
		}(i, t)
	}
	wg.Wait()

	// Accumulate samples per metric family, keeping the first-seen HELP/TYPE
	// (identical across hosts — they run the same build).
	fam := newFamilySet()
	for _, res := range results {
		up := "0"
		if res.ok {
			up = "1"
			fam.ingest(res.id, res.text)
		}
		fam.addSample("sandbox_host_scrape_ok", "gauge",
			"1 if the gateway scraped this host's /metrics on the last collection, else 0.",
			fmt.Sprintf("sandbox_host_scrape_ok{host=%q} %s", res.id, up))
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(fam.render()))
}

// scrapeHost fetches a host's /metrics text with bearer auth.
func scrapeHost(ctx context.Context, addr, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/metrics", nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hostScrapeClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("host returned %d", resp.StatusCode)
	}
	return string(body), nil
}

// family collects the samples for one metric name across all hosts, plus its
// HELP/TYPE (captured once).
type family struct {
	name    string
	help    string
	typ     string
	samples []string
}

type familySet struct {
	order  []string // family names, in first-seen order (sorted at render)
	byName map[string]*family
}

func newFamilySet() *familySet { return &familySet{byName: map[string]*family{}} }

func (fs *familySet) fam(name string) *family {
	f, ok := fs.byName[name]
	if !ok {
		f = &family{name: name}
		fs.byName[name] = f
		fs.order = append(fs.order, name)
	}
	return f
}

// addSample appends a fully-formed sample line to a family, filling in HELP/TYPE
// if not already set.
func (fs *familySet) addSample(name, typ, help, sample string) {
	f := fs.fam(name)
	if f.help == "" {
		f.help = help
	}
	if f.typ == "" {
		f.typ = typ
	}
	f.samples = append(f.samples, sample)
}

// ingest parses one host's exposition text and folds it into the family set,
// injecting host="<id>" into every sample's labels.
func (fs *familySet) ingest(hostID, text string) {
	var help, typ map[string]string
	help, typ = map[string]string{}, map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			// "# HELP name text..." / "# TYPE name kind"
			fields := strings.SplitN(line, " ", 4)
			if len(fields) >= 4 && fields[1] == "HELP" {
				help[fields[2]] = fields[3]
			} else if len(fields) >= 4 && fields[1] == "TYPE" {
				typ[fields[2]] = fields[3]
			}
			continue
		}
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			continue // malformed; skip rather than corrupt output
		}
		series, value := line[:sp], line[sp+1:]
		name, labeled := injectHostLabel(series, hostID)
		f := fs.fam(name)
		if f.help == "" {
			f.help = help[name]
		}
		if f.typ == "" {
			f.typ = typ[name]
		}
		f.samples = append(f.samples, labeled+" "+value)
	}
}

// injectHostLabel rewrites a series identifier to carry host="<id>" as its first
// label, and returns the metric-family name (the part before any '{'). It
// handles both `name` and `name{a="b",...}`.
func injectHostLabel(series, hostID string) (name, labeled string) {
	hl := fmt.Sprintf("host=%q", hostID)
	if i := strings.IndexByte(series, '{'); i >= 0 {
		name = series[:i]
		inner := strings.TrimSuffix(series[i+1:], "}")
		if inner == "" {
			return name, name + "{" + hl + "}"
		}
		return name, name + "{" + hl + "," + inner + "}"
	}
	return series, series + "{" + hl + "}"
}

// render emits the accumulated families as valid Prometheus text: each family's
// HELP/TYPE once, then its samples. Families are sorted for stable output.
func (fs *familySet) render() string {
	sort.Strings(fs.order)
	var b strings.Builder
	for _, name := range fs.order {
		f := fs.byName[name]
		if f.help != "" {
			fmt.Fprintf(&b, "# HELP %s %s\n", name, f.help)
		}
		if f.typ != "" {
			fmt.Fprintf(&b, "# TYPE %s %s\n", name, f.typ)
		}
		for _, s := range f.samples {
			b.WriteString(s)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
