# Tensorlake architecture notes for sandbox

Research date: 2026-08-23

## Scope and conclusion

Tensorlake's [official archive](https://www.tensorlake.ai/blog/archive) mixes
sandbox-runtime engineering, product examples, agent-harness analysis, and its
older Document AI material. This note focuses on the posts that can change this
project's architecture. Entries whose public page is only a draft teaser are
listed but are not used as evidence.

The main conclusion is that this project is already on a very similar path:

- Firecracker microVMs restored from a golden snapshot;
- a separate pause/resume lifecycle and reusable snapshots;
- XFS reflinks plus changed-extent uploads;
- content-addressed, chunked memory in GCS with working-set prewarm;
- cross-host reconstruction and an ownership fence;
- a stateless, heartbeat-driven gateway with reservations, queueing, and safe
  scale-in;
- a public edge that keeps workload traffic out of the gateway.

The best lessons to adopt next are therefore incremental: unify placement as an
explicit filter-and-rank pipeline, make warm capacity template-aware, finish and
measure lazy cross-host memory restore, and separate the host forwarding process
from the frequently deployed sandbox orchestrator. A custom Firecracker block
engine or a RocksDB command plane would be premature today.

Tensorlake's performance and scale numbers are vendor-reported. They are useful
as design hypotheses, not as baselines we should claim without reproducing the
workloads on our fleet.

## Architecture-relevant reading list

