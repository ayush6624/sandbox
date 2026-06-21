# CLAUDE.md

## Project overview

Firecracker-based microVM sandboxes for development, exposed via a
local HTTP API over a Unix socket. Each sandbox boots Ubuntu 24.04 with Node 22,
pnpm, TypeScript, Python 3, and common build tooling (build-essential, git). It's
a bare sandbox — no app server runs on boot; the primary guest port (3000) is
forwarded to a host port for whatever you start there. e2b style — but
self-hosted, on bare metal.

Multi-sandbox: each one gets its own tap, IP, host port, and rootfs copy.
State is in SQLite at `/var/lib/sandbox/registry.db`. The server (`sandbox serve`)
owns all running VMs in-process.

Every VM also runs `sandboxd` (cmd/sandboxd), a small in-guest HTTP agent on
`:8090` providing exec + file read/write. The host server proxies
`/sandboxes/{id}/exec|files|dir` to it over the bridge, and `POST /sandboxes`
blocks until the agent answers `/health` (~2 s total), so a created sandbox is
immediately usable.

## Build & run

```bash
make build            # Local build (uses stub on macOS — Firecracker calls return ErrLinuxOnly)
make build-linux      # Cross-compile bin/sandbox for linux/amd64 (pure-Go SQLite, CGO disabled)
```

Server + CLI (on a Linux host; both need root):

```bash
sudo ./sandbox serve --config configs/devbox.json    # daemon-ish; listens on /run/sandbox.sock
sudo ./sandbox doctor --config configs/devbox.json   # env validation
sudo ./sandbox up                                    # POST /sandboxes → prints JSON + URL
sudo ./sandbox list                                  # GET /sandboxes
sudo ./sandbox down <id>                             # DELETE /sandboxes/<id>
sudo ./sandbox exec <id> -- "node --version"         # run a command in the guest
sudo ./sandbox shell <id>                            # interactive PTY shell (WebSocket) in the guest
sudo ./sandbox read <id> /path                       # file out of the guest → stdout
sudo ./sandbox write <id> /path [--from local]       # stdin/local file → guest
sudo ./sandbox ls <id> [/path]                       # list a guest directory
sudo ./sandbox install-agent --agent ./sandboxd      # bake sandboxd into base rootfs
sudo ./sandbox stop-server [--force]                 # SIGTERM (graceful) / SIGKILL the server
```

The non-serve commands are thin HTTP clients over the Unix socket. They need
`sudo` because the socket is mode 0600 and the binary needs the NOPASSWD
sudoers rule below. `install-agent` and `stop-server` are subcommands (not
scripts) specifically so they're covered by that NOPASSWD rule.

`serve` is self-healing on startup: it runs `EnsureNetwork` (bridge, sysctls,
NAT — survives host reboots) and reconciles stale state (kills orphaned
firecracker processes, removes stale taps/rootfs/DNAT/DB rows).

## Remote deployment

```bash
make sync                              # build-linux + rsync bin/{sandbox,sandboxd} + Makefile + configs + scripts
make remote-install-agent              # sync + bake sandboxd into the base rootfs
make remote-doctor                     # ssh + run doctor
make remote-serve                      # ssh + run server (blocks)
make remote-up                         # ssh + create a sandbox
make remote-list                       # ssh + list
make remote-down SANDBOX=<id>          # ssh + destroy one
```

`sync` rsyncs the binaries so they land at `~/sandbox/sandbox` and
`~/sandbox/sandboxd` (not under `bin/`). All `remote-*` targets and the
README use `./sandbox`. Don't reintroduce `./bin/sandbox` in remote commands.

NOPASSWD sudoers (one-time, lets the CLI/server run as root without prompting):

```
ayush ALL=(ALL) NOPASSWD: /home/ayush/sandbox/sandbox
```

in `/etc/sudoers.d/sandbox` with mode `0440`.

## One-time host setup

```bash
sudo bash scripts/setup-firecracker.sh      # install firecracker binary
sudo bash scripts/setup-kernel.sh           # download Firecracker-compatible kernel
sudo bash scripts/build-devbox-rootfs.sh    # build /opt/fc/devbox-rootfs.ext4 (resumable, ~5 min)
sudo ./sandbox install-agent             # bake sandboxd into the rootfs (loop-mount, fast)
```

`setup-network.sh` still exists but is no longer required: `serve` runs
`provisioner.EnsureNetwork()` on every startup, which idempotently creates the
bridge, sets the sysctls, and adds the NAT/FORWARD rules. A host reboot just
needs `serve` restarted.

EnsureNetwork sets these critical host-wide knobs:
- `net.ipv4.ip_forward=1`
- `net.ipv4.conf.all.route_localnet=1` — **required**: lets DNAT'd packets with src=127.0.0.1
  route out non-loopback interfaces (otherwise `curl localhost:<host_port>` hangs)
