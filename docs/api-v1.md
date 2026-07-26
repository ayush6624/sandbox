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
