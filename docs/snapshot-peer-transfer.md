# Snapshot peer transfer

Status: implemented locally; production rollout requires a disposable-worker
canary and cross-host benchmark.

## Why

Workers already advertise user snapshot IDs in heartbeats, and the gateway
prefers the worker that has a snapshot locally. When capacity places a restore
on another worker, that worker previously downloaded the snapshot from GCS even
while a live source worker held the exact immutable files. That adds an object-
store upload/download round trip to the first restore on each host.

## Data path

1. The gateway selects a target with normal capacity reservation.
2. If the selected target differs from the live snapshot owner, the gateway
   sends its private worker URL in `X-Sandbox-Snapshot-Peer`.
3. The target validates that the hint is an IP-only private or loopback URL and
   calls the source's `/internal/v1/snapshots/...` endpoints with the existing
   worker bearer credential.
4. Metadata, rootfs, memory, and state stream directly over the VPC. Artifacts
   use the existing gzip sparse wire format; diff rootfs transfers include only
   extents that diverge from the immutable base.
5. The target stages `.peer.tmp` files, syncs each decoded stream, atomically
   renames the completed files, and creates the registry row last.
6. Any peer error cleans partial files and falls back to the existing GCS pull.

GCS remains the durable commit log and dead-host fallback. The gateway only
sends a small routing hint; snapshot bytes never transit the gateway.

Concurrent restores use the existing per-snapshot pull lock, so a target host
performs at most one peer or GCS population while other requests wait for the
same local registry row.

## Security boundary

- Export endpoints live under `/internal/v1` and require the worker credential.
- Tenant API credentials cannot call them.
- Peer hints reject hostnames, public/link-local IPs, URL credentials, paths,
  queries, fragments, and invalid ports.
- HTTP redirects are disabled so a peer cannot redirect the worker bearer to a
  different address.
- Corrupt or truncated gzip streams fail decoding and never create a registry
  row.

## Metrics

- `sandbox_snapshot_peer_pulls_total`
- `sandbox_snapshot_peer_pull_failures_total`
- `sandbox_snapshot_peer_serves_total`
- `sandbox_snapshot_peer_payload_bytes_total`
- `sandbox_snapshot_gcs_fallbacks_total`

## Canary gate

On two disposable production-shaped workers:

1. Create the dirty working-set snapshot on worker A.
2. Confirm its GCS upload is still pending or clear worker B's local copy.
3. Force an 8-way restore onto worker B.
4. Verify one peer population, zero fallback, eight successful clones, and no
   temporary artifacts or registry rows left after cleanup.
5. Repeat with the peer endpoint blocked and confirm GCS fallback succeeds.
6. Run counts `1,4,8,16,24,32` twice and compare first-host command readiness,
   peer bytes, worker CPU, and full hydration against the current production
   result.

The production gate is correctness first, then a statistically meaningful
reduction in the cold non-owner restore. Same-owner restores should be unchanged.
