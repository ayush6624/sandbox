# Peer transfer for hibernation

This page describes the original durable-first implementation and its measurements.
Eligible workers now support [planned moves before backup completes](async-peer-handoff.md).
Use `--handoff durable` to reproduce the policy measured here.

Released sandboxes can resume on another worker using the source worker's
retained checkpoint. Existing release/adopt callers, including gateway drain,
discover the peer through the durable hibernation record. No new move API is
required.

Release still waits for cloud durability. It then retains immutable copies of
the normalized memory, device state and frozen root filesystem in a directory
identified by a fresh UUID. The source deletes its serving row before attaching
the peer reference to the durable record with a conditional update. An adopter
that has already consumed that record cannot have its checkpoint recreated by
a delayed hint publication. Retained files are a cache, never a serving sandbox.

The destination validates the peer descriptor against the durable record and
manifest digest. Disk and device state use the existing authenticated sparse
stream protocol. Memory faults use verified local cache, then raw peer chunks,
then the same content hash in cloud storage. Bad peer bytes and unavailable
peers fall back. Failed sparse transfers discard partial staging before cloud
reconstruction so stale bytes cannot survive in holes.

After readiness, eight background workers populate the destination chunk cache.
They acknowledge the exact source generation only after every nonzero chunk
has been read back and verified locally. VM exit cancels further scheduling;
already active requests retain their bounded timeouts. The source removes an
acknowledged generation, while a persisted 30-minute expiry and minute sweep
cover failed acknowledgments and worker restarts. Old-generation deletion
cannot remove a sandbox that has subsequently returned to that worker.

Peer endpoints live under `/internal/v1/hibernations/{generation}` and require
worker credentials. Peer URLs use the existing private-IP validation and
redirect refusal. Chunk requests require membership in the captured manifest.
The File backend also uses peer chunks when assembling its local memory file.

## Verification

The [full-memory proof](../scripts/verify-lazy-adoption.py) accepts
`--transport peer` to require peer artifact/chunk counters and successful source
cleanup. `--transport fallback` deletes the retained generation before adoption,
requires a fallback counter, and verifies the same complete RAM/disk hashes in
both directions. The latter proves source-unavailable recovery; deterministic
tests separately cover an uncached late memory read after request cancellation
and source removal.

`--transport peer-loss` removes a still-present source checkpoint immediately
after adoption reports ready, then checks all RAM and disk bytes. It fails if
background hydration already removed that generation, so a no-op deletion is
never reported as source-loss evidence. Its fallback counters include background
hydration and do not identify which fetch was caused by a guest memory access.

The [adoption benchmark](../sdk/typescript/benchmarks/hibernation-adoption-bench.ts)
accepts `--transport peer` or `--transport gcs`, independently of its File/UFFD
backend. GCS mode removes the peer generation before starting the adoption
timer. Both modes retain the same source publication and retention work.
Reports keep release, readiness, first command and working-set checks separate.

Counters begin with `sandbox_hib_peer_`. Chunk byte counters contain raw memory
bytes; destination artifact bytes contain sparse wire bytes; source serve bytes
contain uncompressed payload bytes. `sandbox_hib_peer_retained_logical_bytes`
includes sparse holes and shared reflink extents and is not physical disk usage.

## Limits

This change retains the cloud publication wait and existing ownership protocol.
It does not establish an active ownership lease for arbitrary concurrent direct
adopters. Cloud metadata and cold base-rootfs retrieval may still be needed on
the peer path. Missing chunks remain dependent on cloud storage after the peer
expires. Background hydration intentionally transfers all nonzero memory to
permit early source cleanup, even if the guest has not touched every chunk.

Live results are recorded in [the verification directory](verification/peer-hibernation-2026-09-06/).
