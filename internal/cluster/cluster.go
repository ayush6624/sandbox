// Package cluster defines the wire protocol between a host's `sandbox serve`
// agent and the `sandbox gateway` control plane.
//
// The gateway holds no durable state of its own: hosts push a Heartbeat on a
// fixed interval carrying everything the gateway needs (where to reach the
// host, the credential to use, current capacity, and the IDs of the sandboxes
// it owns). The gateway rebuilds its sandbox→host routing table from these
// heartbeats, so it self-heals after a restart once every host has reported
// once. Hosts remain the source of truth for which sandboxes actually exist.
package cluster

// Heartbeat is the body of POST {gateway}/register, sent by a host on startup
// and then periodically. Re-sends are idempotent — the gateway upserts by
// HostID and refreshes the host's last-seen time.
type Heartbeat struct {
	// HostID is a stable identifier for the host (defaults to its hostname),
	// so a restarted host reclaims its identity rather than duplicating it.
	HostID string `json:"host_id"`
	// Addr is the host's TCP API address the gateway dials back (e.g. its
	// tailnet IP:port). Must match the host's `serve --listen` address.
	Addr string `json:"addr"`
	// Token is the bearer token the gateway must present when calling Addr —
	// i.e. the host's own api_token. Sent over the (Tailscale) control link.
	Token string `json:"token"`
	// SlotsTotal is the host's sandbox capacity (min of its tap/IP/port pools).
	SlotsTotal int `json:"slots_total"`
	// SlotsUsed is the number of running sandboxes on the host right now.
	// Hibernated sandboxes don't count — their resources are released.
	SlotsUsed int `json:"slots_used"`
	// SlotsFree is the number of creates the host can actually satisfy right
	// now — min over its per-pool availability, including port holds by
	// hibernated sandboxes and extra exposed ports (registry.FreeSlots). The
	// gateway must place against this, NOT SlotsTotal-SlotsUsed: hibernated
	// sandboxes hold no slot but do hold their host port, so the difference
	// overstates capacity. Pointer so the gateway can tell an old host binary
	// (absent → fall back to SlotsTotal-SlotsUsed) from a genuine zero. A host
	// still warming up (golden snapshot build) advertises 0 to avoid attracting
	// a cold-boot storm.
	SlotsFree *int `json:"slots_free,omitempty"`
	// Hibernated is the number of idle sandboxes frozen to disk on this host.
	// They appear in SandboxIDs (requests must route here to wake them) but
	// consume no slots.
	Hibernated int `json:"hibernated,omitempty"`
	// SandboxIDs are the IDs of the running sandboxes the host owns. The
	// gateway derives its routing table from these.
	SandboxIDs []string `json:"sandbox_ids"`
	// SnapshotIDs are the IDs of the user snapshots stored on the host
	// (golden snapshots excluded — they're per-host internals). The gateway
	// derives its snapshot→host routing from these so restore/fanout/delete
	// reach the host that has the artifacts locally.
	SnapshotIDs []string `json:"snapshot_ids,omitempty"`
}