- `iptables -t nat -A POSTROUTING -o br-fc -j MASQUERADE` — **required**: rewrites
  host→guest source to the bridge IP so the guest can reply (otherwise it tries to
  reply to 127.0.0.1 and the connection times out)

If you change these, host:port → guest:3000 forwarding silently breaks.

## Code layout

```
cmd/sandbox/
  main.go              Root cobra command (wires all subcommands)
  serve.go             Boots the API server (EnsureNetwork + reconcile + listen); --gateway opts into fleet registration
  gateway.go           Boots the multi-host control plane (sandbox gateway)
  up.go                Thin client: POST /sandboxes
  down.go              Thin client: DELETE /sandboxes/<id>
  list.go              Thin client: GET /sandboxes (tabwriter output)
  exec.go              Thin client: POST /sandboxes/<id>/exec; exits with the command's exit code
  shell.go             Interactive PTY: raw-mode stdin ↔ WebSocket /shell; relays SIGWINCH resizes
  files.go             Thin clients: read/write/ls over /files and /dir
  installagent.go      Loop-mounts the base rootfs, installs sandboxd + systemd unit
  stopserver.go        Finds `sandbox serve` PIDs via /proc, SIGTERM/SIGKILL
  doctor.go            Colored env checks (Linux, KVM, firecracker, kernel, rootfs, bridge, ip_fwd, API socket)
  helpers.go           Shared cfg/socket flags and Client constructor
cmd/sandboxd/main.go   In-guest agent: /health, /exec, /files (GET/PUT), /dir, /shell (PTY WebSocket) on :8090
internal/agentapi/agentapi.go     Shared host↔guest protocol types + port constant
internal/config/config.go         JSON config + Defaults(); DisallowUnknownFields
internal/client/client.go         HTTP client: New(socket) for the local socket, NewHTTP(addr,token) for TCP+bearer (gateway/host)
internal/cluster/cluster.go       Host→gateway heartbeat protocol type
internal/gateway/gateway.go       Multi-host control plane: host registry, derived routing, placement, reverse-proxy, scatter-gather
internal/server/
  server.go           http.ServeMux on Unix socket; owns map[id]*vm.Machine; vmCtx lifetime
  proxy.go            Reverse-proxy to sandboxd (incl. /shell WebSocket via httputil) + waitForAgent readiness poll
  heartbeat.go        When --gateway is set, periodically registers this host with the gateway
  reconcile.go        Startup cleanup of stale rows/taps/rootfs/orphan firecrackers
internal/registry/registry.go     SQLite-backed registry; resource allocation (tap/IP/port from pools)
internal/provisioner/provisioner.go  Host-side ops: EnsureNetwork, rootfs cp, tap create/delete, iptables DNAT
internal/vm/
  machine_linux.go    Firecracker SDK integration; ShutdownGuest, Wait, PID; captures stderr to firecracker-<vmid>.log
  machine_stub.go     Non-Linux stub matching the Linux signatures
  options.go          RunOptions + RuntimeConfig
configs/devbox.json   Default config (pools, bridge, paths, vCPUs/mem)
scripts/              Host setup shell scripts
```

## Architecture notes

- **Single long-running server.** `serve` owns every `*vm.Machine` in `machines sync.Map`.
  If the server crashes, firecracker children become orphaned and we can no longer ACPI-shutdown
  via the SDK. On the next `serve` startup, `reconcile()` kills any process whose
  `/proc/<pid>/comm` is `firecracker` for each registry row (guards against PID reuse), then
  releases DNAT rules, tap, rootfs copy, and the row itself. Every row is stale by definition
  at startup, since VMs only live inside a running server.
