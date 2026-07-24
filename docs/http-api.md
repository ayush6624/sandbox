# HTTP API reference

Everything the server (and gateway) speaks, verified against the code. The
[TypeScript SDK](../sdk/typescript/README.md) wraps all of this; use the raw
API from other languages or shells.

## Base URL and auth

| Listener | Auth | Notes |
| --- | --- | --- |
| Unix socket `/run/sandbox.sock` | none (file mode 0600, root) | what the CLI uses on-host |
| TCP `--listen <ip>:8080` | `Authorization: Bearer <token>` | per-host API |
| Gateway `--listen <ip>:9090` | `Authorization: Bearer <token>` | fleet front door; same API |

All request/response bodies are JSON unless noted. Errors are always
`{"error": "message"}` with a 4xx/5xx status. There is no versioning yet.

Notes for browser frontends:

- **No CORS headers are set.** A browser can't call the TCP/gateway API
  cross-origin directly — put your own backend (or a same-origin proxy) in
  front and inject the bearer token there.
- **WebSocket auth**: browsers can't set an `Authorization` header on a
  WebSocket handshake, so upgrade requests (only) may carry the token as
  `?access_token=<token>` instead. Auth and routing failures on `/shell` are
  delivered as **post-handshake close frames** with code `4000 + HTTP status`
  (`4401` bad token, `4404` unknown sandbox, `4500` failed wake, `4502` agent
  unreachable) and the error message as the close reason — browsers surface
  these to the page, unlike a failed handshake (opaque `1006`).
- **Hibernation is transparent.** A sandbox with `"status": "hibernated"` is
  still fully addressable: any exec/files/dir/shell request — or a TCP
  connection to one of its forwarded ports — wakes it automatically (adds
  ~0.05–2 s to that first request). UIs should treat hibernated as "idle",
  not "stopped".

