# web-sandbox

Firecracker-based microVM sandboxes for frontend development. Spin up isolated Ubuntu VMs — each with a fully configured React/TypeScript environment — in about two seconds, then run commands and edit files inside them over an HTTP API.

Think [Lovable](https://lovable.dev) / [e2b](https://e2b.dev) — but self-hosted, on bare metal.

## How it works

```
┌────────────────────────────────────────────────────────────────┐
│  Host (Linux + KVM)                                            │
│                                                                │
│  websandbox serve  ──── /run/websandbox.sock (HTTP API)        │
│       │                                                        │
│       │ POST /sandboxes                                        │
│       ▼                                                        │
│  ┌───────────────────────────┐  ┌───────────────────────────┐  │
│  │ microVM #1    172.16.0.10 │  │ microVM #2    172.16.0.11 │  │
│  │                           │  │                           │  │
│  │ Ubuntu 24.04 + Node 22    │  │ Ubuntu 24.04 + Node 22    │  │
│  │ Vite React-TS app  :5173  │  │ Vite React-TS app  :5173  │  │
│  │ sandboxd agent     :8090  │  │ sandboxd agent     :8090  │  │
│  └───────────┬───────────────┘  └───────────┬───────────────┘  │
│              │ fc0                          │ fc1              │
│              └──────────┬───────────────────┘                  │
│                       br-fc (bridge, NAT)                      │
│                                                                │
│  host:5200 → VM#1:5173        host:5201 → VM#2:5173            │
└────────────────────────────────────────────────────────────────┘
```

A single long-running server (`websandbox serve`) owns all VMs. Each sandbox gets its own tap device, guest IP, host port, and rootfs copy, allocated atomically from pools in a SQLite registry. Every VM runs `sandboxd`, a small in-guest agent that the host proxies to for command execution and file I/O — so `create` returns only once the sandbox is actually ready to use.

Firecracker provides hardware-level isolation (KVM) with ~125ms boot times and ~5MB memory overhead. Each sandbox gets its own kernel, filesystem, and network stack.

## Requirements

- Linux host with **KVM** support (`/dev/kvm` must exist)
- Root access (Firecracker requires it)
- ~4.5 GB disk for shared assets, plus one sparse rootfs copy per sandbox

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
sudo ./websandbox install-agent --agent ./sandboxd
```

Host networking (bridge, NAT, sysctls) is ensured automatically every time the server starts — no separate network setup step, and nothing to re-run after a reboot.

### 3. Start the server

```bash
sudo ./websandbox serve --config configs/devbox.json
```

On startup the server also reconciles state left over from a crash or reboot: orphaned firecracker processes are killed and stale taps, rootfs copies, DNAT rules, and registry rows are cleaned up.

### 4. Use sandboxes

```bash
sudo ./websandbox up
# sandbox 890691a8-… ready → http://localhost:5200

sudo ./websandbox list
sudo ./websandbox exec 890691a8 -- "node --version"
echo 'export const x = 1' | sudo ./websandbox write 890691a8 /home/sandbox/app/src/x.ts
sudo ./websandbox read 890691a8 /home/sandbox/app/src/x.ts
sudo ./websandbox ls 890691a8 /home/sandbox/app/src
curl http://localhost:5200          # the Vite app, live

sudo ./websandbox down 890691a8
sudo ./websandbox stop-server       # graceful: tears down all sandboxes
```

## CLI

```
websandbox serve          Run the API server (owns all VMs)
websandbox up             Create a sandbox; blocks until the agent is ready
websandbox down <id>      Destroy a sandbox
websandbox list           List running sandboxes
websandbox exec <id> -- <cmd>   Run a shell command inside a sandbox
websandbox read <id> <path>     Read a file from a sandbox to stdout
websandbox write <id> <path>    Write stdin (or --from file) into a sandbox
websandbox ls <id> [path]       List a directory inside a sandbox
websandbox install-agent  Bake/refresh sandboxd inside the base rootfs
websandbox stop-server    Stop the server (SIGTERM; --force for SIGKILL)
websandbox doctor         Validate the environment
```

`up`, `down`, `list`, `exec`, `read`, `write`, and `ls` are thin HTTP clients over the server's Unix socket.

## HTTP API

The server listens on a Unix socket (`/run/websandbox.sock`, mode 0600):

| Method & path | Description |
|---|---|
| `POST /sandboxes` | Create a sandbox; returns when the in-guest agent is healthy |
| `GET /sandboxes` | List running sandboxes |
| `GET /sandboxes/{id}` | Get one sandbox |
| `DELETE /sandboxes/{id}` | Graceful guest shutdown + resource cleanup |
| `POST /sandboxes/{id}/exec` | `{"cmd": "...", "cwd": "...", "timeout_sec": 60}` → `{stdout, stderr, exit_code, timed_out, duration_ms}` |
| `GET /sandboxes/{id}/files?path=` | Read a file (raw bytes) |
| `PUT /sandboxes/{id}/files?path=` | Write request body to a file (creates parent dirs) |
| `GET /sandboxes/{id}/dir?path=` | Directory listing (JSON) |

The exec/file endpoints are proxied to the `sandboxd` agent at `guestIP:8090` inside the VM.

## Configuration

Default config at `configs/devbox.json`. Anything omitted falls back to defaults:

| Field | Default | Description |
|-------|---------|-------------|
| `socket_path` | `/run/websandbox.sock` | API Unix socket |
| `db_path` | `/var/lib/websandbox/registry.db` | SQLite registry |
| `rootfs_base` | `/opt/fc/devbox-rootfs.ext4` | Immutable base image |
| `rootfs_dir` | `/var/lib/websandbox/rootfs` | Per-sandbox copies |
| `bridge` | `br-fc` | Host bridge for tap devices |
| `gateway_ip` | `172.16.0.1` | Bridge IP / guest default gateway |
| `guest_port` | `5173` | In-guest app port that gets forwarded |
| `pools.*` | taps `fc0-63`, IPs `.10-.73`, ports `5200-5263` | Resource pools |
| `vcpus`, `mem_mib` | 2, 1024 | Per-VM resources (template-wide) |
| `firecracker_bin`, `kernel_image`, `kernel_args` | … | VM template |

## Networking

```
Guest (172.16.0.x) ←──fcN──→ br-fc (172.16.0.1) ←──NAT──→ Internet
```

- **Guest → Internet**: iptables MASQUERADE through the host's default interface
- **Host → Guest**: direct via the bridge (this is how exec/files reach sandboxd)
- **External → Guest**: per-sandbox DNAT maps `host:520N` → `guestIP:5173`

Guest IPs are set via the kernel `ip=` boot parameter — no DHCP. The server ensures the bridge, sysctls (`ip_forward`, `route_localnet`), and NAT rules on every startup, so a host reboot needs nothing more than restarting `websandbox serve`.

## What's in the rootfs

The base rootfs is a 4 GB sparse ext4 image built by `scripts/build-devbox-rootfs.sh`:

| Layer | Details |
|-------|---------|
| **Base OS** | Ubuntu 24.04 (Noble) via debootstrap |
| **Runtime** | Node.js 22 LTS, npm, pnpm, TypeScript |
| **Project** | Vite React-TS template at `/home/sandbox/app`, `node_modules` pre-installed |
| **Services** | `vite-dev.service` (Vite on `0.0.0.0:5173`), `sandboxd.service` (agent on `:8090`) |
| **Debug** | Root password `devbox`, serial console on `ttyS0` |

Each sandbox boots from its own sparse copy of this image; writes never touch the base. The build script is resumable, and `websandbox install-agent` updates the agent in-place without a rebuild.

## Project structure

```
web-sandbox/
├── cmd/
│   ├── websandbox/          CLI + server entry point (cobra)
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
| `make build-linux` | Cross-compile `websandbox` + `sandboxd` for linux/amd64 |
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
