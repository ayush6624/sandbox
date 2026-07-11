# HTTP API reference

Everything the server (and gateway) speaks. The [TypeScript SDK](../sdk/typescript/README.md)
wraps all of this; use the raw API from other languages or shells.

## Base URL and auth

| Listener | Auth | Notes |
| --- | --- | --- |
| Unix socket `/run/sandbox.sock` | none (file mode 0600, root) | what the CLI uses on-host |
| TCP `--listen <ip>:8080` | `Authorization: Bearer <token>` | per-host API |
| Gateway `--listen <ip>:9090` | `Authorization: Bearer <token>` | fleet front door; same API |

All request/response bodies are JSON unless noted. Errors are
`{"error": "message"}` with a 4xx/5xx status. There is no versioning yet.

Against a **gateway**, every endpoint below behaves identically except the
snapshot collection routes — see [gateway differences](#gateway-differences).

## Sandboxes

### Create — `POST /sandboxes`

Body (optional): `{"timeout_sec": 600, "vcpus": 4, "mem_mib": 4096}`

- `timeout_sec` — auto-destroy TTL in seconds; omit for no expiry.
- `hibernate_after_sec` — idle-hibernation override (>0 custom window,
  -1 never, 0/omit = host default).
- `vcpus` / `mem_mib` — per-sandbox resource overrides; 0/omit = the host
  template's defaults. Bounds-checked against the host (400 on negative,
  more vcpus than host cores, mem below 128 MiB or above host RAM).

Blocks until the sandbox's in-guest agent is healthy, so the sandbox is usable
the moment this returns. Served from a pre-booted golden snapshot when
available (~0.5 s), cold boot otherwise (~2-3.5 s). **A `vcpus`/`mem_mib`
override always cold-boots** — Firecracker bakes resources into the golden
snapshot, so an override can't be served from it. Give the request a
generous client timeout (the SDK uses 90 s).

```json
201 {
  "id": "2fdcea66-d551-417a-9877-77c586f0ea91",
  "guest_ip": "172.16.0.10",
  "host_port": 5200,
  "status": "running",
  "created_at": "2026-07-02T10:56:51Z",
  "expires_at": "2026-07-02T11:06:51Z"
}
```

`host_port` is the pre-forwarded mapping to guest port 3000 — reach an
in-guest server at `<api-host>:<host_port>`.

### List — `GET /sandboxes` → `200 [Sandbox…]`
### Get — `GET /sandboxes/{id}` → `200 Sandbox` | `404`
### Destroy — `DELETE /sandboxes/{id}` → `204`

Graceful in-guest shutdown, then full resource cleanup (VM, tap, IP, ports, disk).

### Reset TTL — `POST /sandboxes/{id}/timeout`

Body: `{"timeout_sec": N}`. Replaces the TTL counting from now; `0` clears it.
Returns the updated sandbox. A reaper destroys expired sandboxes within ~10 s.

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
as produced — not SSE):

```
{"type":"stdout","data":"installing…\n"}
{"type":"stderr","data":"warning: …\n"}
{"type":"exit","exit_code":0,"duration_ms":41230}
```

Exactly one `exit` event ends the stream. All fields except `type` are
omitted when zero-valued — treat absent fields as `0`/`""`/`false`
(a clean exit may arrive as just `{"type":"exit","duration_ms":12}`).

### Interactive shell — `GET /sandboxes/{id}/shell?cols=120&rows=32&cwd=/home/sandbox`

WebSocket upgrade to a real `bash -l` on a pty.

- **Binary frames**: raw terminal bytes, both directions.
- **Text frames** (client→server): `{"type":"resize","cols":120,"rows":32}`.
- Clean shell exit closes the socket with close reason `exit:<code>`.
- Client disconnect kills the shell's process group.

`sandbox shell <id>` in the CLI is a ready-made client.

## Files

### Read — `GET /sandboxes/{id}/files?path=/abs/path`

`200` with the raw file bytes (`application/octet-stream`). `404` if missing.

