# HTTP API v1

`api/openapi.yaml` is the source of truth for the public HTTP contract. The
fleet gateway and a standalone worker both serve the same `/v1` resources.
Routes without `/v1` remain available as a compatibility adapter for the
existing TypeScript SDK and operational scripts.

## Vocabulary

The public API uses resource-oriented names:

- A **sandbox** is one isolated development environment.
- A **template** is a declarative, reproducible base environment. `default` is
  the template currently supplied by each worker image.
- A **snapshot** is an immutable capture of a sandbox's runtime state.
- A **sandbox batch** creates several independent sandboxes and returns an
  **operation** that records every indexed success or error.
- A **port forward** publishes a guest TCP port on its worker host.

The legacy word `fanout` describes an internal Firecracker optimization. Public
clients create a sandbox batch whose sandbox source is a snapshot; the runtime
may use that optimization without exposing it in the contract.

## Request behavior

All responses include `X-Request-Id`. Clients may supply a valid request ID for
log correlation or allow the server to generate one.

Every public mutation requires `Idempotency-Key`. A key is scoped to the HTTP
method and resource path. Repeating an identical request replays the original
status, headers, and body and adds `Idempotency-Replayed: true`. Reusing a key
with another body returns `409 idempotency_key_reused`.

Single sandbox creates and sandbox batches persist their request identity and
results across gateway or worker process restart with the database intact.
Their replay records are retained indefinitely for now. Other mutations retain
the process-local cache for up to 24 hours. Create replay returns the historical
result even after the sandbox is deleted; it never creates a replacement.
See [create operation recovery](create-operation-recovery.md) for assignment,
failure, and storage boundaries.

New durable creates retain the original `X-Request-Id` with acceptance. Their
error bodies and replay responses carry that original identifier; batch item
errors also retain it when read through a later operation poll. The poll itself
has its own HTTP request ID. Gateway logs connect the accepted request ID to the
operation ID. Historical operations accepted before this field was recorded can
still have an empty identifier; their stored responses are not rewritten.

Errors use RFC 9457 `application/problem+json`. The standard members are
extended with a stable `code`, the response `request_id`, and optional field
`violations`. Clients should branch on `code`, not human-readable `detail`.

Collection methods return resource-shaped envelopes such as
`{"sandboxes":[...],"next_page_token":"..."}`. `page_token` is opaque and must
be returned unchanged. The default page size is 50 and the maximum is 100.

Operation pages return newest creations first, with operation ID breaking equal
timestamps. Their continuation token anchors to the last returned operation, so
newer creates between requests do not repeat items on later pages. Each page is
a fresh read; operation status can change between requests. Existing numeric
operation tokens remain accepted, and new responses return the current opaque
token format. Listing decodes only the requested operations.

## Creation sources

`POST /v1/sandboxes` waits for a single-sandbox create result.
`POST /v1/sandbox-creations` accepts the same request body and returns a durable
operation handle with HTTP 202. Both methods use the same creation sources:

```json
{
  "name": "test-shard-1",
  "source": {"type": "snapshot", "id": "snap_123"},
  "lifecycle": {"ttl_seconds": 600, "idle_timeout_seconds": 300},
  "resources": {"vcpu": 2, "memory_mib": 1024},
  "metadata": {"run_id": "ci_456", "shard": "1"}
}
```

Supported sources are `default`, `template` with ID `default`, and `snapshot`
with a snapshot ID. Runtime placement, process IDs, tap devices, guest
addresses, socket paths, rootfs paths, and artifact paths are deliberately not
part of public objects.

A missing or deleted snapshot/template source returns `404 source_not_found`.
Batch creates retain that error for each failed item. Repeating the create with
the same idempotency key replays the recorded failure. Storage corruption and
an unavailable peer whose snapshot is not yet durable remain server failures;
they do not establish that the source is absent.

## Lifecycle

- `POST /v1/sandboxes/{id}:pause` preserves the sandbox identity while
  releasing runtime capacity.
- `POST /v1/sandboxes/{id}:resume` restores the same sandbox resource.
- `PATCH /v1/sandboxes/{id}` changes its name, metadata, TTL, or idle timeout.
- `DELETE /v1/sandboxes/{id}` permanently deletes it.

Snapshot creation is `POST /v1/sandboxes/{id}/snapshots`. A snapshot can be
used by many independent creates while its source sandbox continues running.

The snapshot's `state` is `local` until its object-store metadata commits, then
`durable`. When object storage is configured, the capturing worker commits an
upload job with the snapshot row and resumes it after a restart with its disk
intact. The optional `upload` object exposes `state`, `attempts`,
`next_attempt_at`, and `error`. Upload states are `pending`, `uploading`,
`retrying`, and `failed`; successful completion removes the upload object.
Transient storage failures retry automatically. Missing local artifacts fail
with an inspectable error. Copies fetched from another worker do not acquire
that worker's upload job.

TypeScript clients can call `client.snapshots.waitForDurable(id, options)`.
Timeout and cancellation stop the wait while the background upload continues.
See [snapshot recovery](snapshot-upload-recovery.md) for the persistence and
deletion boundaries.

## Batch creation

`POST /v1/sandbox-batches` accepts `count`, a normal sandbox creation payload,
and `max_parallelism`. It returns `202 Accepted` plus a location under
`/v1/operations/{id}`. A completed operation always has one result for every
requested index, including structured problem details for failures. Operations
and their original acceptance responses persist in SQLite. After restart,
unfinished members retry their recorded worker database; they are not placed
elsewhere when a previous response is unknown.

## Billable usage

`GET /v1/usage` returns billable intervals and their totals;
`GET /v1/sandboxes/{id}/usage` scopes the same shape to one sandbox. An
interval is the span one VM served a sandbox, so a pause/resume cycle produces
two — the paused span in between bills nothing.

