package apiv1

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/ayush6624/sandbox/internal/httpapi"
	"github.com/ayush6624/sandbox/internal/registry"
)

// Billable usage, in the public v1 vocabulary.
//
// The adapter's job here is more than renaming fields. The ledger stores what
// is cheap to store — integer seconds, a raw cgroup counter, "0 means template
// default" already resolved — and a caller needs to know which of those numbers
// they are charged for. So the response separates BILLED quantities
// (vcpu_seconds, memory_mib_seconds) from RECORDED ones (cpu_seconds), and says
// on every response what the window selected and which hosts answered.

// UsageInterval is one billable span: the time a single VM served a sandbox. A
// pause/resume cycle produces two, because it runs two VMs.
type UsageInterval struct {
	ID        string `json:"id"`
	SandboxID string `json:"sandbox_id"`
	// Sequence counts a sandbox's intervals from 1.
	Sequence int64  `json:"sequence"`
	HostID   string `json:"host_id,omitempty"`
	// State is "open" while the sandbox is still running on this VM.
	State     string    `json:"state"`
	Resources Resources `json:"resources"`
	StartedAt time.Time `json:"started_at"`
	// EndedAt is absent while the interval is open. An open interval is
	// measured to its last heartbeat, never to "now", so a report is
	// reproducible and a crashed host cannot bill an outage.
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	DurationSeconds float64    `json:"duration_seconds"`
	// Billed quantities: allocated resources × duration.
	VcpuSeconds      float64 `json:"vcpu_seconds"`
	MemoryMIBSeconds float64 `json:"memory_mib_seconds"`
	// CPUSeconds is host CPU actually consumed. Recorded for transparency, and
	// deliberately NOT the billing base — CPU is oversubscribed, so consumption
	// is neither predictable nor something a caller controls.
	CPUSeconds float64           `json:"cpu_seconds"`
	EndReason  string            `json:"end_reason,omitempty"`
	Metadata   map[string]string `json:"metadata"`
}

// UsageTotals aggregates a selection. It covers everything the window selected,
// including intervals beyond the current page.
type UsageTotals struct {
	Intervals        int64   `json:"intervals"`
	OpenIntervals    int64   `json:"open_intervals"`
	DurationSeconds  float64 `json:"duration_seconds"`
	VcpuSeconds      float64 `json:"vcpu_seconds"`
	MemoryMIBSeconds float64 `json:"memory_mib_seconds"`
	CPUSeconds       float64 `json:"cpu_seconds"`
}

// UsageWindow echoes the requested window and states how it selects.
type UsageWindow struct {
	From *time.Time `json:"from,omitempty"`
	To   *time.Time `json:"to,omitempty"`
	// Selection is always "overlap": an interval that started before the window
	// and is still running is included, and it is reported WHOLE rather than
	// clipped to the window. Clipping would have to apportion cpu_seconds,
	// which is one counter for the whole interval and cannot be split over time.
	Selection string `json:"selection"`
}

// UsageCoverage is the honest caveat, in the response rather than the docs: a
// live read sees only hosts that are still alive.
type UsageCoverage struct {
	Hosts []string `json:"hosts,omitempty"`
	// Scope is always "live_hosts". Usage from a worker that has since been
	// deleted survives only in the durability bucket, which is the billing
	// record of truth; this API is for dashboards and debugging.
	Scope string `json:"scope"`
	// Truncated means a host returned fewer rows than it holds. Totals are
	// unaffected.
	Truncated bool `json:"truncated"`
}

type usageResponse struct {
	Intervals     []UsageInterval `json:"intervals"`
	Totals        UsageTotals     `json:"totals"`
	Window        UsageWindow     `json:"window"`
	Coverage      UsageCoverage   `json:"coverage"`
	NextPageToken string          `json:"next_page_token,omitempty"`
}

func (h *Handler) listUsage(w http.ResponseWriter, r *http.Request) {
	h.usageFrom(w, r, "/usage")
}

