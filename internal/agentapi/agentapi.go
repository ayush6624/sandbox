// Package agentapi defines the HTTP protocol between the host server and the
// in-guest sandboxd agent. Both sides (and the CLI) share these types.
package agentapi

import "time"

// Port is the fixed port sandboxd listens on inside the guest. The host
// reaches it directly at guestIP:Port over the bridge (no DNAT involved).
const Port = 8090

// AgentPath is where sandboxd is installed in every guest. It is a shared
// constant rather than three literals because a template guest boots the agent
// as init (`init=<AgentPath>`, see internal/server/template.go), so the path the
// build overlays it at and the path the kernel is told to exec must agree.
const AgentPath = "/usr/local/bin/sandboxd"

// GuestUser is the unprivileged account every exec, file operation, and shell
// runs as. sandboxd resolves it BY NAME in the guest's /etc/passwd, so a
// template built from a container image has to carry an entry under this name.
const GuestUser = "sandbox"

// GuestProfilePath is where a template build records the identity the IMAGE
// declares, for sandboxd to adopt instead of the defaults above. A container
// image's contract is "processes run as its USER, in its WORKDIR" — usually
// root in something like /app — and workloads written for that image assume it:
// a Terminal-Bench verifier apt-gets packages and checks `$PWD`. Honoring it is
// what makes an unmodified published image behave as a sandbox.
//
// It is deliberately a file in the image rather than a create-time field: the
// values come from the image, so they belong to the template, not the request.
// Absent (every non-template guest) means the defaults, so the base image is
// completely unaffected.
//
// Running as root here is consistent with the model this project already
// documents — the isolation boundary is the microVM, not the guest uid, which
// is why the base image grants passwordless sudo in the first place.
const GuestProfilePath = "/etc/sandbox-guest.json"

// GuestProfile is the content of GuestProfilePath. Empty fields fall back to
// the built-in defaults, so a partially written profile degrades rather than
// breaking the guest.
type GuestProfile struct {
	User string `json:"user,omitempty"`
	Home string `json:"home,omitempty"`
	Cwd  string `json:"cwd,omitempty"`
}

// ThawWakeEtherType and ThawWakeMagic identify the private Ethernet frame the
// host sends across an unbridged tap immediately after resuming a clone. It is
// only a latency hint; MMDS polling remains the correctness fallback.
const (
	ThawWakeEtherType = 0x88B5
	ThawWakeMagic     = "sandbox-thaw-v1"
)

// DefaultTimeout bounds command execution when ExecRequest.TimeoutSec is 0.
const DefaultTimeout = 60 * time.Second

// MaxOutputBytes caps captured stdout/stderr per stream.
const MaxOutputBytes = 2 << 20 // 2 MiB

// ExecRequest asks the agent to run a shell command.
type ExecRequest struct {
	Cmd        string            `json:"cmd"`                   // run via bash -lc
	Cwd        string            `json:"cwd,omitempty"`         // default: /home/sandbox/app
	Env        map[string]string `json:"env,omitempty"`         // appended to the agent's env
	TimeoutSec int               `json:"timeout_sec,omitempty"` // default: DefaultTimeout
}

// ExecResult is the outcome of an ExecRequest.
type ExecResult struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out"`
	DurationMS int64  `json:"duration_ms"`
}

// ExecEvent types (the Type field of ExecEvent).
const (
	EventStdout = "stdout"
	EventStderr = "stderr"
	EventExit   = "exit"
)

