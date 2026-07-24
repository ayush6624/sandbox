# sandbox

Firecracker-based microVM sandboxes for development. Spin up isolated Ubuntu VMs — each with Node 22, Python 3, and common build tooling — in a few hundred milliseconds, then run commands and edit files inside them over an HTTP API or the TypeScript SDK.

Think [Lovable](https://lovable.dev) / [e2b](https://e2b.dev) — but self-hosted, on bare metal.

**Docs:** [Quickstart](docs/quickstart.md) · [Concepts](docs/concepts.md) · [HTTP API](docs/http-api.md) · [Self-hosting](docs/self-hosting.md) · [TypeScript SDK](sdk/typescript/README.md)

- **Fast creates** — every `POST /sandboxes` clones a pre-booted golden snapshot (~0.5 s end-to-end; automatic cold-boot fallback).
- **Snapshots & fan-out** — capture a running sandbox (memory + processes + disk), restore it 1:1, or fan out N copy-on-write clones (32 clones in ~2.7 s).
- **Multi-host** — a stateless gateway fronts N hosts with the same API: least-loaded placement, per-sandbox routing, merged listing.
- **e2b-style SDK** — `Sandbox.create()`, `commands.run` (buffered or streaming), `files`, ports, TTLs.

## How it works

```
┌────────────────────────────────────────────────────────────────┐
│  Host (Linux + KVM)                                            │
│                                                                │
│  sandbox serve  ──── /run/sandbox.sock (HTTP API)        │
│       │                                                        │
│       │ POST /sandboxes                                        │
│       ▼                                                        │
│  ┌───────────────────────────┐  ┌───────────────────────────┐  │
│  │ microVM #1    172.16.0.10 │  │ microVM #2    172.16.0.11 │  │
│  │                           │  │                           │  │
│  │ Ubuntu + Node 22 + Py3    │  │ Ubuntu + Node 22 + Py3    │  │
│  │ sandboxd agent     :8090  │  │ sandboxd agent     :8090  │  │
│  └───────────┬───────────────┘  └───────────┬───────────────┘  │
│              │ fc0                          │ fc1              │
│              └──────────┬───────────────────┘                  │
│                       br-fc (bridge, NAT)                      │
│                                                                │
│  explicit expose: host:5200 → VM#1:3000                         │
└────────────────────────────────────────────────────────────────┘
```

A single long-running server (`sandbox serve`) owns all VMs. Each sandbox gets its own tap device, guest IP, and rootfs copy, allocated atomically from pools in a SQLite registry. Host ports are allocated only when explicitly exposed. Every VM runs `sandboxd`, a small in-guest agent that the host proxies to for command execution and file I/O — so `create` returns only once the sandbox is actually ready to use.

At startup the server boots one pristine sandbox, snapshots it, and serves every subsequent create by cloning that **golden snapshot** — memory and all — instead of cold-booting. To scale past one machine, `sandbox gateway` fronts any number of hosts with the same API ([concepts](docs/concepts.md#multi-host-fleet-mode)).

Firecracker provides hardware-level isolation (KVM) with ~5MB memory overhead. Each sandbox gets its own kernel, filesystem, and network stack.

## Requirements

- Linux host with **KVM** support (`/dev/kvm` must exist)
- Root access (Firecracker requires it)
- ~6 GB disk for shared assets, plus one sparse rootfs copy per sandbox

## Quick start

### 1. Build and sync to a remote Linux machine

```bash
git clone https://github.com/ayush6624/sandbox.git
cd sandbox
make sync REMOTE_HOST=your-server
```

### 2. One-time setup (on the remote machine)

```bash
ssh you@your-server
cd ~/sandbox

# Install Firecracker + kernel
sudo bash scripts/setup-firecracker.sh
sudo bash scripts/setup-kernel.sh

# Build the devbox rootfs (takes ~5 min, resumable if interrupted)
sudo apt-get install -y debootstrap
sudo bash scripts/build-devbox-rootfs.sh

# Bake the sandboxd guest agent into the rootfs
sudo ./sandbox install-agent --agent ./sandboxd
```

Host networking (bridge, NAT, sysctls) is ensured automatically every time the server starts — no separate network setup step, and nothing to re-run after a reboot.

### 3. Start the server

```bash
sudo ./sandbox serve --config configs/devbox.json
```

On startup the server also reconciles state left over from a crash or reboot: orphaned firecracker processes are killed and stale taps, rootfs copies, legacy DNAT rules, and registry rows are cleaned up; the port-forward listeners of hibernated sandboxes are re-bound.

### 4. Use sandboxes

```bash
sudo ./sandbox up
# sandbox 890691a8-… ready

sudo ./sandbox list
sudo ./sandbox exec 890691a8 -- "node --version && python3 --version"
echo 'export const x = 1' | sudo ./sandbox write 890691a8 /home/sandbox/x.ts
sudo ./sandbox read 890691a8 /home/sandbox/x.ts
sudo ./sandbox ls 890691a8 /home/sandbox
sudo ./sandbox expose 890691a8 3000 # prints the allocated host port
curl http://localhost:5200          # if expose printed host 5200

sudo ./sandbox down 890691a8
sudo ./sandbox stop-server       # graceful: tears down all sandboxes
```

### 5. Or use the TypeScript SDK

Expose the API over TCP (`serve --listen <private-ip>:8080 --token <tok>`), then from any machine that can reach it:

```ts
import { Sandbox } from 'sandbox'   // sdk/typescript

const sbx = await Sandbox.create({ timeoutMs: 600_000 })
await sbx.commands.run('pnpm create vite my-app')
await sbx.files.write('/home/sandbox/app/index.js', code)
const host = await sbx.exposePort(3000) // "your-server:5200"
await sbx.kill()
```

See the [SDK README](sdk/typescript/README.md) for streaming exec, snapshots & fan-out, ports, and e2b migration.

## CLI

```
sandbox serve          Run the API server (owns all VMs)
sandbox up [--ttl s]   Create a sandbox; blocks until the agent is ready
sandbox down <id>      Destroy a sandbox
sandbox list           List running sandboxes
sandbox exec [--stream] <id> -- <cmd>   Run a shell command inside a sandbox
sandbox shell <id>           Open an interactive PTY shell inside a sandbox
sandbox read <id> <path>     Read a file from a sandbox to stdout
sandbox write <id> <path>    Write stdin (or --from file) into a sandbox
sandbox ls <id> [path]       List a directory inside a sandbox
sandbox expose <id> <port>   Forward an extra guest port to a host port
sandbox ports <id>           List a sandbox's forwarded ports
sandbox gateway        Run the multi-host gateway (control plane, no root needed)
sandbox install-agent  Bake/refresh sandboxd inside the base rootfs
sandbox stop-server    Stop the server (SIGTERM; --force for SIGKILL)
sandbox doctor         Validate the environment
```

`up`, `down`, `list`, `exec`, `read`, `write`, and `ls` are thin HTTP clients over the server's Unix socket. Add `--gateway <addr:port> --gateway-token <tok>` to any of them to drive a fleet gateway over TCP instead (no sudo needed).

## HTTP API

The server listens on a Unix socket (`/run/sandbox.sock`, mode 0600). It can
additionally serve TCP — e.g. on a Tailscale address for SDK access from other
machines — with bearer-token auth:

```bash
sudo ./sandbox serve --listen <tailnet-ip>:8080 --token $(openssl rand -hex 24)
# clients send: Authorization: Bearer <token>
```

Endpoints (both listeners):

| Method & path | Description |
|---|---|
| `GET /info` | Host template defaults (`default_vcpus`, `default_mem_mib`) and per-sandbox override limits |
| `POST /sandboxes` | Create a sandbox; optional `timeout_sec` sets an auto-destroy TTL and `hibernate_after_sec` overrides idle hibernation (`-1` disables). Returns when the in-guest agent is healthy |
| `GET /sandboxes` | List running sandboxes |
| `GET /sandboxes/{id}` | Get one sandbox |
| `DELETE /sandboxes/{id}` | Graceful guest shutdown + resource cleanup |
| `POST /sandboxes/{id}/exec` | `{"cmd": "...", "cwd": "...", "timeout_sec": 60}` → `{stdout, stderr, exit_code, timed_out, duration_ms}` |
| `POST /sandboxes/{id}/exec/stream` | Same body; NDJSON stream of `{"type":"stdout"\|"stderr","data":…}` events ending with a `{"type":"exit",…}` event |
| `POST /sandboxes/{id}/timeout` | `{"timeout_sec": N}` resets the TTL (0 clears); a reaper destroys expired sandboxes |
| `POST /sandboxes/{id}/ports` | `{"guest_port": 8000}` → explicitly forwards a guest port from a pool-allocated host port (idempotent) |
| `GET /sandboxes/{id}/ports` | All explicitly forwarded ports |
| `GET /sandboxes/{id}/files?path=` | Read a file (raw bytes) |
| `PUT /sandboxes/{id}/files?path=` | Write request body to a file (creates parent dirs) |
| `GET /sandboxes/{id}/dir?path=` | Directory listing (JSON) |
| `GET /sandboxes/{id}/shell?cols=&rows=&cwd=` | WebSocket upgrade → interactive `bash -l` on a pty. Binary frames carry raw terminal bytes; text frames carry `{"type":"resize","cols":…,"rows":…}`. Closes with reason `exit:<code>`; errors close with code `4000+status` (browsers may auth via `?access_token=`) |
| `POST /sandboxes/{id}/snapshot` | Capture the running sandbox (memory + processes + disk); it pauses ~1 s and keeps running |
| `POST /snapshots/{id}/restore` | Boot a new sandbox resuming the snapshot 1:1 (source must be dead) |
| `POST /snapshots/{id}/fanout` | `{"count": N}` → N identity-neutral clones, each with a fresh IP and CoW disk |
| `GET /snapshots` / `DELETE /snapshots/{id}` | List / delete saved snapshots |

The exec/file/shell endpoints are proxied to the `sandboxd` agent at `guestIP:8090` inside the VM. Full request/response shapes, errors, and limits: [HTTP API reference](docs/http-api.md).

**Multi-host:** `sandbox gateway --listen <ip>:9090 --token <tok>` fronts N hosts with this same API (hosts join with `serve --gateway …`); it adds `GET /hosts` for fleet state and routes id-scoped requests to the owning host. See [Self-hosting](docs/self-hosting.md#multi-host-fleet).

## Configuration

Default config at `configs/devbox.json`. Anything omitted falls back to defaults:

| Field | Default | Description |
|-------|---------|-------------|
| `socket_path` | `/run/sandbox.sock` | API Unix socket |
| `listen_addr` / `api_token` | — | Optional TCP listener with bearer auth |
| `gateway_url` / `gateway_token` | — | Register this host with a fleet gateway |
| `db_path` | `/var/lib/sandbox/registry.db` | SQLite registry |
| `rootfs_base` | `/opt/fc/devbox-rootfs.ext4` | Immutable base image |
| `rootfs_dir` | `/var/lib/sandbox/rootfs` | Per-sandbox copies (XFS/btrfs → instant reflink clones) |
| `snapshot_dir` | `/var/lib/sandbox/snapshots` | Snapshot artifacts (memory + state + frozen rootfs) |
| `disable_hot_create` | `false` | `true` = always cold-boot creates instead of cloning the golden snapshot |
| `hibernate_after_sec` | `600` in shipped configs | Hibernate after externally idle seconds; `0` disables the host default |
| `bridge` | `br-fc` | Host bridge for tap devices |
| `gateway_ip` | `172.16.0.1` | Bridge IP / guest default gateway |
| `pools.*` | taps `fc0-63`, IPs `.10-.73`, ports `5200-5263` | VM identity and explicit-forwarding pools |
| `vcpus`, `mem_mib` | 2, 1024 | Per-VM resources (template-wide) |
| `firecracker_bin`, `kernel_image`, `kernel_args` | … | VM template |

## Networking

```
Guest (172.16.0.x) ←──fcN──→ br-fc (172.16.0.1) ←──NAT──→ Internet
```

- **Guest → Internet**: iptables MASQUERADE through the host's default interface
- **Host → Guest**: direct via the bridge (this is how exec/files reach sandboxd)
- **External → Guest**: after an explicit port exposure, the server proxies `host:520N` → `guestIP:<requested-port>` in userspace. Each connection counts as sandbox activity and transparently wakes a hibernated sandbox.

Guest IPs are set via the kernel `ip=` boot parameter — no DHCP. The server ensures the bridge, sysctls (`ip_forward`, `route_localnet`), and NAT rules on every startup, so a host reboot needs nothing more than restarting `sandbox serve`.

## What's in the rootfs

The base rootfs is a 10 GB sparse ext4 image built by `scripts/build-devbox-rootfs.sh`:

| Layer | Details |
|-------|---------|
| **Base OS** | Ubuntu 24.04 (Noble) via debootstrap |
| **Node** | Node.js 22 LTS, npm, pnpm, TypeScript |
| **Python** | Python 3, pip, venv |
| **Build tooling** | build-essential (gcc/g++/make), git |
| **Services** | `sandboxd.service` (agent on `:8090`) — no app server runs by default |
| **Debug** | Root password `devbox`, serial console on `ttyS0` |

Each sandbox boots from its own sparse copy of this image; writes never touch the base. The build script is resumable, and `sandbox install-agent` updates the agent in-place without a rebuild.

To avoid rebuilding on every host, package the built image once and stash it in object storage (e.g. R2):

```bash
sudo bash scripts/package-rootfs.sh          # -> ./dist/devbox-rootfs.tar.zst (+ .sha256)
# upload dist/* to your bucket
```

A prebuilt image is published, so you can skip the build entirely:

```
https://sandbox.ayushgoyal.dev/images/devbox-rootfs.tar.zst
https://sandbox.ayushgoyal.dev/images/devbox-rootfs.tar.zst.sha256
```

On a fresh host, the pull helper does the whole restore — download, verify the checksum, sparse-extract into `/opt/fc`, and bake the agent in:

```bash
sudo bash scripts/fetch-rootfs.sh https://sandbox.ayushgoyal.dev/images/devbox-rootfs.tar.zst
sudo ./sandbox serve --config configs/devbox.json
```

The tarball is sparse-aware, so it carries only real content (~1–1.5 GB) rather than the full 10 GB. The cached image holds no agent — `fetch-rootfs.sh` runs `install-agent` (a fast loop-mount) after download, so the `sandboxd` binary you ship stays updatable independently of the OS layer.

## Project structure

```
sandbox/
├── cmd/
│   ├── sandbox/             CLI + server + gateway entry point (cobra)
│   └── sandboxd/            In-guest agent (exec, files, PTY shell, thaw/reidentify)
├── internal/
│   ├── agentapi/            Shared host↔guest protocol types
│   ├── client/              HTTP client for the CLI (Unix socket or TCP+token)
│   ├── config/              JSON config with defaults
│   ├── gateway/             Multi-host control plane (placement, routing, scatter-gather)
│   ├── provisioner/         Host ops: rootfs copies, taps, iptables, ARP listener
│   ├── registry/            SQLite registry + resource pool allocation + snapshots
│   ├── server/              HTTP API, VM ownership, golden snapshot, reconciliation
│   └── vm/                  Firecracker integration: boot, snapshot, clone (+ stub)
├── sdk/typescript/          TypeScript SDK (e2b-style) + examples + benchmarks
├── docs/                    Quickstart, concepts, API reference, self-hosting
├── infra/gcp/               Reference fleet deployment (GCP VMs + systemd units)
├── configs/devbox.json      Default configuration
├── scripts/                 Host setup (firecracker, kernel, rootfs, bootstrap)
└── Makefile                 Build, sync, remote targets
```

## Makefile targets

| Target | Description |
|--------|-------------|
| `make build` | Compile locally (uses stub on macOS) |
| `make build-linux` | Cross-compile `sandbox` + `sandboxd` for linux/amd64 |
| `make sync` | Build + rsync binaries, configs, scripts to remote |
| `make remote-setup` | Install Firecracker + kernel on remote |
| `make remote-setup-devbox` | Build rootfs + network setup on remote |
| `make remote-install-agent` | Sync + bake sandboxd into the base rootfs |
| `make remote-serve` | Run the server on remote (blocks) |
| `make remote-up` / `remote-list` / `remote-down SANDBOX=<id>` | Sandbox lifecycle |
| `make remote-doctor` | Validate the remote environment |

Override the remote target: `make sync REMOTE_USER=you REMOTE_HOST=your-server`

## Developing locally

The project compiles on macOS/Windows via a build stub — all Firecracker calls return `ErrLinuxOnly`. This lets you work on the CLI, server, registry, and config without a Linux machine:

```bash
go build ./...          # compiles fine on macOS
```

To actually run VMs, you need Linux with KVM. Use `make sync` to push to a remote machine.

## How Firecracker compares

| | Firecracker | Docker | Traditional VM |
|---|---|---|---|
| **Isolation** | Hardware (KVM) | Process (namespaces) | Hardware (KVM) |
| **Boot time** | ~125ms | ~500ms | ~10-30s |
| **Memory overhead** | ~5 MB | ~10 MB | ~100+ MB |
| **Kernel** | Dedicated per VM | Shared with host | Dedicated per VM |
| **Root filesystem** | Dedicated per VM | Layered (overlayfs) | Dedicated per VM |
| **Attack surface** | Minimal (reduced device model) | Broad (shared kernel) | Broad (full device model) |

Firecracker was built by AWS for Lambda and Fargate. It strips the virtual device model down to the bare minimum — no USB, no GPU, no PCI — giving you VM-level security at container-like speed.

## License

MIT
