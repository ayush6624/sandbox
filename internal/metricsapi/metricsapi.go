// Package metricsapi is the wire contract for per-sandbox resource
// utilization: what a sandbox is CONSUMING, as opposed to the allocation the
// usage ledger bills. It exists as its own package because the worker produces
// these samples and both the CLI client and the public v1 adapter read them —
// the alternative is a second copy of the struct drifting on the client side.
//
// See docs/sandbox-metrics-plan.md.
package metricsapi

import "time"

// Sample is one point in a sandbox's utilization series. Field names track
// E2B's metrics contract where the semantics match, so an SDK adapter is a
// rename rather than a translation.
type Sample struct {
	Timestamp time.Time `json:"timestamp"`
	// VMMGeneration counts the VMMs this host has observed serving the
	// sandbox. Every counter below belongs to a VMM, not to the sandbox: a
	// wake, restore or fanout replaces it and restarts them at zero. Publishing
	// the generation makes that reset self-describing, instead of leaving a
	// consumer to infer it from a counter that went backwards.
	VMMGeneration int64 `json:"vmm_generation"`
	CPUCount      int64 `json:"cpu_count"`
	// CPUUsedPct is consumed host CPU over the tick as a percentage of the
	// sandbox's ALLOCATED vCPUs, so 100 means "using everything it was given".
	// Absent (0) on the first sample of a generation, which has no predecessor
	// to difference against.
	CPUUsedPct      float64 `json:"cpu_used_pct"`
	CPUSecondsTotal float64 `json:"cpu_seconds_total"`
	// HostMemBytes is the VMM's cgroup charge: guest pages TOUCHED. Without a
	// balloon device (see CLAUDE.md, "No memory overcommit") freed guest pages
	// never come back, so this is a high-water mark of what the host is paying
	// — deliberately not named mem_used, which is a guest number phase 2 adds.
	HostMemBytes int64 `json:"host_mem_bytes"`
	// RootfsAllocBytes is the per-VM rootfs's allocated blocks. Under reflink
	// CoW this is the sandbox's real incremental cost on the shared data disk.
	RootfsAllocBytes int64 `json:"rootfs_alloc_bytes"`
	// Net counters are from the GUEST's perspective: the tap's rx is the
	// guest's tx, so they are swapped on the way in.
	NetRxBytes int64 `json:"net_rx_bytes"`
	NetTxBytes int64 `json:"net_tx_bytes"`

	// Guest-reported fields (phase 2). Present only when guest stats are
	// enabled AND the agent answered: an agent predating GET /stats leaves
	// them absent rather than reporting a zero that reads as "no memory used".
	MemTotalBytes  int64    `json:"mem_total_bytes,omitempty"`
	MemUsedBytes   int64    `json:"mem_used_bytes,omitempty"`
	DiskTotalBytes int64    `json:"disk_total_bytes,omitempty"`
	DiskUsedBytes  int64    `json:"disk_used_bytes,omitempty"`
	Load1          *float64 `json:"load1,omitempty"`
	Processes      int      `json:"processes,omitempty"`
}

// SandboxMetrics is the GET /sandboxes/{id}/metrics response.
type SandboxMetrics struct {
	Samples []Sample `json:"samples"`
	// State is the sandbox's status. A hibernated sandbox keeps its samples
	// and stops producing new ones; reading them must not wake it.
	State           string  `json:"state"`
	IntervalSeconds float64 `json:"interval_seconds"`
}
