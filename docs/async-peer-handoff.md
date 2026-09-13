# Planned moves before cloud backup completes

The implementation is deployed in development. [Live verification](verification/async-handoff-2026-09-06/)
measured faster successful moves but exposed two guest-readiness timeouts in
eleven async benchmark attempts. Production adoption needs that failure resolved.

A planned move can resume from the source worker's frozen checkpoint while that
worker uploads the checkpoint to cloud storage. The foreground work includes
freezing the VM, hashing memory, retaining immutable files, and publishing a
small ownership record. Compression and bulk cloud upload run in the background.

Eligible workers use this path through the existing release/adopt API. Eligibility
requires chunked cloud memory, worker credentials, and a private advertised URL.
`POST /sandboxes/{id}/release?durability=required` waits for the generation's
completed backup. Ordinary hibernation retains its durable publication behavior.

## Ownership precedes execution

Each sandbox has one shared control object, `handoff/{id}/control.json`.
Conditional writes move it from a local owner to an offered checkpoint, then a
destination claim, then a running owner. The destination records running
ownership before starting the VM. A second worker cannot claim that generation,
even after the first claim becomes old.

The source commits its retained-generation journal and removes its serving row
in one database transaction. A crash after that transaction leaves a retryable
publication job. A lost claim response can recover the same unstarted claim on
the same host and registry. Failed startup reopens a claim only after the process
is confirmed stopped. Uncertain termination keeps ownership reserved.

There is no automatic takeover of a running claim. Replaying an older checkpoint
could repeat external effects after the destination had already executed. Recovery
from an ambiguous owner requires an explicit operational decision.

## Source retention has two obligations

The destination copies disk and device state through authenticated peer streams.
Lazy memory reads use verified local chunks, peer bytes, and cloud chunks. If
cloud upload is incomplete, a failed peer read retries within a bounded timeout.
After readiness, background hydration verifies every nonzero memory chunk before
acknowledging the generation.

An acknowledgment cannot delete the source's only pending backup. Source files
remain until cloud backup completes and the destination acknowledges, or the
completed generation's retention interval expires. Pending backups survive
restart through the registry journal. A worker accepts at most 32 retained jobs.

Backup writes immutable generation paths and content-addressed memory chunks.
The generation descriptor is written last. Background completion never rewrites
current ownership, so an old upload cannot resurrect a checkpoint after a return
move. If the peer is unavailable, adoption accepts only that exact completed
generation from cloud storage.

## Availability during the move

Until destination hydration or cloud backup completes, missing memory remains
dependent on the source. Losing that source can stall or fail the destination's
memory reads. A network partition can cause the same result. This is a deliberate
availability tradeoff for removing bulk upload from the move's critical path.

The gateway prevents automatic retirement while any retained handoff remains.
An unreadable journal count also prevents retirement. Planned maintenance must
allow pending backups to finish. Persistent files make restart recovery possible,
but do not guarantee uninterrupted memory access during a source outage.

Graceful shutdown rejects new non-peer requests and keeps peer HTTP and uploads
alive for up to 10 seconds while admitted requests and pending backups drain.
The shutdown phases share a 115-second deadline. If backup exceeds the drain
window, shutdown logs the unfinished jobs and restart resumes them. This bounded
drain does not replace waiting for zero pending jobs before maintenance.

## Verification

`scripts/verify-lazy-adoption.py --transport peer --handoff async` checks every byte
of a 256 MiB random memory buffer and an 8 MiB disk file across a round trip.
Source-removal modes require `--handoff durable`.

`sdk/typescript/benchmarks/hibernation-adoption-bench.ts` records release,
adoption readiness, first command, and working-set checks separately. Its
`--handoff async|durable` flag makes the durability policy explicit. Compare
release plus readiness plus first command to measure the whole interruption.

Deterministic tests block cloud payload writes while release and peer memory
reads succeed. Other tests cover concurrent and staggered claims, lost CAS
responses, acknowledgment before backup, restart cleanup, and delayed uploads
after a return move.
