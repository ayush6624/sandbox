# Bounded memory-chunk cache

The worker cache now has a configurable budget and evicts unused entries.
The implementation passes local loader, peer fallback, filesystem, and race
tests. It has not been deployed to the development fleet. Remote chunk and
base-object collection remains unfinished.

On September 13, 2026, read-only disk measurements found:

| Development worker | Allocated cache bytes | Apparent cache bytes |
| --- | ---: | ---: |
| `sandbox-workers-us-frr2` | 89,967,992,832 | 89,963,626,496 |
| `sandbox-workers-us-qslx` | 84,989,140,992 | 84,984,987,648 |

These measurements cover `snapshots/chunkcache`, about 175 GB across both
workers. They are a baseline, not evidence of reclamation by the new code.
The control VM service account could not inspect the snapshot bucket. A subsequent
read-only inventory through the worker's existing identity completed in 40.4 seconds.
It listed 75,418 current objects totaling 104,515,167,218 bytes, including 74,413
chunk objects totaling 100,255,626,619 bytes. Both the control and worker identities
were denied bucket-settings reads. Versioning, retention, and soft-deletion
settings remain unverified, along with noncurrent and soft-deleted object bytes.

## Configuration and behavior

Set `uffd_chunk_cache_mib` in the worker JSON configuration. Zero or omission
selects 4096 MiB. A positive value sets the budget; `-1` disables disk caching.
The budget covers final payload bytes plus temporary-write and missing-chunk
reservations. It does not cap filesystem metadata, VM heap, retained peer
checkpoints, local snapshots, or cloud objects. Allocated filesystem bytes are
reported separately from payload bytes.

The worker acquires an exclusive OS file lock on the cache directory, scans
existing entries, removes abandoned cache temporary files, and trims old chunks
before serving loaders. Startup eviction uses file modification time. During
the process lifetime, reads update recency in memory. Unknown files remain
counted as residue. Failed unlinks remain counted; admission cannot pretend
those bytes were reclaimed. An incomplete inventory disables cache admission.

Every eager reconstruction, lazy memory source, and peer loader shares the
same cache. Network requests run outside the cache lock. A cache read returns
an owned byte slice, so unlinking the file cannot invalidate a VM's existing
buffer. A full or unavailable cache still returns verified fetched bytes to the
caller. Closing the cache disables late writes before releasing ownership.

Background peer hydration reserves capacity for every unique nonzero chunk
before fetching. Overlapping hydrations share identical chunks. Reservations
remain pinned through cache verification and acknowledgment. A generation
handoff also waits for the matching cloud commit before acknowledging, keeping
the complete destination image pinned while its backup is pending. Cancellation
or a mismatched commit skips acknowledgment and releases the reservation. If a complete
image cannot fit, hydration leaves the source's cache-ready flag unset. The
VM can continue fetching through peer or cloud fallback.

After acknowledgment, those disk pins can be released because cloud fallback
is available. Generation handoffs retain the source until publication and cloud
backup complete, regardless of cache acknowledgment. Legacy peers already have
cloud fallback. Remote deletion remains disabled.

## Observability

Worker `/metrics` exposes the following series:

| Series suffix after `sandbox_chunk_cache_` | Meaning |
| --- | --- |
| `ready` | Inventory and exclusive ownership succeeded; zero means cache admission is disabled |
| `limit_bytes` | Configured payload and reservation budget |
| `resident_bytes` | Payload bytes in managed final files |
| `reserved_bytes` | Capacity held for missing hydration chunks |
| `allocated_bytes` | Observed file allocation, excluding directory metadata |
| `residue_bytes` | Unmanaged or failed-cleanup bytes charged against admission |
| `reclaimed_bytes_total` | Payload bytes successfully unlinked during this process |
| `errors_total` | Inventory, write, or cleanup failures |
| `bypassed_total` | Verified loads whose cache writes were skipped |

When `ready` is zero, inventory and allocation totals may be incomplete. A
failed temporary-file cleanup conservatively charges its reserved size as
residue and disables further admission until restart.

## Verification and remaining work

Run the focused tests with:

```sh
go test -race ./internal/server -run 'Test(ChunkCache|PeerHydration|NewChunkLoad|ChunkLoad|HibPeerClient)' -count=1
go test ./...
```

Both commands passed locally. The focused suite also passed as a compiled Linux
test binary on the development control VM, using isolated temporary files and
loopback servers. No worker or gateway service was replaced. Tests use real cache files and production loaders.
They cover repeated eviction followed by a late read from the captured manifest,
concurrent writers, shared and rejected reservations, startup cleanup, failed
rename and unlink, exclusive ownership, and a fetch completing after cache close.
A peer test holds acknowledgment open while other writes apply cache pressure,
then evicts the acknowledged image and verifies cloud refetch after peer retirement.
Pending-backup tests verify that cache pressure cannot evict the reserved image
before its matching cloud commit, and that cancellation and mismatched commits
neither acknowledge nor leak reservations.
Metrics are checked against the files that remain on disk.

The full storage task remains open. Remote collection must first account for
publication intents, retained roots, active UFFD sources, independent hydration,
and worker incarnation death. Deleting a hibernation record or observing VM
process exit does not prove all chunk readers have finished. Existing positive
upload caches and old worker releases do not participate in a collector protocol.
Saved-snapshot tombstones and ownership fences must remain intact.

The design comparison favored generation-owned storage over a global shared-chunk
sweep. The inventory found 243 distinct observed manifest sets, including historical
records. Their listed shared chunks occupy 88,104,113,526 compressed bytes; storing
each set independently would require 107,743,038,270 bytes, about 22% more. This is
a measured tradeoff for those observed manifests, not a forecast of live retention.
The listing excludes local journals and active loader references, and is not an
atomic snapshot of the bucket. It cannot authorize deletion.

Run the metadata-only inventory on a GCE worker with snapshot-bucket read access:

```sh
python3 scripts/audit-chunk-storage.py --bucket YOUR_SNAPSHOT_BUCKET > storage-inventory.json
python3 scripts/audit-chunk-storage.py --bucket YOUR_SNAPSHOT_BUCKET --bucket-settings-only
python3 -m unittest discover -s scripts -p test_audit_chunk_storage.py
```

The script reports truncation, unreadable or changed manifests, and omitted
version classes. It does not download memory payloads or change cloud objects.
Four local tests verify pagination, including an empty page, scan limits,
duplicate generation references, and invalid manifests.

The [remote storage design](chunk-storage-design.md) and its runnable models
record the proposed next stage and the remaining integration gates.
Generation-owned storage remains a proposal.
It is not implemented or enabled. Provider semantics also need integration proof,
including [GCS object listing](https://docs.cloud.google.com/storage/docs/json_api/v1/objects/list)
and [generation-conditioned deletion](https://docs.cloud.google.com/storage/docs/json_api/v1/objects/delete).