// getSandboxUsage reads one sandbox's ledger. It routes by id, so on a fleet it
// reaches the owning host — which means it can only answer for a sandbox that
// still has an owner. A deleted sandbox's usage is reachable through
// GET /v1/usage?sandbox_id=, which asks every host instead, and the 404 below
// says so rather than leaving a caller to conclude the usage is gone.
func (h *Handler) getSandboxUsage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h.usageFrom(w, r, "/sandboxes/"+url.PathEscape(id)+"/usage")
}

func (h *Handler) usageFrom(w http.ResponseWriter, r *http.Request, path string) {
	query, err := usageForwardQuery(r.URL.Query())
	if err != nil {
		httpapi.WriteProblem(w, r, 400, "invalid_request", err.Error())
		return
	}
	if query != "" {
		path += "?" + query
	}
	rec := h.call(r, http.MethodGet, path, nil)
	if rec.Code == http.StatusNotFound {
		httpapi.WriteProblem(w, r, 404, "sandbox_not_found",
			"sandbox not found; usage for a deleted sandbox is available from GET /v1/usage?sandbox_id=")
		return
	}
	if !translateError(w, r, rec) {
		return
	}
	var report registry.UsageReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		httpapi.WriteProblem(w, r, 502, "invalid_upstream_response", err.Error())
		return
	}

	items := make([]UsageInterval, 0, len(report.Intervals))
	for _, iv := range report.Intervals {
		items = append(items, publicUsageInterval(iv))
	}
	page, next, ok := paginate(w, r, items)
	if !ok {
		return
	}
	writeJSON(w, 200, usageResponse{
		Intervals:     page,
		Totals:        publicUsageTotals(report.Totals),
		Window:        UsageWindow{From: report.From, To: report.To, Selection: "overlap"},
		Coverage:      UsageCoverage{Hosts: reportHosts(report), Scope: "live_hosts", Truncated: report.Truncated},
		NextPageToken: next,
	})
}

// usageForwardQuery passes only the ledger filters through to the legacy route.
// Pagination is applied here, over the rows that come back, so page_size and
// page_token must not leak downstream where they would mean something else.
func usageForwardQuery(in url.Values) (string, error) {
	out := url.Values{}
	for _, name := range []string{"from", "to"} {
		value := in.Get(name)
		if value == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339, value); err != nil {
			return "", &usageParamError{name: name}
		}
		out.Set(name, value)
	}
	if value := in.Get("sandbox_id"); value != "" {
		out.Set("sandbox_id", value)
	}
	return out.Encode(), nil
}

type usageParamError struct{ name string }

func (e *usageParamError) Error() string { return e.name + " must be an RFC 3339 timestamp" }

func publicUsageInterval(iv registry.UsageInterval) UsageInterval {
	state := "open"
	if iv.EndedAt != nil {
		state = "closed"
	}
	return UsageInterval{
		ID:               iv.ID,
		SandboxID:        iv.SandboxID,
		Sequence:         iv.Seq,
		HostID:           iv.HostID,
		State:            state,
		Resources:        Resources{VCPU: iv.Vcpus, MemoryMIB: iv.MemMIB},
		StartedAt:        iv.StartedAt,
		EndedAt:          iv.EndedAt,
		DurationSeconds:  iv.Duration().Seconds(),
		VcpuSeconds:      iv.VcpuSeconds(),
		MemoryMIBSeconds: iv.MemMIBSeconds(),
		CPUSeconds:       iv.CPUSeconds(),
		EndReason:        iv.EndReason,
		Metadata:         nonNilMetadata(iv.Metadata),
	}
}

func publicUsageTotals(t registry.UsageTotals) UsageTotals {
	return UsageTotals{
		Intervals:        t.Intervals,
		OpenIntervals:    t.OpenIntervals,
		DurationSeconds:  t.DurationSeconds,
		VcpuSeconds:      t.VcpuSeconds,
		MemoryMIBSeconds: t.MemMIBSeconds,
		CPUSeconds:       t.CPUSeconds,
	}
}

// reportHosts normalizes the two shapes the ledger answers in: a worker names
// itself, a gateway names everyone that answered.
func reportHosts(report registry.UsageReport) []string {
	if len(report.Hosts) > 0 {
		return report.Hosts
	}
	if report.HostID != "" {
		return []string{report.HostID}
	}
	return nil
}