| Post | Main architectural idea | Relevance here |
| --- | --- | --- |
| [Firecracker disk snapshots in O(changed bytes), not O(disk size)](https://www.tensorlake.ai/blog/firecracker-disk-snapshots-o-changed-bytes) (2026-07-13) | A read-only base, live CoW overlay, dirty/zero bitmaps, content-addressed frozen layers, and asynchronous upload after a short freeze. Benchmark the complete guest path and real databases, not only `fio`; `io_uring` is not automatically faster. | Very high. We already use XFS reflinks and FIEMAP-derived changed extents, which obtains much of the snapshot-size benefit without a Firecracker fork. |
| [Zero-copy TLS ingress with kTLS and splice(2)](https://www.tensorlake.ai/blog/near-zero-overhead-sandbox-networking) (2026-07-09) | Keep authentication at the edge, move the host hop to a minimal L4 forwarder, meter byte flow for idle activity, and give the forwarder a lifecycle independent from orchestration. Removing the second L7 hop produced most of their measured gain; kTLS added a smaller throughput gain. | Very high. Our public edge already bypasses the gateway, but the worker tunnel is still served by the same `sandbox serve` process that owns VM orchestration. |
| [Filter and rank: how we schedule sandboxes across every cloud](https://www.tensorlake.ai/blog/multi-cloud-scheduling) (2026-05-13) | First apply hard eligibility filters, then rank by bin-packing, topology, and snapshot/cache locality. Prefer same node, then zone/region. Keep a shared control plane while federating ingress and storage into each data plane. | Very high. Our placement inputs exist but are split across several selection functions and do not yet model topology. |
| [Starting hundreds of sandboxes in parallel](https://www.tensorlake.ai/blog/starting-1000-sandboxes-in-parallel) (2026-04-24) | Persist each placement decision and its command atomically in an outbox; workers long-poll by sequence number, acknowledge progress, and start their batch concurrently. Backlog, drain rate, and drain latency become direct signals. | High later. Our synchronous proxy plus reservations and bounded queue is simpler and appropriate at current scale, but gateway restart loses queued requests and in-process operations. |
| [How we got to 5,000,000 sandboxes per project](https://www.tensorlake.ai/blog/5-million-sandboxes-one-api) (2026-04-14) | Shard scheduler state so hot placement data stays local, use content-addressed memory chunks with a manifest and lazy page faults, preserve shared page-cache inodes, and reflink root filesystems. | High conceptually. Reflinks and lazy chunk sources are already present. Scheduler sharding is not justified by our current fleet size. |
| [Suspend vs. snapshot](https://www.tensorlake.ai/blog/suspend-vs-snapshot) (2026-04-16) | Suspend is cost control for one continuing identity; snapshot is a durable, reusable artifact for checkpointing and fan-out. Long-running agents commonly need both. | Already adopted. The v1 API exposes `:pause`/`:resume` separately from immutable snapshot creation. |
| [Introducing Tensorlake BYOC](https://www.tensorlake.ai/blog/introducing-tensorlake-byoc) (2026-07-28) | Split control plane from customer-owned compute/storage; use outbound node identity, keep images, snapshots, and workload traffic inside the data plane, and retain one API across runtimes/providers. | Medium now, high if this becomes a managed BYOC product. Our self-hosted design and separate trust domains are a good base, but discovery and topology are GCP-oriented. |
| [Build Your Own CI infrastructure](https://www.tensorlake.ai/blog/build-your-own-ci-infrastructure) (2026-07-30) | Admit webhook work durably, restore one prepared snapshot into parallel test shards, and attach a persistent build cache to otherwise ephemeral runners. | High as a product/workload lesson. It validates template-aware pools, batch fan-out, and durable operations; a shared mutable cache needs its own isolation design. |
| [Autoresearch on steroids with sandboxes](https://www.tensorlake.ai/blog/autoresearch-on-steroids-with-sandboxes) (2026-04-03) | Prepare once, snapshot, fan out reproducible candidates, always apply resource/time limits, and collect outputs even for crashes/timeouts. | Already supported by snapshot fan-out and the RL examples. It suggests a higher-level map/reduce SDK helper rather than a runtime rewrite. |
| [The End of Database-Backed Workflow Engines](https://www.tensorlake.ai/blog/end-of-database-backed-workflow-engines-graphrag-object-storage) (2026-01-29) | Make large artifacts and checkpoints object-native; keep small manifests and references in coordination state instead of serializing workload data through a database. | Medium. Our GCS snapshot/usage design follows this rule. It should also guide durable batch-operation results, but does not imply replacing SQLite for host-local allocation. |

Related application and integration posts—Harbor, Claude Managed Agents,
Devin Outposts, browser/computer-use harnesses, and coding-agent scaffold
reviews—are useful for examples and distribution but do not materially change
the runtime architecture. The archive also lists *Sub-second cold starts for
stateful microVMs*, *Durable agent loops without a queue*, *Filesystem
benchmarks*, and a security primer, but their current public pages contain only
draft teasers. They are not a sufficient basis for design decisions.

## What this project already does well

| Tensorlake lesson | Current project mechanism | Assessment |
| --- | --- | --- |
| Restore instead of boot | A per-host golden snapshot and a ready VM pool; normal hot clones are already fast. | Keep. Optimize measured post-restore identity/readiness work, not Linux boot. |
| Pause and snapshot are different resources | v1 `:pause`/`:resume` preserves the sandbox identity; snapshots are immutable sources for independent creates and batches. | Keep. The public vocabulary is clearer than the legacy internal `hibernate` name. |
| CoW disks and delta persistence | XFS reflink clones plus FIEMAP comparison upload only rootfs extents that diverged from the base. | Keep. This captures the main O(changed-bytes) storage property without owning a Firecracker fork. |
| Content-addressed, lazy memory | Full freezes can chunk memory into content-hashed GCS objects; all-zero chunks are implicit; a working set can be prefetched through UFFD. | Keep, but it remains default-off because same-host eager file restore won the benchmark. |
| Durable state can move across hosts | Workers persist the hibernation record, VM state, memory/rootfs deltas, and an owner fence to GCS; the gateway can dispatch an adopt when a route is missing. | Keep. Finish the lazy UFFD clone path and failure-race measurements before enabling it broadly. |
| Cache/locality-aware routing | Snapshot operations prefer the known owner and otherwise choose another host that can pull from GCS. Heartbeats advertise snapshot IDs. | Extend. Locality is special-cased rather than expressed through one ranking model. |
| Burst-safe placement | The gateway atomically reserves advertised capacity, queues bounded overflow, fails over on capacity pushback, and sizes the MIG from memory-aware demand. | Keep. This solves today's burst problem with much less machinery than a distributed command bus. |
| Safe bin-packing and scale-in | Placement fills the tightest fitting live host; scale-in cordons one named host and deletes it only after it is empty. | Keep. This is a strong correctness property and should remain part of ranking. |
| Federated data path | `sandbox-edge` resolves the owner and connects directly to a worker; user bytes do not transit the gateway. URL-only exposure avoids consuming the worker's host-port pool. | Keep and simplify the worker hop further. |
| Distinct trust domains | Client, worker-control, and edge credentials are separated; worker callbacks and public edge resolution do not reuse tenant authority. | Keep. Extend identity into topology/BYOC metadata only when needed. |

## Recommended roadmap

### P0 — make placement an explicit filter-and-rank pipeline

Today `reserveHostMode`, `reserveHostFor`, `pickHost`, rollout release gates,
warm-pool preference, and snapshot-owner preference each encode part of the
placement policy. Consolidate them behind one internal model:

```text
PlacementRequest
  kind: default | template | snapshot | adopt
  slots + memory units
  source/template/snapshot id
  excluded host ids

filter
  live, release-compatible, not draining/penalized, tenant/topology match,
  enough physical slots and memory units

rank (lexicographic initially)
  exact warm-template hit
  source already local
  same prior host/zone when resuming
  smallest residual capacity (bin-pack)
  stable host-id tie-break
```

Do not add a spatial index yet. A scan over the current host set is easy to
audit and fast enough; the value is a single policy and observable decisions.
Emit counters for rejection reason and selected locality tier, plus a placement
latency histogram. Preserve atomic reservation under the gateway lock.

Acceptance criteria:

- every create, restore, fan-out, and adopt goes through the same eligibility
  predicates;
- a snapshot-local preference never overrides capacity or draining;
- tests enumerate why every rejected host was ineligible;
- bin-packing and the one-host-at-a-time drain property stay unchanged.

### P0 — implement the existing multi-template warm-pool plan

The existing `docs/warm-templates-plan.md` is almost exactly the practical
lesson from Tensorlake's CI and locality posts. Make `warm_ready` template-keyed,
record which template a warm row belongs to, apply a total warm-pool budget, and
rank an exact warm hit above a merely staged snapshot.

This should precede scheduler indexing or a command outbox: it directly changes
user-visible create latency for non-default environments and makes the proposed
placement abstraction earn its keep.

Acceptance criteria:

- two different templates can be warmed without cross-claiming;
- one exhausted template pool falls back to an ordinary clone without consuming
  another template's ready VM;
- per-template hit, miss, ready, preparing, and failure metrics exist;
- warm targets cannot starve ordinary capacity beyond the configured budget.

### P1 — finish lazy cross-host restore, then decide from measurements

The hard durability work is already present. The missing performance proof is a
cross-host adopt that resumes against the GCS chunk source rather than fully
materializing memory first. Implement the planned clone-UFFD path and compare it
with the eager File backend on:

- wake-to-first-successful-exec;
- bytes fetched before first exec and during the first minute;
- page-fault p50/p95/p99/max and chunk-cache hit ratio;
- failures during GCS timeout, corrupt/missing chunk, owner-fence contention,
  and source/target overlap;
- small idle sandboxes and larger database/build workloads.

Keep File as the default until the remote path wins a representative workload
without an unacceptable tail. Same-host results should continue choosing File;
the project already learned that UFFD is a source-abstraction feature, not a
universal latency optimization.

### P1 — split the worker's forwarding data path from orchestration

`sandbox-edge` already has the right external boundary, but its CONNECT tunnel
terminates in `sandbox serve`, the process that also changes whenever scheduling,
snapshot, or VM lifecycle code changes. Tensorlake's strongest ingress lesson is
lifecycle separation, not merely `splice(2)`.

Introduce a small host-local forwarder daemon that:

- accepts an authenticated, length-bounded preamble naming sandbox id and target;
- asks the local orchestrator only for wake/route authorization before the
  tunnel starts;
- owns the long-lived socket and forwards TCP half-closes faithfully;
- reports byte activity without parsing tenant payloads;
- holds no sandbox registry or scheduling state;
- drains independently during its rare upgrades.

First prove that deploys or orchestrator restarts currently interrupt meaningful
connections. Start with ordinary Go TCP copy or Linux `splice`; add kTLS only if
CPU-per-GB or throughput measurements justify its kernel/configuration cost.
The Tensorlake data shows that deleting the redundant L7 behavior produced most
of the gain.

### P2 — add a durable command/outbox plane only at the demonstrated boundary

The present gateway is intentionally stateless and synchronous. Keep that
simplicity until at least one of these becomes a real requirement:

- accepted creates must survive a gateway restart or client disconnect;
- batch operations must survive process restart;
- dispatch throughput or queue inspection becomes a bottleneck;
- one scheduling transaction must atomically commit placement and delivery.

At that point, introduce an operation record plus an outbox in the same durable
transaction, monotonically sequence commands per worker/shard, have workers
long-poll and acknowledge the highest applied sequence, and make every command
handler idempotent. Export backlog, drain rate, oldest-command age, redelivery,
and full-resync counts from day one.

Do not put snapshot bytes or command output in the outbox. Store artifacts in
GCS and keep immutable references plus checksums in the operation record. This
combines the useful parts of Tensorlake's command and object-native designs.

### P3 — add topology and BYOC identity when there is a second failure domain

When the fleet expands beyond one GCP region, add cloud/region/zone and tenant
or data-plane identity to host registration. Make tenancy and required locality
hard filters; make same node, zone, and region ranked preferences. Keep snapshot
and image storage inside the selected data plane, and mint short-lived workload
credentials at its ingress rather than routing data through the central gateway.

Avoid building a multi-cloud scheduler speculatively. The proposed P0 placement
model creates the seam now; new topology dimensions can be added when a real
second region/provider exists.

## Explicit non-recommendations

- **Do not fork Firecracker for a custom block overlay yet.** Our reflink +
  FIEMAP path already gives CoW clones and changed-byte durability. A fork is
  justified only if large-disk snapshot pause, layer depth, or guest database
  I/O becomes a measured product limit that the host filesystem cannot solve.
- **Do not add a nearest-neighbor/spatial host index yet.** The current fleet is
  nowhere near the state-size boundary described by Tensorlake. Centralizing
  placement semantics brings value now; indexing is a replaceable implementation
  detail later.
- **Do not replace host-local SQLite with object storage.** SQLite provides
  atomic allocation and local lifecycle truth. Object storage is the right home
  for immutable/durable artifacts and cross-host manifests, not tap/IP/port
  transactions.
- **Do not enable remote UFFD because another platform reports a fast restore.**
  Our measurements already show the same-host trade-off is workload- and
  cache-dependent. Enable it per path based on our p99 and failure behavior.
- **Do not add kTLS before removing unnecessary protocol work and measuring.**
  It raises kernel and operational requirements, and Tensorlake measured most
  CPU savings from the L4 boundary itself.

## Suggested implementation order

Implementation update (2026-08-23): steps 1 and 2 are now implemented. The
gateway uses one filter-and-rank scheduler with decision metrics, and warm pools
are keyed by immutable template id with bounded targets and exact claims.

1. Refactor placement behind a tested filter-and-rank interface and add decision
   metrics; no behavior change first.
2. Land template-keyed warm pools and use the new ranking dimensions.
3. Add the cross-host clone-UFFD path and publish an eager-vs-lazy benchmark.
4. Run a connection-survival and CPU/throughput benchmark on public ingress;
   split the worker forwarder if the lifecycle hypothesis is confirmed.
5. Design durable operations/outbox only when restart durability or dispatch
   throughput is placed on the public contract.
6. Add topology/BYOC filters with the first real additional data plane.

This ordering favors changes that improve today's default/template create path,
preserves the project's existing correctness properties, and makes each larger
Tensorlake-inspired idea earn its operational cost through a local benchmark.
