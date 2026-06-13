# web-sandbox

Firecracker-based microVM sandboxes for development. Spin up isolated Ubuntu VMs — each with Node 22, Python 3, and common build tooling — in about two seconds, then run commands and edit files inside them over an HTTP API.

Think [Lovable](https://lovable.dev) / [e2b](https://e2b.dev) — but self-hosted, on bare metal.

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
│  host:5200 → VM#1:3000        host:5201 → VM#2:3000            │
└────────────────────────────────────────────────────────────────┘
```

A single long-running server (`sandbox serve`) owns all VMs. Each sandbox gets its own tap device, guest IP, host port, and rootfs copy, allocated atomically from pools in a SQLite registry. Every VM runs `sandboxd`, a small in-guest agent that the host proxies to for command execution and file I/O — so `create` returns only once the sandbox is actually ready to use.

Firecracker provides hardware-level isolation (KVM) with ~125ms boot times and ~5MB memory overhead. Each sandbox gets its own kernel, filesystem, and network stack.

## Requirements

- Linux host with **KVM** support (`/dev/kvm` must exist)
- Root access (Firecracker requires it)
- ~6 GB disk for shared assets, plus one sparse rootfs copy per sandbox

## Quick start

### 1. Build and sync to a remote Linux machine

```bash
git clone https://github.com/ayush6624/web-sandbox.git
cd web-sandbox
make sync REMOTE_HOST=your-server
```

### 2. One-time setup (on the remote machine)

```bash
ssh you@your-server
cd ~/web-sandbox

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

On startup the server also reconciles state left over from a crash or reboot: orphaned firecracker processes are killed and stale taps, rootfs copies, DNAT rules, and registry rows are cleaned up.

### 4. Use sandboxes

```bash
sudo ./sandbox up
# sandbox 890691a8-… ready → http://localhost:5200

sudo ./sandbox list
sudo ./sandbox exec 890691a8 -- "node --version && python3 --version"
echo 'export const x = 1' | sudo ./sandbox write 890691a8 /home/sandbox/x.ts
sudo ./sandbox read 890691a8 /home/sandbox/x.ts
sudo ./sandbox ls 890691a8 /home/sandbox
curl http://localhost:5200          # whatever you've started on guest :3000

sudo ./sandbox down 890691a8
sudo ./sandbox stop-server       # graceful: tears down all sandboxes
```

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
sandbox install-agent  Bake/refresh sandboxd inside the base rootfs
sandbox stop-server    Stop the server (SIGTERM; --force for SIGKILL)
sandbox doctor         Validate the environment
```

`up`, `down`, `list`, `exec`, `read`, `write`, and `ls` are thin HTTP clients over the server's Unix socket.

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
| `POST /sandboxes` | Create a sandbox; optional `{"timeout_sec": N}` body sets an auto-destroy TTL. Returns when the in-guest agent is healthy |
| `GET /sandboxes` | List running sandboxes |
| `GET /sandboxes/{id}` | Get one sandbox |
| `DELETE /sandboxes/{id}` | Graceful guest shutdown + resource cleanup |
| `POST /sandboxes/{id}/exec` | `{"cmd": "...", "cwd": "...", "timeout_sec": 60}` → `{stdout, stderr, exit_code, timed_out, duration_ms}` |
| `POST /sandboxes/{id}/exec/stream` | Same body; NDJSON stream of `{"type":"stdout"\|"stderr","data":…}` events ending with a `{"type":"exit",…}` event |
| `POST /sandboxes/{id}/timeout` | `{"timeout_sec": N}` resets the TTL (0 clears); a reaper destroys expired sandboxes |
| `POST /sandboxes/{id}/ports` | `{"guest_port": 8000}` → DNATs an extra guest port to a pool-allocated host port (idempotent) |
| `GET /sandboxes/{id}/ports` | All forwarded ports, including the primary 3000 mapping |
| `GET /sandboxes/{id}/files?path=` | Read a file (raw bytes) |
| `PUT /sandboxes/{id}/files?path=` | Write request body to a file (creates parent dirs) |
| `GET /sandboxes/{id}/dir?path=` | Directory listing (JSON) |
| `GET /sandboxes/{id}/shell?cols=&rows=&cwd=` | WebSocket upgrade → interactive `bash -l` on a pty. Binary frames carry raw terminal bytes; text frames carry `{"type":"resize","cols":…,"rows":…}`. Closes with reason `exit:<code>` |

The exec/file/shell endpoints are proxied to the `sandboxd` agent at `guestIP:8090` inside the VM.

## Configuration

Default config at `configs/devbox.json`. Anything omitted falls back to defaults:

| Field | Default | Description |
|-------|---------|-------------|
| `socket_path` | `/run/sandbox.sock` | API Unix socket |
| `db_path` | `/var/lib/sandbox/registry.db` | SQLite registry |
| `rootfs_base` | `/opt/fc/devbox-rootfs.ext4` | Immutable base image |
| `rootfs_dir` | `/var/lib/sandbox/rootfs` | Per-sandbox copies |
| `bridge` | `br-fc` | Host bridge for tap devices |
| `gateway_ip` | `172.16.0.1` | Bridge IP / guest default gateway |
| `guest_port` | `3000` | In-guest app port that gets forwarded |
| `pools.*` | taps `fc0-63`, IPs `.10-.73`, ports `5200-5263` | Resource pools |
| `vcpus`, `mem_mib` | 2, 1024 | Per-VM resources (template-wide) |
| `firecracker_bin`, `kernel_image`, `kernel_args` | … | VM template |

## Networking

```
Guest (172.16.0.x) ←──fcN──→ br-fc (172.16.0.1) ←──NAT──→ Internet
```

- **Guest → Internet**: iptables MASQUERADE through the host's default interface
- **Host → Guest**: direct via the bridge (this is how exec/files reach sandboxd)
- **External → Guest**: per-sandbox DNAT maps `host:520N` → `guestIP:3000`

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
web-sandbox/
├── cmd/
│   ├── sandbox/          CLI + server entry point (cobra)
│   └── sandboxd/            In-guest agent (exec + file HTTP API)
├── internal/
│   ├── agentapi/            Shared host↔guest protocol types
│   ├── client/              Unix-socket HTTP client for the CLI
│   ├── config/              JSON config with defaults
│   ├── provisioner/         Host ops: rootfs copies, taps, iptables, EnsureNetwork
│   ├── registry/            SQLite registry + resource pool allocation
│   ├── server/              HTTP API, VM ownership, startup reconciliation
│   └── vm/                  Firecracker SDK wrapper (+ non-Linux stub)
├── configs/devbox.json      Default configuration
├── scripts/                 Host setup (firecracker, kernel, rootfs, network)
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
