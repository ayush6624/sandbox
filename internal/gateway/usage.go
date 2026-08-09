package gateway

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ayush6624/sandbox/internal/client"
	"github.com/ayush6624/sandbox/internal/registry"
)

// Fleet-wide reads over the billable ledger.
//
// The gateway holds no billing state, exactly as it holds no routing state: it
// asks every live host and folds the answers together. What that can and cannot
// see is a property of the design, not an implementation gap — a host that the
// MIG deleted took its SQLite with it, so its usage exists only in the
// durability bucket. The bucket is the billing record; this endpoint is for
// dashboards and debugging.
const usageFanoutTimeout = 10 * time.Second

// handleUsage scatter-gathers GET /usage across every known host.
func (g *Gateway) handleUsage(w http.ResponseWriter, r *http.Request) {
	q, err := parseUsageQuery(r.URL.Query())
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}

	// Unlike handleList, this does NOT skip hosts that look empty. Usage
	// outlives the sandboxes that produced it, so the host most worth asking is
	// often one that now holds nothing at all: the sandboxes it billed for are
	// precisely the ones already destroyed.
	g.mu.RLock()
	candidates := make([]host, 0, len(g.hosts))
	for _, h := range g.hosts {
		candidates = append(candidates, *h)
	}
	g.mu.RUnlock()

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		reports  = make([]registry.UsageReport, 0, len(candidates))
		failures = map[string]error{}
	)
	for _, h := range candidates {
		wg.Add(1)
		go func(h host) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), usageFanoutTimeout)
			defer cancel()
			report, err := client.NewHTTP(h.addr, h.token).Usage(ctx, q)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fmt.Fprintf(os.Stderr, "gateway: usage from host %s: %v\n", h.id, err)
				failures[h.id] = err
				return
			}
			if report.HostID == "" {
				report.HostID = h.id
			}
			reports = append(reports, report)
		}(h)
	}
	wg.Wait()

	if len(failures) > 0 {
		// Fail closed, for the same reason handleList does and one more: a
		// partial list reads as lost sandboxes, and a partial LEDGER reads as
		// a smaller bill. Both are silent, and this one is also wrong in the
		// customer's favour, which is the kind of error nobody reports.
		hostIDs := make([]string, 0, len(failures))
		for hostID := range failures {
			hostIDs = append(hostIDs, hostID)
		}
		sort.Strings(hostIDs)
		details := make([]string, 0, len(hostIDs))
		for _, hostID := range hostIDs {
			details = append(details, fmt.Sprintf("%s: %v", hostID, failures[hostID]))
		}
		httpError(w, http.StatusBadGateway,
			fmt.Errorf("usage incomplete; %d/%d hosts failed: %s",
				len(hostIDs), len(candidates), strings.Join(details, "; ")))
		return
	}

	writeJSON(w, http.StatusOK, mergeUsageReports(q, reports))
}

// mergeUsageReports folds per-host answers into one.
//
// Totals are summed from each host's OWN totals rather than recomputed from the
// merged rows: a host aggregates in SQL over its whole selection even when it
// returns a truncated page, so summing totals stays exact where summing rows
// would quietly under-report every host that hit its limit.
func mergeUsageReports(q registry.UsageQuery, reports []registry.UsageReport) registry.UsageReport {
	out := registry.UsageReport{
		SandboxID: q.SandboxID,
		From:      timePtr(q.From),
		To:        timePtr(q.To),
		Intervals: []registry.UsageInterval{},
		Hosts:     make([]string, 0, len(reports)),
	}
	for _, report := range reports {
		out.Hosts = append(out.Hosts, report.HostID)
		out.Intervals = append(out.Intervals, report.Intervals...)
		out.Totals = out.Totals.Add(report.Totals)
		out.Truncated = out.Truncated || report.Truncated
	}
	sort.Strings(out.Hosts)
	sort.Slice(out.Intervals, func(i, j int) bool {
		if out.Intervals[i].StartedAt.Equal(out.Intervals[j].StartedAt) {
			return out.Intervals[i].ID < out.Intervals[j].ID
		}
		return out.Intervals[i].StartedAt.After(out.Intervals[j].StartedAt)
	})
	// The limit is per-host on the way in; applying it again here keeps the
	// fleet response the size the caller asked for, and Truncated says so.
	if limit := q.Limit; limit > 0 && len(out.Intervals) > limit {
		out.Intervals = out.Intervals[:limit]
		out.Truncated = true
	}
	return out
}

// parseUsageQuery mirrors the worker's parser: the gateway validates before
// fanning out so a malformed window costs one 400 instead of N host round trips
// that each reject it.
func parseUsageQuery(values map[string][]string) (registry.UsageQuery, error) {
	get := func(name string) string {
		if v := values[name]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	q := registry.UsageQuery{SandboxID: get("sandbox_id"), Limit: 1000}
	for name, dst := range map[string]*time.Time{"from": &q.From, "to": &q.To} {
		raw := get(name)
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return q, fmt.Errorf("%s must be an RFC 3339 timestamp", name)
		}
		*dst = t.UTC()
	}
	if !q.From.IsZero() && !q.To.IsZero() && !q.To.After(q.From) {
		return q, fmt.Errorf("to must be after from")
	}
	if raw := get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 5000 {
			return q, fmt.Errorf("limit must be between 1 and 5000")
		}
		q.Limit = n
	}
	return q, nil
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
