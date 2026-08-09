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
with another body returns `409 idempotency_key_reused`. Completed entries are
kept for up to 24 hours in the serving process; a gateway restart currently
clears them, so durable cross-restart replay remains an operational-readiness
follow-up.

Errors use RFC 9457 `application/problem+json`. The standard members are
extended with a stable `code`, the response `request_id`, and optional field
`violations`. Clients should branch on `code`, not human-readable `detail`.

Collection methods return resource-shaped envelopes such as
`{"sandboxes":[...],"next_page_token":"..."}`. `page_token` is opaque and must
be returned unchanged. The default page size is 50 and the maximum is 100.

## Creation sources

`POST /v1/sandboxes` is the only single-sandbox creation method:

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

## Lifecycle

- `POST /v1/sandboxes/{id}:pause` preserves the sandbox identity while
  releasing runtime capacity.
- `POST /v1/sandboxes/{id}:resume` restores the same sandbox resource.
- `PATCH /v1/sandboxes/{id}` changes its name, metadata, TTL, or idle timeout.
- `DELETE /v1/sandboxes/{id}` permanently deletes it.

Snapshot creation is `POST /v1/sandboxes/{id}/snapshots`. A snapshot can be
used by many independent creates while its source sandbox continues running.

## Batch creation

`POST /v1/sandbox-batches` accepts `count`, a normal sandbox creation payload,
and `max_parallelism`. It returns `202 Accepted` plus a location under
`/v1/operations/{id}`. A completed operation always has one result for every
requested index, including structured problem details for failures. Operations
are retained in the serving process for up to 24 hours. Operation persistence
across a gateway restart belongs with the transactional storage work in P3.

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