- **Multi-host is a gateway in front, not shared state.** `sandbox gateway` fronts the same
  API and fans out across hosts. Each host keeps its own SQLite + pools + `reconcile()`
  unchanged (a *shared* DB would break reconcile's "every row is stale" + PID checks). Hosts
  opt in with `serve --gateway <url> --gateway-token <tok> --listen <addr> --token <addr-tok>`
  and heartbeat (`internal/server/heartbeat.go`) their `{addr, token, slots, sandbox_ids}` to
  the gateway every 5 s. The gateway (`internal/gateway`) holds **no durable state**: it rebuilds
  its `sandbox_id → host` routing table from heartbeats, so it self-heals after a restart once
  each host reports. `POST /sandboxes` places on the least-loaded live host (flip to bin-pack
  for Phase-2 autoscaling); id-scoped requests (incl. `/exec/stream` + `/shell`) are
  reverse-proxied to the owning host with the host's token injected; `GET /sandboxes`
  scatter-gathers. Point the CLI at it with `--gateway <addr> --gateway-token <tok>`. The
  whole roadmap (incl. Phase-2 elastic/Nomad) is in `~/.claude/plans/i-know-the-agent-abundant-starfish.md`.
- **Guest agent readiness gates create.** `handleCreate` polls `http://guestIP:8090/health`
  for up to 60 s and tears the sandbox down if the agent never answers. If the base rootfs
  lacks sandboxd (fresh build, forgot `install-agent`), every create will fail this way —
  that's the first thing to check.
- **exec kills whole process groups.** sandboxd runs commands with `Setpgid` and kills
  `-pgid` on timeout so shell children don't outlive the request. stdout/stderr are capped
  at 2 MiB each (`agentapi.MaxOutputBytes`).
- **Streaming exec is NDJSON, not SSE.** `POST .../exec/stream` emits
  `agentapi.ExecEvent` lines (stdout/stderr/exit); the server proxy wraps the
  ResponseWriter in a flush-on-write writer so chunks pass through immediately. All
  non-Type ExecEvent fields are omitempty — decoders must treat absent fields as zero.
- **Interactive shell is a WebSocket PTY.** `GET /sandboxes/{id}/shell` upgrades and
  `handleShellProxy` reverse-proxies it to the guest's `/shell` via `httputil.ReverseProxy`
  (Go handles the Upgrade handshake + raw byte copy natively, so the host needs no
  WebSocket lib and it works over both the Unix socket and the TCP listener). In the guest,
  sandboxd runs `bash -l` on a real pty (`creack/pty`): binary frames are raw terminal bytes
  both ways, text frames are JSON `agentapi.ShellControl` resizes. Clean exit closes the
  socket with reason `exit:<code>`; client disconnect kills the shell's process group. See
  the protocol doc-comment in `agentapi`.
- **TTL reaper.** `POST /sandboxes` accepts optional `{"timeout_sec":N}`; a 10 s ticker
  goroutine in `Serve` destroys rows whose `expires_at` passed. `POST .../timeout`
  resets (0 clears). No default TTL — absent means live forever.
- **Extra port mappings** live in the `sandbox_ports` table and draw host ports from the
  same pool as primary ports (`loadUsed` reads both tables). destroy() and reconcile()
  must remove their DNAT rules — read mappings before deleting rows.
- **`vmCtx` ≠ request ctx.** `handleCreate` must pass `s.vmCtx` (server-scoped) to `vm.NewMachine`
  and `vm.Start`, NOT `r.Context()` — the request ctx cancels when the handler returns, and the
  firecracker SDK SIGTERMs the VM when its ctx cancels. This was an early bug that wasted hours.
- **Pools allocated atomically via SQLite.** `registry.Create` runs INSERT inside a TX with
  partial unique indexes (`uniq_tap_running` etc.) guaranteeing no two running sandboxes share
  a tap/IP/port. Concurrent creates that race lose to UNIQUE constraint and surface as 500.
- **Per-VM rootfs is a full `cp --sparse=always`.** Slow on ext4 (~2 GB-sparse copy in ~1 s,
  but I/O scales linearly with N). On btrfs/XFS, switching to `--reflink=auto` would make it
  instant. Don't share the rootfs between VMs — ext4 corrupts under concurrent mount.
- **Build tags**: `//go:build linux` for SDK code, `//go:build !linux` for the stub. Keep the
  signatures identical in both files.
- **`disableValidation` arg on `NewMachine`** lets you build the SDK config on non-Linux for
  dry runs. Server passes `false`.
- **Firecracker stderr/stdout is captured** to `firecracker-<vmid>.log` in the server's cwd.
  After `/logger` is bootstrapped, firecracker writes most logs to its log FIFO (drained by
  the SDK, never persisted). For deep-dive debugging, switch `LogFifo` to a regular file path.

## Conventions

- Config merging: JSON file < CLI flags. Only `--config` and `--socket` flags exist now;
  per-VM overrides are not yet exposed in `POST /sandboxes`.
- Socket paths auto-generate UUIDs when left empty.
- Use `signal.NotifyContext` for signal handling, not raw `signal.Notify` + channel.
- Commits: short imperative subject lines (see `git log`). No co-author trailer.
- Use `modernc.org/sqlite` (pure-Go) NOT `github.com/mattn/go-sqlite3` — we need
  `CGO_ENABLED=0` to cross-compile from macOS.

## Not done yet

- **No CoW rootfs.** Full `cp` on ext4 hosts. btrfs/XFS reflink is a one-line change.
- **No per-VM overrides on `POST /sandboxes`.** Vcpus, mem, kernel args, etc. are
  template-wide. Body currently ignored.
- **No tests on the Go side.** Zero `_test.go` files (the TS SDK has a mock-server suite).
- **No TLS on the TCP listener.** `serve --listen <tailnet-ip>:8080 --token <tok>` exposes
  the API over TCP with bearer auth (constant-time compare); we rely on Tailscale for
  transport security. Don't bind it to a public interface. The Unix socket stays auth-free
  (mode 0600). The local token for the dev machine lives in `.sandbox-token` (gitignored).