Against a **gateway**, every endpoint below behaves identically, plus fleet
extras — see [gateway differences](#gateway-differences).

## Objects

### Sandbox

```json
{
  "id": "2fdcea66-d551-417a-9877-77c586f0ea91",
  "name": "my devbox",
  "pid": 41823,
  "vm_id": "…",
  "socket_path": "/run/fc-….sock",
  "tap_device": "fc-tap3",
  "guest_ip": "172.16.0.10",
  "rootfs_path": "/opt/fc/rootfs/….ext4",
  "status": "running",
  "created_at": "2026-07-02T10:56:51Z",
  "expires_at": "2026-07-02T11:06:51Z",
  "hibernate_after_sec": 600,
  "vcpus": 4,
  "mem_mib": 4096,
  "base_snapshot_id": "…",
  "host_addr": "100.64.0.7"
}
```

- `name` — free-form display label (set at create or via `/rename`); omitted
  when unnamed. Not unique, not a lookup key.
- `status` — `"running"` or `"hibernated"` (`"stopping"` is a transient
  internal state you normally won't see).
- `expires_at` — omitted when there's no TTL.
- `vcpus`, `mem_mib` — **always present**: the effective resources the
  sandbox runs with. When no override was given at create, the host
  template's defaults are filled in (compare against `GET /info`).
- `hibernate_after_sec`, `base_snapshot_id` — omitted when zero/empty
  (zero = host default).
- `host_addr` — **gateway only**: the owning host's address. Pair it with a
  `host_port` returned by the ports API (not the gateway address).
- `pid`, `vm_id`, `socket_path`, `tap_device`, `rootfs_path` are host-side
  internals; frontends can ignore them.

### Snapshot

```json
{
  "id": "b72c5f9e-…",
  "name": "deps-installed",
  "source_id": "2fdcea66-…",
  "tap_device": "fc-tap3",
  "guest_ip": "172.16.0.10",
  "mem_path": "…", "state_path": "…", "rootfs_path": "…", "source_rootfs_path": "…",
  "created_at": "2026-07-02T11:00:00Z",
  "golden": true,
  "format": "diff",
  "base_id": "…",
  "vcpus": 4,
  "mem_mib": 4096
}
```

- `name` — free-form display label (set at snapshot time or via `/rename`);
  omitted when unnamed.
- `golden` — the server-managed pristine snapshot that hot creates clone
  from (at most one; omitted when false). You generally want to hide it or
  badge it in a UI — deleting it just makes creates cold-boot until the next
  server restart.
- `format` — `"full"` or `"diff"` (delta against `base_id`); absent = full.
- `vcpus`/`mem_mib` — resources baked into the snapshot (omitted = template
  default); restores and clones run with these.
- The `*_path` fields are host-side internals.

### PortMapping

```json
{"guest_port": 8000, "host_port": 5201}
```

## Host info

### `GET /info`

Template defaults and per-sandbox override limits — what a sandbox gets when
created without `vcpus`/`mem_mib`, and the accepted override bounds. Lets a
UI label resources without guessing.

```json
{
  "default_vcpus": 2,
  "default_mem_mib": 1024,
  "max_vcpus": 16,
  "max_mem_mib": 64312,
  "hot_create": true,
  "hibernate_after_sec": 600,
  "host_id": "testvm-1"
}
```

- `hibernate_after_sec` — the host-wide idle default (0 = off);
  per-sandbox overrides still apply.
- `host_id` — omitted when the host isn't fleet-registered.

Against a **gateway**, `/info` is answered by one live host (fleet hosts
share a template config); `503` when no host is live.

### `GET /metrics`

Prometheus text exposition format (`text/plain; version=0.0.4`) — this host's
occupancy and lifecycle counters. Behind the same bearer auth as the rest of
the TCP listener; auth-free over the Unix socket. (The **gateway** exposes its
own fleet-aggregate `/metrics` — see [Gateway differences](#gateway-differences);
this is the per-host detail a heartbeat elides.)

Gauges:

- `sandbox_running`, `sandbox_hibernated` — sandbox counts by state.
- `sandbox_slots_free` — allocatable slots right now (smallest per-pool
  availability, memory-bounded); what the heartbeat advertises.
- `sandbox_pool_used{pool="tap|ip|port"}` / `sandbox_pool_total{pool=…}` —
  per-pool occupancy. Taps/IPs are held by running sandboxes only; ports are
  held by explicit mappings, including while their sandbox is hibernated.
- `sandbox_committed_mem_mib` / `sandbox_mem_budget_mib` — committed guest
  memory vs the admission ceiling (`0` = admission disabled).
- `sandbox_golden_ready` — `1` when the golden snapshot is staged (hot create
  available).
- `sandbox_create_inflight` / `sandbox_create_concurrency` — bring-ups
  currently holding a create slot vs the semaphore size.
- `sandbox_uptime_seconds` — since process start.

Counters (reset only on restart): `sandbox_creates_ok_total`,
`sandbox_creates_error_total`, `sandbox_hibernations_total`,
`sandbox_wakes_total`, `sandbox_wake_failures_total`.

## Sandboxes

### Create — `POST /sandboxes`

Body (optional — empty body is fine):

```json
{"name": "my devbox", "timeout_sec": 3600, "hibernate_after_sec": 600, "vcpus": 4, "mem_mib": 4096,
 "ssh_pubkey": "ssh-ed25519 AAAAC3Nza… me@laptop"}
```

- `name` — display label, at most 64 bytes, no control characters;
  ""/omit = unnamed. Change it later via `/rename`.
- `timeout_sec` — auto-destroy TTL in seconds; 0/omit = no expiry.
- `hibernate_after_sec` — idle-hibernation override: >0 custom window,
  `-1` never hibernate, 0/omit = host default.
- `vcpus` / `mem_mib` — per-sandbox resource overrides; 0/omit = the host
  template's defaults. Bounds-checked (400 on negative, more vcpus than host
  cores, mem below 128 MiB or above host RAM).
- `ssh_pubkey` — a single OpenSSH public key line, installed as
  `/root/.ssh/authorized_keys` in the guest for key-only root SSH
  (`400` on multi-line input or an unknown key type). Unlike other
  create-time extras this is **not** best-effort: if the key can't be
  installed the sandbox is destroyed and the create fails, so a sandbox
  handed back with SSH requested is always reachable. The key lives in the
  rootfs, so it survives hibernation/wake with no re-push. Reach it by
  exposing guest port 22 (`POST /sandboxes/{id}/ports`) and connecting to
  the returned `host_port` — the port proxy carries wake-on-connect, so SSH
  to a hibernated sandbox wakes it. **Fleet caveat:** the gateway proxies
  HTTP only, so fleet SSH needs a `ProxyJump` through the owning worker
  (`host_addr`).

Returns `201 Sandbox`. Blocks until the sandbox's in-guest agent is healthy,
so the sandbox is usable the moment this returns. Served from a pre-booted
golden snapshot when available (~0.3–0.5 s), cold boot otherwise (~2–3.5 s).
**A `vcpus`/`mem_mib` override always cold-boots** — Firecracker bakes
resources into the golden snapshot, so an override can't be served from it.
Give the request a generous client timeout — the SDK uses 300 s, which has to
exceed the gateway's create queue-wait (180 s default) plus bring-up, or a
burst abandons creates the queue would have served.

#### Idle-hibernation activity contract

The shipped host configs hibernate after 10 minutes without observable external
activity. Agent API requests (exec, files, directories), an open shell, and
connections through explicitly forwarded ports count as activity. In-flight
requests and open connections pin the sandbox; its idle clock restarts when the
last one closes.

Processes running only inside the guest do not count as activity. The server
does not infer intent from process lists, CPU, disk I/O, or outbound network
traffic. A detached `tmux` job can therefore be hibernated; its memory and
process state resume on the next API, shell, or forwarded-port connection, but
external TCP sessions may have timed out. Set `hibernate_after_sec: -1` for
unattended workloads that must remain continuously running.

Idle hibernation is independent of `timeout_sec`: hibernation is recoverable,
whereas the TTL destroys the sandbox when it expires, whether it is running or
hibernated.

### List — `GET /sandboxes` → `200 [Sandbox…]`

Includes both `running` and `hibernated` sandboxes (a hibernated sandbox is
still addressable). Empty list is `[]`, never `null`.

### Get — `GET /sandboxes/{id}` → `200 Sandbox` | `404`

### Destroy — `DELETE /sandboxes/{id}` → `204` | `404`

Graceful in-guest shutdown, then full resource cleanup (VM, tap, IP, ports,
disk). Works on hibernated sandboxes too.

### Reset TTL — `POST /sandboxes/{id}/timeout`

Body: `{"timeout_sec": N}`. Replaces the TTL counting from now; `0` clears it.
Returns `200` with the updated Sandbox. A reaper destroys expired sandboxes
within ~10 s (running **and** hibernated).

### Rename — `POST /sandboxes/{id}/rename`

Body: `{"name": "new name"}`. Sets the display name; `""` clears it. Returns
`200` with the updated Sandbox. Same validation as at create (≤ 64 bytes, no
control characters → `400`).

### Hibernate — `POST /sandboxes/{id}/hibernate`

Freezes a running sandbox to disk immediately (memory snapshot + VM kill);
the same thing the idle reaper does automatically after
`hibernate_after_sec`. Returns `200` with the updated Sandbox
(`"status": "hibernated"`). `409` if it isn't running on this server.

Waking is implicit — there is no `/wake` endpoint. The next agent-bound
request (exec/files/dir/shell) or connection to a forwarded port resumes it
(~50 ms typical, up to ~2 s on the clone path).

## Commands

### Exec — `POST /sandboxes/{id}/exec`

```json
{"cmd": "pnpm install", "cwd": "/home/sandbox/app", "env": {"CI": "true"}, "timeout_sec": 120}
```

- `cmd` runs via `bash -lc`, as root, in the sandbox.
- `cwd` defaults to `/home/sandbox/app`; `env` is appended to the agent's env.
- `timeout_sec` defaults to 60. On timeout the whole process group is killed
  and `timed_out` is true.
- stdout/stderr are each capped at 2 MiB.
- The request returns when the **shell** exits — background children
  (`my-server & sleep 0.5`) keep running inside the sandbox afterwards; their
  output is captured only until shortly after the shell exits.

```json
200 {"stdout": "…", "stderr": "", "exit_code": 0, "timed_out": false, "duration_ms": 288}
```

Note: a non-zero exit code is still HTTP 200 — check `exit_code`.

### Streaming exec — `POST /sandboxes/{id}/exec/stream`

Same request body. Response is **NDJSON** (one JSON object per line, flushed
as produced — not SSE, so read the response body incrementally):

```
{"type":"stdout","data":"installing…\n"}
{"type":"stderr","data":"warning: …\n"}
{"type":"exit","exit_code":0,"duration_ms":41230}
```

Exactly one `exit` event ends the stream. All fields except `type` are
omitted when zero-valued — treat absent fields as `0`/`""`/`false`
(a clean exit may arrive as just `{"type":"exit","duration_ms":12}`).

### Interactive shell — `GET /sandboxes/{id}/shell?cols=120&rows=32&cwd=/home/sandbox`

WebSocket upgrade to a real `bash -l` on a pty. This is a **supported client
API**, not an internal detail. Query params set the initial size (defaults
80×24) and working directory; browser clients append `&access_token=<token>`
(headers can't be set on a WebSocket).

- **Binary frames**: raw terminal bytes, both directions (guest→client is
  stdout+stderr combined; client→guest is stdin).
- **Text frames** (client→server): `{"type":"resize","cols":120,"rows":32}`.
- Clean shell exit closes the socket with close reason `exit:<code>` — parse
  the trailing integer.
- **Errors close with code `4000 + HTTP status`** and the message as the
  close reason: `4401` bad token, `4404` unknown sandbox, `4500` failed
  wake, `4502` agent unreachable. The handshake itself succeeds first, so
  these reach browser `onclose` handlers instead of collapsing into `1006`.
- Client disconnect kills the shell's process group.
- An open shell pins the sandbox running (it won't idle-hibernate or be
  considered idle while connected).

`sandbox shell <id>` in the CLI and `sandbox.pty` in the SDK are ready-made
clients; the protocol pairs naturally with xterm.js in a browser.

## Files

### Read — `GET /sandboxes/{id}/files?path=/abs/path`

`200` with the raw file bytes (`application/octet-stream`, `Content-Length`
set). `400` if `path` is missing or a directory; `404` if missing.

### Write — `PUT /sandboxes/{id}/files?path=/abs/path`

Request body = file content (raw bytes). Creates parent directories,
truncates any existing file (mode 0644).

```json
201 {"path": "/abs/path", "bytes": 123}
```

### List directory — `GET /sandboxes/{id}/dir?path=/abs/path`

`path` defaults to `/home/sandbox/app`.

```json
200 [{"name":"app","size":4096,"mode":"drwxr-xr-x","is_dir":true,"mtime":"2026-07-02T10:00:00Z"}, …]
```

## Ports

### Expose — `POST /sandboxes/{id}/ports`

Body: `{"guest_port": 8000}` (1–65535). Forwards the guest port to a
pool-allocated host port. Idempotent — re-exposing the same guest port returns
the existing mapping.

```json
200 {"guest_port": 8000, "host_port": 5201}
```

Works on a hibernated sandbox without waking it — the new port is just
another wake-on-connect entry point.

### List — `GET /sandboxes/{id}/ports`

`200 [PortMapping…]` — all explicitly forwarded ports. A newly created
sandbox returns `[]`.

## Snapshots

A snapshot is a complete capture of a running sandbox: memory, processes, and
disk. See [Concepts](concepts.md#snapshots-restore-and-fan-out) for the
prepare-once/clone-many workflow.

### Take — `POST /sandboxes/{id}/snapshot`

Body (optional): `{"name": "deps-installed"}` — a display label for the
snapshot. Pauses the sandbox (~1 s), writes memory + device state + a frozen
disk copy, resumes it. The source keeps running. Returns `201 Snapshot`.
`409` if the sandbox isn't running on this server (e.g. hibernated).

### Restore (1:1) — `POST /snapshots/{id}/restore`

Body (optional): `{"name": "…", "timeout_sec": N, "hibernate_after_sec": N}`.
Boots a **new** sandbox resuming the snapshot exactly — same processes, same memory.
Reuses the snapshot's baked network identity, so: the source sandbox must be
dead, and at most one restore of a given snapshot can run at a time (`409` on
conflict). Returns `201 Sandbox`.

`vcpus`/`mem_mib` are rejected with `400`: resources are baked into the
snapshot when it is taken — a restored sandbox always runs (and reports)
the source's resources.

### Fan-out (1:N) — `POST /snapshots/{id}/fanout`

Body: `{"count": 32, "timeout_sec": 600, "hibernate_after_sec": N}`
(`count` >= 1 required). Starts N identity-neutral clones concurrently — each
with a fresh IP and copy-on-write disk. Returns `201 [Sandbox…]` with
every clone that came up (**partial success possible** — the array may be
shorter than `count`; failures are logged server-side and their resources
reclaimed). `500` only if every clone failed. `vcpus`/`mem_mib` are rejected
with `400`, same as restore.

### Rename — `POST /snapshots/{id}/rename`

Body: `{"name": "new name"}`. Sets the snapshot's display name; `""` clears
it. Returns `200` with the updated Snapshot.

### List — `GET /snapshots` → `200 [Snapshot…]`
### Delete — `DELETE /snapshots/{id}` → `204` | `404`

Removes the snapshot record, its on-disk artifacts, and (when GCS durability
is on) its bucket objects in the background.

## Gateway differences

The gateway fronts N hosts with the same API, plus:

| | |
| --- | --- |
| `GET /info` | Forwarded to one live host (fleet hosts share a template config); `503` when none is live |
| `GET /hosts` | Fleet state: `[{"id","addr","slots_total","slots_used","hibernated","free","alive","last_seen_ms_ago"}]` |
| `GET /metrics` | Prometheus text format: `sandbox_hosts_live`, `sandbox_slots_total/used/free`, `sandbox_create_queue_depth`, per-host gauges |
| `GET /metrics/hosts` | Federated per-host metrics: the gateway scrapes each live host's `/metrics` (using the addr+token from its heartbeat) and re-exports every series with a `host="<id>"` label, grouped into valid exposition. `sandbox_host_scrape_ok{host}` flags any host it couldn't reach. Lets Prometheus collect per-host detail while scraping only the gateway — no worker service discovery |
| `POST /sandboxes` | Bin-packed onto the fullest live host with a free slot. When the fleet is full the request **waits in a bounded queue**; if it can't be placed it fails `503` with `Retry-After: 5` — retry with backoff. `502` if the chosen host errored |
| `GET /sandboxes` | Merged across all hosts |
| `/sandboxes/{id}/…` | Proxied to the owning host (includes exec, exec/stream, files, dir, `/shell`, ports, `/snapshot`, `/hibernate`); unknown id → `404` |
| `host_addr` | Sandbox objects gain `"host_addr"`: the owning host's address. Pair it with the `host_port` from an explicit port mapping (not the gateway); the SDK does this |
| `GET /snapshots` | Merged + deduped across live hosts |
| `POST /snapshots/{id}/restore` / `/fanout`, `DELETE /snapshots/{id}` | Forwarded to the owning host; if that host is dead/unknown, any live host serves it by pulling the snapshot from GCS (returns `503` if no live host) |

## Errors and limits

| Status | Meaning |
| --- | --- |
| 400 | malformed body (`timeout_sec < 0`, `count < 1`, out-of-bounds `vcpus`/`mem_mib`, resource overrides on restore/fanout, missing `path`, bad JSON) |
| 401 | missing/invalid bearer token (TCP/gateway listeners only) |
| 404 | unknown sandbox/snapshot/file |
| 409 | restore conflict (source or a prior restore still running); snapshot/hibernate of a sandbox not running on this server |
| 502 | in-guest agent unreachable; gateway → host proxy failure |
| 503 | gateway only: no live host with capacity (create queue timed out; `Retry-After` set) |
| 500 | provisioning failure (host out of pool slots, VM boot failure, failed wake, …) |

Capacity: each host has fixed tap/IP pools (64 sandboxes by default) and a
separate host-port pool (5200–5263) for explicit mappings. Hibernated
sandboxes release their tap/IP slot but retain explicitly exposed ports for
wake-on-connect. Exhausting the port pool prevents further exposure but does
not prevent sandbox creation.
