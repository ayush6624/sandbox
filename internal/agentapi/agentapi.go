// Package agentapi defines the HTTP protocol between the host server and the
// in-guest sandboxd agent. Both sides (and the CLI) share these types.
package agentapi

import "time"

// Port is the fixed port sandboxd listens on inside the guest. The host
// reaches it directly at guestIP:Port over the bridge (no DNAT involved).
const Port = 8090

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

// SSHKeyRequest installs an SSH public key into the guest so a user can `ssh`
// straight into the sandbox (POST /ssh-key). The host calls this right after
// the create readiness gate, on both the cold and hot (golden-clone) paths, so
// a sandbox created with a key is immediately reachable over SSH. The key is
// written to root's authorized_keys (sandboxd runs as root; login is as root).
// Idempotent: the file is overwritten, not appended. The key lives in the
// rootfs, so it survives hibernation/wake with no re-push.
type SSHKeyRequest struct {
	PublicKey string `json:"public_key"` // one authorized_keys line, e.g. "ssh-ed25519 AAAA... user@host"
}

// DirEntry is one row of a directory listing.
type DirEntry struct {
	Name  string    `json:"name"`
	Size  int64     `json:"size"`
	Mode  string    `json:"mode"`
	IsDir bool      `json:"is_dir"`
	MTime time.Time `json:"mtime"`
}
