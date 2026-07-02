# Self-hosting

Run the sandbox system on your own hardware — one host, or a fleet behind a
gateway. Everything is a single static Go binary plus a handful of idempotent
scripts.

## Requirements

- **Linux with KVM** (`/dev/kvm` must exist). Bare metal, or a cloud VM with
  nested virtualization (GCP: `--enable-nested-virtualization` on N2/C2/C3;
  AWS: `.metal`; Hetzner bare metal works out of the box).
- Root (Firecracker needs it).
- Disk: ~6 GB for shared assets. For fast snapshots/fan-out, put the data
  directories on **XFS or btrfs** — `cp --reflink` makes rootfs copies
  instant. On ext4 everything still works, just with ~1 s copies.
- Go toolchain on your workstation (cross-compiles from macOS: `make build-linux`).

## Single host

### 1. Build and sync

```bash
git clone https://github.com/ayush6624/sandbox.git && cd sandbox
make sync REMOTE_HOST=your-server        # builds linux/amd64 + rsyncs to ~/sandbox
```

### 2. One-time host setup

```bash
ssh you@your-server
cd ~/sandbox
sudo bash scripts/setup-firecracker.sh      # install the firecracker binary
sudo bash scripts/setup-kernel.sh           # download the guest kernel

# Base image: build it (~5 min, resumable)…
sudo apt-get install -y debootstrap
sudo bash scripts/build-devbox-rootfs.sh
# …or pull the prebuilt one (~1.5 GB download, checksum-verified):
sudo bash scripts/fetch-rootfs.sh https://sandbox.ayushgoyal.dev/images/devbox-rootfs.tar.zst

sudo ./sandbox install-agent --agent ./sandboxd   # bake the guest agent in
sudo ./sandbox doctor                             # validate everything
```

There is no network setup step: the server idempotently creates the bridge,
sysctls, and NAT rules on every startup. A host reboot just needs the server
restarted.

### 3. Serve

```bash
# Local-only (Unix socket):
sudo ./sandbox serve --config configs/devbox.json

# Also over TCP with bearer auth (for the SDK / remote clients):
sudo ./sandbox serve --listen <private-ip>:8080 --token $(openssl rand -hex 24)
```

On startup the server also:

- **reconciles** — kills orphaned VM processes and reclaims taps/disk/ports
  left by a crash or reboot;
- **builds the golden snapshot** (~6 s) — boots a pristine sandbox, snapshots
  it, destroys it. All subsequent creates clone this snapshot in ~200-500 ms
  instead of cold-booting. Disable with `"disable_hot_create": true` if you
  ever need to.

Run it under systemd for production (`Restart=always`; see
`infra/gcp/fleet-deploy.sh` for a ready-made unit).

### Updating

```bash
make sync REMOTE_HOST=your-server                  # new binaries
ssh you@your-server 'cd ~/sandbox && sudo ./sandbox install-agent --agent ./sandboxd'
# restart the server — it rebuilds the golden snapshot from the new base image
```

`install-agent` changes the base image, which automatically invalidates the
golden snapshot on the next restart. If you skip the restart, creates keep
working from the old snapshot.

## Multi-host fleet

Scale horizontally by running a **gateway** in front of N identical hosts.
Each host keeps its own state; the gateway holds none (it rebuilds routing
from heartbeats, so it can restart freely).

```bash
# On the gateway machine:
./sandbox gateway --listen <gw-ip>:9090 --token <GATEWAY_TOKEN>

# On every host:
sudo ./sandbox serve --config configs/devbox.json \
  --listen <host-ip>:8080 --token <HOST_TOKEN> \
  --gateway http://<gw-ip>:9090 --gateway-token <GATEWAY_TOKEN> \
  --advertise <host-ip>:8080
```

Hosts heartbeat every 5 s; `GET /hosts` on the gateway shows who's alive.
Clients point at the gateway and never think about hosts again
(`SANDBOX_API_URL=http://<gw-ip>:9090`).

Adding a host = set up the machine (steps above) + start `serve --gateway …`.
It registers itself; no gateway config, no restart.

**Reference implementation:** `infra/gcp/` automates the whole thing on GCP —
`vms.sh` creates spot VMs (nested virt + a 1 TB XFS data disk),
`fleet-deploy.sh` bootstraps every host in parallel and installs systemd units
for gateway + serve, generating stable tokens into `fleet-secrets.env`.
`fleet-deploy.sh status|restart` for day-2 ops.

## Network exposure

- The TCP listeners have bearer auth but **no TLS**. Bind them to a private
  network only — a VPN/tailnet (Tailscale is what the reference fleet uses),
  a VPC, or localhost. Never a public interface.
- The Unix socket is mode 0600 (root) and needs no token.
- Sandbox `host_port`s (5200-5263) bind on the host; firewall them to the
  audience your in-guest servers expect.

## Configuration reference

`configs/devbox.json`; every field has a sensible default. CLI flags override
the file.

| Field | Default | Description |
| --- | --- | --- |
| `socket_path` | `/run/sandbox.sock` | API Unix socket |
| `listen_addr` | — | optional TCP listener (`ip:port`); requires `api_token` |
| `api_token` | — | bearer token for the TCP listener |
| `gateway_url` / `gateway_token` | — | register + heartbeat to a gateway |
| `advertise_addr` | `listen_addr` | address the gateway dials back |
| `host_id` | hostname | stable identity reported to the gateway |
| `db_path` | `/var/lib/sandbox/registry.db` | SQLite registry |
| `rootfs_base` | `/opt/fc/devbox-rootfs.ext4` | immutable base image |
| `rootfs_dir` | `/var/lib/sandbox/rootfs` | per-sandbox disks (put on XFS for reflink) |
| `snapshot_dir` | `/var/lib/sandbox/snapshots` | snapshot artifacts (same FS as `rootfs_dir` for reflink) |
| `disable_hot_create` | `false` | `true` = always cold-boot creates |
| `bridge` / `gateway_ip` | `br-fc` / `172.16.0.1` | host bridge and guest default gateway |
| `nameservers` | `8.8.8.8` | guest DNS (comma-separated; match your network's egress rules) |
| `guest_port` | `3000` | in-guest port pre-forwarded at create |
| `pools.*` | taps `fc0-63`, IPs `.10-.73`, ports `5200-5263` | capacity: 64 sandboxes/host |
| `vcpus` / `mem_mib` | `2` / `1024` | per-VM resources (template-wide) |
| `firecracker_bin` / `kernel_image` / `kernel_args` | … | VM template |

## Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| `sandbox doctor` fails on KVM | nested virtualization off, or not a KVM-capable machine type |
| Every create fails "agent never became ready" | base image has no agent — run `install-agent` |
| Creates are slow (~3 s) | no golden snapshot yet (just restarted?) or `disable_hot_create` on; check server log for `golden snapshot … creates are hot` |
| `curl localhost:<host_port>` hangs | nothing listening on guest `:3000`, or the host-side NAT rules were flushed — restart the server (it re-ensures networking) |
| Creates fail with 500 pool errors | host at capacity (64) — add hosts or shorten TTLs |
| Fan-out slow on this host | disks on ext4 — move `rootfs_dir` + `snapshot_dir` to XFS |

Server logs go to stderr (journalctl when under systemd). Per-VM Firecracker
logs land in `firecracker-<vmid>.log` in the server's working directory.
