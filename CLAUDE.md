# CLAUDE.md

## Project overview

Firecracker-based microVM sandboxes for frontend development, exposed via a
local HTTP API over a Unix socket. Each sandbox boots Ubuntu 24.04 with Node 22,
pnpm, TypeScript, and a Vite React-TS project (Vite started by systemd on boot).
Lovable / e2b style — but self-hosted, on bare metal.

Multi-sandbox: each one gets its own tap, IP, host port, and rootfs copy.
State is in SQLite at `/var/lib/websandbox/registry.db`. The server (`websandbox serve`)
owns all running VMs in-process.

## Build & run

```bash
make build            # Local build (uses stub on macOS — Firecracker calls return ErrLinuxOnly)
make build-linux      # Cross-compile bin/websandbox for linux/amd64 (pure-Go SQLite, CGO disabled)
```

Server + CLI (on a Linux host; both need root):

```bash
sudo ./websandbox serve --config configs/devbox.json    # daemon-ish; listens on /run/websandbox.sock
sudo ./websandbox doctor --config configs/devbox.json   # env validation
sudo ./websandbox up                                    # POST /sandboxes → prints JSON + URL
sudo ./websandbox list                                  # GET /sandboxes
sudo ./websandbox down <id>                             # DELETE /sandboxes/<id>
```

The `up`, `down`, and `list` commands are thin HTTP clients over the Unix socket.
They need `sudo` because the socket is mode 0600 and the binary needs the
NOPASSWD sudoers rule below.

To stop the server: `sudo pkill websandbox` — SIGTERM triggers a graceful
shutdown that tears down every running sandbox.

## Remote deployment

```bash
make sync                              # build-linux + rsync bin/websandbox + Makefile + configs + scripts
make remote-doctor                     # ssh + run doctor
make remote-serve                      # ssh + run server (blocks)
make remote-up                         # ssh + create a sandbox
make remote-list                       # ssh + list
make remote-down SANDBOX=<id>          # ssh + destroy one
```

`sync` rsyncs `bin/websandbox` so the binary lands at `~/web-sandbox/websandbox`
(not `~/web-sandbox/bin/websandbox`). All `remote-*` targets and the README use
`./websandbox`. Don't reintroduce `./bin/websandbox` in remote commands.

NOPASSWD sudoers (one-time, lets the CLI/server run as root without prompting):

```
ayush ALL=(ALL) NOPASSWD: /home/ayush/web-sandbox/websandbox
```

in `/etc/sudoers.d/websandbox` with mode `0440`.

## One-time host setup

```bash
sudo bash scripts/setup-firecracker.sh      # install firecracker binary
sudo bash scripts/setup-kernel.sh           # download Firecracker-compatible kernel
sudo bash scripts/build-devbox-rootfs.sh    # build /opt/fc/devbox-rootfs.ext4 (resumable, ~5 min)
sudo bash scripts/setup-network.sh          # create br-fc, NAT, sysctls
```

`setup-network.sh` is NOT one-time per host: the bridge and iptables rules don't
survive a reboot (only the sysctls persist via `/etc/sysctl.d/99-firecracker.conf`).
Re-run after every host restart.

It sets these critical host-wide knobs:
- `net.ipv4.ip_forward=1`
- `net.ipv4.conf.all.route_localnet=1` — **required**: lets DNAT'd packets with src=127.0.0.1
  route out non-loopback interfaces (otherwise `curl localhost:<host_port>` hangs)
- `iptables -t nat -A POSTROUTING -o br-fc -j MASQUERADE` — **required**: rewrites
  host→guest source to the bridge IP so the guest can reply (otherwise it tries to
  reply to 127.0.0.1 and the connection times out)

If you change these, host:port → guest:5173 forwarding silently breaks.

## Code layout

```
cmd/websandbox/
  main.go              Root cobra command (wires serve/up/down/list/doctor)
  serve.go             Boots the API server (daemon-ish)
  up.go                Thin client: POST /sandboxes
  down.go              Thin client: DELETE /sandboxes/<id>
  list.go              Thin client: GET /sandboxes (tabwriter output)
  doctor.go            Colored env checks (Linux, KVM, firecracker, kernel, rootfs, bridge, ip_fwd, API socket)
  helpers.go           Shared cfg/socket flags and Client constructor
internal/config/config.go         JSON config + Defaults(); DisallowUnknownFields
internal/client/client.go         Unix-socket HTTP client (Create/List/Get/Destroy)
internal/server/server.go         http.ServeMux on Unix socket; owns map[id]*vm.Machine; vmCtx lifetime
internal/registry/registry.go     SQLite-backed registry; resource allocation (tap/IP/port from pools)
internal/provisioner/provisioner.go  Host-side ops: rootfs cp, tap create/delete, iptables DNAT
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
  via the SDK — recovery on next start is NOT implemented; stale taps/rootfs/DB rows must be
  cleaned manually. Acceptable for v1.
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

- **No startup reconciliation.** If the server crashes, the DB has stale `running` rows and
  the host has stale taps/rootfs/iptables. `pkill websandbox` then manual cleanup, or wipe
  `/var/lib/websandbox/`. Should add: on `serve` startup, for each row, check PID alive →
  if dead, run destroy() to release resources.
- **No `stop-server` command.** `pkill websandbox` is the current way; SIGTERM is handled
  gracefully via signal.NotifyContext.
- **No CoW rootfs.** Full `cp` on ext4 hosts. btrfs/XFS reflink is a one-line change.
- **No exec/SSH bridge into the guest.** Serial console only (root password `devbox`,
  baked into the rootfs build script).
- **No per-VM overrides on `POST /sandboxes`.** Vcpus, mem, kernel args, etc. are
  template-wide. Body currently ignored.
- **No tests.** Zero `_test.go` files.
- **API is local-only** (Unix socket, mode 0600). No auth, no TLS — for remote access
  you'd tunnel over SSH or front it with something else.
