package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

// Read paths over the billable ledger (phase 3 of docs/usage-metering-plan.md).
//
// Two things about these handlers are load-bearing:
//
//  1. They answer from the LEDGER, never from the sandbox table. Usage outlives
//     the sandbox it describes — Destroy deletes the sandbox row outright — so a
//     handler that first resolved the id would report nothing for exactly the
//     sandboxes an invoice is made of.
//  2. Totals are aggregated in SQL over the whole selection while rows are
//     paginated, so a truncated page never understates what is owed.
//
// The bucket, not this API, is the billing record: a worker only holds its own
// intervals, and only until the retention window prunes the spooled ones.

const (
	// usageMaxLimit bounds one response. Intervals are ~300 bytes, so this is a
	// few MiB worst case, and the caller can page.
	usageMaxLimit = 5000
	// usageDefaultLimit keeps an unparameterized read cheap.
	usageDefaultLimit = 1000
)

// handleUsage answers for this host's ledger: GET /usage?from=&to=&sandbox_id=.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	q, err := parseUsageQuery(r.URL.Query())
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	s.writeUsageReport(w, r, q)
}

// handleSandboxUsage answers for one sandbox: GET /sandboxes/{id}/usage.
//
// It deliberately does NOT check that the sandbox exists. A destroyed sandbox's
// intervals are still on this host until they are pruned, and they are the ones
// most likely to be queried — the bill arrives after the sandbox is gone.
func (s *Server) handleSandboxUsage(w http.ResponseWriter, r *http.Request) {
	q, err := parseUsageQuery(r.URL.Query())
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	q.SandboxID = r.PathValue("id")
	s.writeUsageReport(w, r, q)
}

func (s *Server) writeUsageReport(w http.ResponseWriter, r *http.Request, q registry.UsageQuery) {
	intervals, truncated, err := s.reg.QueryUsage(r.Context(), q)
	if err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Errorf("read usage ledger: %w", err))
		return
	}
	totals, err := s.reg.UsageTotalsFor(r.Context(), q)
	if err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Errorf("total usage ledger: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, registry.UsageReport{
		HostID:    s.hostID(),
		SandboxID: q.SandboxID,
		From:      timePtr(q.From),
		To:        timePtr(q.To),
		Intervals: intervals,
		Totals:    totals,
		Truncated: truncated,
	})
}

// parseUsageQuery reads the window from query parameters. Absent bounds mean
// "everything this host still holds"; a malformed bound is rejected rather than
// silently ignored, because a dropped filter turns into a wrong number that
// looks right.
func parseUsageQuery(values url.Values) (registry.UsageQuery, error) {
	q := registry.UsageQuery{SandboxID: values.Get("sandbox_id"), Limit: usageDefaultLimit}
	for name, dst := range map[string]*time.Time{"from": &q.From, "to": &q.To} {
		raw := values.Get(name)
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
	if raw := values.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > usageMaxLimit {
			return q, fmt.Errorf("limit must be between 1 and %d", usageMaxLimit)
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