// ExecEvent is one NDJSON line of a streaming exec response (POST /exec/stream).
// Output events carry Type stdout/stderr plus Data; the stream ends with
// exactly one exit event carrying ExitCode/TimedOut/DurationMS. All non-Type
// fields are omitempty, so decoders must treat absent fields as zero values
// (e.g. a successful exit may arrive as {"type":"exit","duration_ms":12}).
type ExecEvent struct {
	Type       string `json:"type"`
	Data       string `json:"data,omitempty"`
	ExitCode   int    `json:"exit_code,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// Shell is the WebSocket sub-protocol for interactive PTY sessions: GET /shell
// on the agent, proxied as GET /sandboxes/{id}/shell on the host. Once the
// connection is upgraded the two sides exchange:
//
//   - Binary frames: raw terminal bytes. Client→guest frames are written to the
//     pty (stdin); guest→client frames are pty output (stdout+stderr combined).
//   - Text frames: JSON ShellControl messages (currently only window resize).
//
// Initial window size and working directory ride on the handshake URL as query
// params: ?cols=<n>&rows=<n>&cwd=<path>. The guest closes the WebSocket when the
// shell process exits, carrying the exit code in the close reason as "exit:<n>".
const (
	// ShellResize is the Type of a ShellControl message that resizes the pty.
	ShellResize = "resize"
	// ShellExitPrefix prefixes the WebSocket close reason on a clean shell exit,
	// e.g. "exit:0". Clients parse the trailing integer for the exit code.
	ShellExitPrefix = "exit:"
)

// ShellControl is a JSON control message sent as a WebSocket text frame on a
// /shell connection.
type ShellControl struct {
	Type string `json:"type"`           // currently only ShellResize
	Cols uint16 `json:"cols,omitempty"` // terminal width in columns
	Rows uint16 `json:"rows,omitempty"` // terminal height in rows
}

// ClockSyncRequest asks the agent to step the guest's CLOCK_REALTIME to the
// host's wall clock (POST /clock). A snapshot-restored guest resumes with its
// clock frozen at snapshot-creation time — hours stale for golden-snapshot hot
// creates — and NTP is not a reliable fallback (some deployments block
// outbound UDP). The host calls this right after the readiness gate on every
// resume path; sub-second accuracy is all that's needed.
type ClockSyncRequest struct {
	UnixNano int64 `json:"unix_nano"` // host CLOCK_REALTIME, Unix nanoseconds
}

// GuestIdentityRequest initializes security-sensitive identity for one
// independently created sandbox. Repeating the same SandboxID is a no-op;
// changing it rotates SSH host keys and clears inherited authorized keys.
type GuestIdentityRequest struct {
	SandboxID string `json:"sandbox_id"`
}

// SnapshotPollRequest switches the thaw agent into/out of rapid MMDS polling.
// The host arms it immediately before pausing a VM for snapshot, so the
// captured process resumes looking for clone identity without imposing a high
// steady-state poll rate on every running sandbox.
type SnapshotPollRequest struct {
	Armed bool `json:"armed"`
}

// SSHKeyRequest installs an SSH public key for the normal sandbox user. The
// host calls this only after guest identity initialization has cleared any key
// inherited from the source image or snapshot. Idempotent: the file is
// overwritten, not appended. The key survives pause/resume unchanged.
type SSHKeyRequest struct {
	PublicKey string `json:"public_key"` // one authorized_keys line, e.g. "ssh-ed25519 AAAA... user@host"
}

// Stats is the guest's own view of its resources, polled by the host's
// utilization sampler (GET /stats). It carries only what the host cannot see
// from outside the VM: the hypervisor's cgroup charge is guest pages TOUCHED,
// which never falls when the guest frees memory, and the rootfs is an opaque
// block device from the host's side. Everything else — CPU, network, allocated
// disk blocks — the host measures for itself and is not asked for here.
//
// Absent fields read as zero. An agent predating this endpoint 404s it, and
// the host degrades to its own host-side fields rather than failing the tick.
type Stats struct {
	MemTotalBytes int64 `json:"mem_total_bytes"`
	// MemAvailableBytes is MemAvailable, not MemFree: page cache is reclaimable,
	// so MemFree reads as "almost out of memory" on any guest that has done I/O.
	MemAvailableBytes int64   `json:"mem_available_bytes"`
	DiskTotalBytes    int64   `json:"disk_total_bytes"`
	DiskFreeBytes     int64   `json:"disk_free_bytes"`
	Load1             float64 `json:"load1"`
	Processes         int     `json:"processes"`
}

// DirEntry is one row of a directory listing.
type DirEntry struct {
	Name  string    `json:"name"`
	Size  int64     `json:"size"`
	Mode  string    `json:"mode"`
	IsDir bool      `json:"is_dir"`
	MTime time.Time `json:"mtime"`
}