```json
{
  "intervals": [{
    "id": "sbx_123:2", "sandbox_id": "sbx_123", "sequence": 2, "state": "closed",
    "resources": {"vcpu": 2, "memory_mib": 1024},
    "started_at": "2026-08-09T10:00:00Z", "ended_at": "2026-08-09T10:10:00Z",
    "duration_seconds": 600, "vcpu_seconds": 1200, "memory_mib_seconds": 614400,
    "cpu_seconds": 90, "end_reason": "hibernate", "metadata": {"run_id": "ci_456"}
  }],
  "totals": {"intervals": 2, "open_intervals": 1, "duration_seconds": 900,
             "vcpu_seconds": 1800, "memory_mib_seconds": 921600, "cpu_seconds": 132.5},
  "window": {"from": "2026-08-01T00:00:00Z", "selection": "overlap"},
  "coverage": {"hosts_reporting": 1, "scope": "live_hosts", "truncated": false}
}
```

Five properties clients should rely on:

- `vcpu_seconds` and `memory_mib_seconds` are **billed** (allocated resources ×
  duration). `cpu_seconds` is host CPU actually consumed — recorded for
  transparency, never charged, because CPU is deliberately oversubscribed.
- `totals` covers the **whole selection**, not the returned page, so paging
  never changes the amount owed.
- `from`/`to` select by **overlap**, and a selected interval is reported whole
  rather than clipped to the window (`window.selection`). An open interval is
  measured to its last heartbeat, never to "now".
- `coverage.scope` is `live_hosts`. Usage from a worker that no longer exists
  survives only in the deployment's durability bucket, which is the billing
  record of truth; this API is for dashboards and debugging.
- **No host identity appears anywhere in a bill.** Which worker ran a sandbox is
  infrastructure, not billing: it is not actionable, it changes on every
  pause/resume, and runtime placement is already excluded from public objects.
  `coverage.hosts_reporting` is a count, and the line-item `id` is
  `<sandbox_id>:<sequence>` rather than the ledger's host-keyed internal key —
  that one exists so the at-least-once durability spool can dedupe, and it stays
  in the spool.

`GET /v1/usage?sandbox_id=` also answers for **deleted** sandboxes, because it
queries every host instead of routing by ID. The id-scoped route cannot — it
routes to an owner that no longer exists — and returns 404 pointing here.

## Live utilization

`GET /v1/sandboxes/{id}/metrics` returns a time series of what a sandbox is
**consuming**, which is a different question from what it is **billed**
(`/usage`, above, which charges allocation). Samples are taken on the owning
host every few seconds:

```json
{
  "samples": [
    {
      "timestamp": "2026-08-17T12:00:05Z",
      "vmm_generation": 1,
      "cpu_count": 2,
      "cpu_used_pct": 42.5,
      "cpu_seconds_total": 5.25,
      "host_mem_bytes": 268435456,
      "rootfs_alloc_bytes": 2334523392,
      "net_rx_bytes": 30720,
      "net_tx_bytes": 40960,
      "mem_used_bytes": 786432000,
      "mem_total_bytes": 1041661952,
      "disk_used_bytes": 5000000000,
      "disk_total_bytes": 8000000000,
      "load1": 1.5,
      "processes": 42
    }
  ],
  "state": "running",
  "interval_seconds": 5
}
```

- **The window is recent, not historical.** Samples live in the owning worker's
  memory and are bounded (30 minutes at the default interval). They do not
  survive a worker restart, and a sandbox that moved hosts starts a fresh
  series. For long-term trends, scrape the deployment's `/metrics` — which
  exports these aggregated per host, deliberately without a sandbox label.
- `samples` is **empty** for the first few seconds of a sandbox's life, before
  the first tick. That is a normal state, not an error.
- `limit` keeps that many of the **newest** samples, so `?limit=1` is the
  current reading. `from`/`to` bound the window.
- **Reading is passive.** It never resumes a paused sandbox: a hibernated one
  keeps its samples, stops producing new ones, and reports `state: hibernated`.
- `cpu_used_pct` is a percentage of **allocated** vCPUs, so 100 means the
  sandbox is using everything it was given. It is 0 on the first sample of a
  generation, which has no predecessor to difference against.
- `host_mem_bytes` is the host's memory charge — guest pages *touched*. It does
  not fall when the guest frees memory, so it is a high-water mark of cost
  (measured: a guest that touched 384 MiB and freed it released 0 MiB).
  `mem_used_bytes` is the guest's own view and is the one a workload cares
  about.
- `rootfs_alloc_bytes` includes blocks still **shared** with the image the
  sandbox was cloned from, so it starts around 2.2 GiB and is not disk the
  sandbox exclusively consumes. Its **growth** is what the sandbox wrote
  (measured: +248 MiB for a 256 MiB write).
- Every counter belongs to a **VM**, not to a sandbox: a resume or restore
  replaces the VM and restarts them at zero. `vmm_generation` changes when that
  happens, so a reset is self-describing rather than a counter mysteriously
  going backwards.
- The guest-reported fields (`mem_*`, `disk_*`, `load1`, `processes`) are
  **absent**, not zero, on deployments that do not poll the in-guest agent.

## Internal compatibility contract

Fleet coordination is not part of the public OpenAPI document. New
integrations use `/internal/v1`:

- `POST /internal/v1/hosts:register`
- `GET /internal/v1/hosts`
- `GET|PUT /internal/v1/worker-release`
- `POST /internal/v1/hosts/{id}:drain`
- `POST /internal/v1/sandboxes/{id}:adopt`
- `POST /internal/v1/sandboxes/{id}:release`

The historical internal paths remain aliases during migration.