### Write — `PUT /sandboxes/{id}/files?path=/abs/path`

Request body = file content (raw bytes). Creates parent directories.
Returns `200 {"path": "...", "size": 123}`.

### List directory — `GET /sandboxes/{id}/dir?path=/abs/path`

```json
200 [{"name":"app","size":4096,"mode":"drwxr-xr-x","is_dir":true,"mtime":"2026-07-02T10:00:00Z"}, …]
```

## Ports

### Expose — `POST /sandboxes/{id}/ports`

Body: `{"guest_port": 8000}`. Forwards the guest port to a pool-allocated host
port. Idempotent — re-exposing returns the existing mapping.

```json
201 {"guest_port": 8000, "host_port": 5201}
```

### List — `GET /sandboxes/{id}/ports`

All forwarded ports, always including the primary `3000 → host_port` mapping.

## Snapshots

A snapshot is a complete capture of a running sandbox: memory, processes, and
disk. See [Concepts](concepts.md#snapshots-restore-and-fan-out) for the
prepare-once/clone-many workflow.

### Take — `POST /sandboxes/{id}/snapshot`

Pauses the sandbox (~1 s), writes memory + device state + a frozen disk copy,
resumes it. The source keeps running.

```json
201 {"id": "b72c5f9e-…", "source_id": "2fdcea66-…", "created_at": "…"}
```

### Restore (1:1) — `POST /snapshots/{id}/restore`

Body (optional): `{"timeout_sec": N}`. Boots a **new** sandbox resuming the
snapshot exactly — same processes, same memory. Reuses the snapshot's baked
network identity, so: the source sandbox must be dead, and at most one restore
of a given snapshot can run at a time (`409` on conflict). Returns a Sandbox.

`vcpus`/`mem_mib` are rejected with `400`: resources are baked into the
snapshot when it is taken — a restored sandbox always runs (and reports)
the source's resources.

### Fan-out (1:N) — `POST /snapshots/{id}/fanout`

Body: `{"count": 32, "timeout_sec": 600}`. Starts N identity-neutral clones
concurrently — each with a fresh IP/ports and copy-on-write disk. Returns
`201 [Sandbox…]` with every clone that came up (partial success possible;
failures are logged server-side and their resources reclaimed).
`vcpus`/`mem_mib` are rejected with `400`, same as restore.

### List — `GET /snapshots` → `200 [Snapshot…]`
### Delete — `DELETE /snapshots/{id}` → `204`

Removes the snapshot record and its on-disk artifacts.

## Gateway differences

The gateway fronts N hosts with the same API, plus:

| | |
| --- | --- |
| `GET /hosts` | Fleet state: `[{"id","addr","slots_total","free","alive","last_seen_ms_ago"}]` |
| `POST /sandboxes` | Placed on the least-loaded live host |
| `GET /sandboxes` | Merged across all hosts |
| `/sandboxes/{id}/…` | Proxied to the owning host (includes exec, files, `/shell`, `/snapshot`) |
| `host_addr` | Sandbox objects gain `"host_addr"`: the owning host's address. Use it (not the gateway's) to reach forwarded ports — the SDK's `getHost()` does this automatically |
| `/snapshots/*` | **Not routed** — restore/fan-out/list/delete are host-local; call the owning host directly |

## Errors and limits

| Status | Meaning |
| --- | --- |
| 400 | malformed body (`timeout_sec < 0`, `count < 1`, out-of-bounds `vcpus`/`mem_mib`, resource overrides on restore/fanout, bad JSON) |
| 401 | missing/invalid bearer token (TCP/gateway listeners only) |
| 404 | unknown sandbox/snapshot/file |
| 409 | restore conflict (source or a prior restore still running); snapshot of a sandbox not running on this server |
| 500 | provisioning failure (host out of pool slots, VM boot failure, …) |

Capacity: each host has fixed pools (default 64 sandboxes: taps, IPs, host
ports 5200-5263). When the pool is exhausted, creates fail with 500 — size
your fleet or TTLs accordingly.
