# Autoscaling and burst-start latency

This note explains where fleet scale-up time comes from, how the current
implementation compares with systems such as Modal, and which changes are most
likely to improve it. It is a design and prioritization document: sections
labelled **proposed** are not implemented yet.

> Production ownership update (2026-07-26): the GCP fleet now uses Nomad
> Autoscaler as its only MIG resize writer. Direct-scale measurements below are
> retained as historical evidence, but the gateway fast path is intentionally
> disabled in `control-install.sh` after dual writers caused post-churn
> overshoot.

The important distinction is between three different clocks:

1. **Placement decision** — decide that more workers are needed and request
   them from the infrastructure provider.
2. **Worker readiness** — make a requested worker capable of accepting
   sandboxes.
3. **Sandbox readiness** — create a microVM on a ready worker and wait until its
   in-guest `sandboxd` agent answers.

Calling all three "autoscaling" hides the actual bottleneck. Optimizing a
10-second control loop does not help much if resuming a worker takes another
9 seconds, and neither improvement makes an application that installs
dependencies after boot immediately ready.

## Current performance and critical path

### Best measured path — event-driven scale-out (2026-07-25, `0b049db`)

The held-burst benchmark started with two ready workers, each with 48
slots, then issued 160 simultaneous creates. The remaining 64 requests waited
while the MIG resumed two suspended standby workers.

| Span | Measured result |
| --- | ---: |
| Demand → direct MIG resize request | ≤1.095 s |
| Suspended worker resume → registered capacity | 8.037–9.161 s |
| Gateway registration → eligible capacity | 27–28 ms |
| First request → all 160 creates ready | 18.653 s |

See [Benchmarks](benchmarks.md#2026-07-25-suspended-standby-and-event-driven-scale-out-release-releasegate-20260725-1-commit-0b049db)
for the workload, timestamps, latency distribution, and comparison with the
older control-loop path.

The result says that the initial decision is no longer the dominant cost.
Worker resume is the hard lower bound for requests that exceed ready capacity,
and sandbox creation under contention accounts for much of the remaining tail.

**That number was produced with the gateway's direct scale-out path enabled.**
Production later disabled it to preserve the single-writer invariant on the
MIG (the `sandbox-gateway` unit documents this), which is what the
2026-07-27 measurement below re-measures.

### Production path today — autoscaler-only (2026-07-27, `ea0f707`)

Same canonical workload — floor of 2 running workers (96 slots) plus 6
suspended standby, 160 held creates. Evidence bundle:
`tests/results/autoscale-20260727-legacy-held-final/` (all `SHA256SUMS`
verified). 160/160 succeeded, zero errors, zero residual sandboxes.

Create latency is cleanly **bimodal**, and the split falls exactly on the
ready-capacity boundary:

| Mode | Count | Range | Mean | What it is |
| --- | ---: | ---: | ---: | --- |
| Fast | 96 | 4.411–17.664 s | 11.922 s | placed immediately on the 2 ready workers |
| Slow | 64 | 24.328–38.422 s | 31.285 s | queued, waiting for scale-out |

The two modes are separated by a 6.664 s gap with no samples in it. Because
only 96 of 160 requests are fast, **p95 (36.162 s) is entirely determined by
the queued 64** — it is the 57th-slowest of that group. Meeting a 30 s p95
therefore requires almost the whole slow mode to finish under 30 s, not merely
a better average.

Decomposing the slow mode against `timeline.jsonl` (times relative to run
start; requests issued ≈15.5 s, queue depth 64 observed at 17.5 s):

| Span | Time | Note |
| --- | ---: | --- |
| Demand → MIG resize issued (`mig_target` 2→5) | ~10.2 s | autoscaler decision latency |
| Resize → new worker advertises free slots | ~13.0 s | resume + serve + golden adopt + heartbeat |
| **Demand → usable new capacity** | **~23.2 s** | matches the 24.328 s slow-mode floor |
| Drain of the queued 64 across new workers | up to 14.1 s | `create_concurrency=24`, 2 waves |

Two observations follow.

1. **The decision, not the resume, is the regression.** Event-driven scale-out
   made this span ≤1.095 s; the Prometheus/Nomad-autoscaler loop makes it
   ~10.2 s even after `ea0f707` tightened the cadence to 5 s. Recovering most
   of that ~9 s is the single largest available win and is exactly
   [improvement 2](#2-make-direct-scale-out-level-triggered).
2. **Per-worker create throughput is the floor on the rest.** The live fleet
   runs `create_concurrency=24` against 48 slots on an `n2-standard-16`, so a
   saturated worker absorbs 48 creates in ~13 s (~3.7 creates/s). Raising the
   semaphore does not raise that ceiling — the host is already CPU/I-O bound —
   so drain time only falls by spreading the burst over more workers.

A plausible route to p95 ≤30 s is therefore ~16 s to usable capacity (needs
the decision back near ~1 s) plus ~12 s of drain. It is achievable but tight,
and it depends on restoring event-driven scale-out **without** reintroducing
two independent MIG writers.

### Resolved — gateway-owned scale-out (2026-07-28, `af3833a`)

That is what `af3833a` does: the gateway became the sole scale-**out** writer
with a level-triggered, grow-only watermark, and the Nomad autoscaler was
capped to scale-**in** only via the `sandbox:workers_scale_in_ceiling`
recording rule. Same canonical workload, same floor, same worker release
`a223889`. Evidence bundle
`tests/results/autoscale-20260728-gateway-scaleout/` (`SHA256SUMS` verified).

| Metric | `ea0f707` | `af3833a` | Δ |
| --- | ---: | ---: | ---: |
| Create p50 | 15.739 s | 16.004 s | +0.265 s |
| **Create p95** | **36.162 s** | **26.074 s** | **−10.088 s** |
| Create max | 38.422 s | 26.995 s | −11.427 s |
| Wall time | 59.728 s | 48.2 s | −11.5 s |
| Demand → usable capacity | ~23.2 s | ~13.0 s | −10.2 s |
| Largest adjacent-sample gap | 6.664 s | 1.297 s | −5.367 s |
| Queued (slow-mode) creates | 64 | 39 | −25 |

160/160 succeeded with zero errors and verified cleanup in both runs. The
improvement matches the predicted ~10 s of autoscaler decision latency almost
exactly, and the distribution is no longer bimodal — capacity now arrives fast
enough that queued creates blend into the placed ones.

Exactly one resize was issued for the whole burst
(`sandbox_direct_scale_out_total 1`, `..._failed_total 0`), logged as
`direct scale-out requested 5 workers (live=2 occupied=96 queued=64)`, so the
coalescing and the demand+1 ceiling both held — no ratchet above demand.

The remaining ~13.0 s is worker resume to advertised capacity. That is now the
hard floor for any request exceeding ready capacity, so further gains need
[improvement 6](#6-maintain-a-prepared-microvm-pool) or a larger ready
headroom, not a faster control loop.

**Scale-in is capped, not disabled.** The policy target is
`min(max_over_time(sandbox:workers_desired[window]), max(hosts_live,
sandbox_scale_out_requested))`. A low `desired` still drives scale-in normally.
The watermark term is required: for ~13 s after a resize the new workers exist
but have not heartbeated, so capping on `hosts_live` alone would scale the
fleet back down mid-burst.

#### Known gap: the cap does not actually make the autoscaler scale-in-only

Measured on this same run, ~2.5 min after the burst finished, the autoscaler
still scaled **out**:

```text
06:56:47 policy_handler: from=5 to=6 reason="scaling up because metric is 6"
06:57:08 unable to scale target: ... failed to confirm scale out GCE Instance
         Group: reached retry limit
```

The cap was permissive because `sandbox_hosts_live` (8) exceeded the MIG's
`targetSize` (5): resumed standby workers heartbeat to the gateway without
being part of the target the autoscaler compares against. So the ceiling was 8,
`max_over_time(desired[15m])` had latched the burst peak of 6, and
`min(6, 8) = 6 > 5` was a legal scale-up.

This did **not** affect the p95 result — the gateway issued the single resize
that served the burst, and this fired after the burst had drained — but it
reproduces the known "over-scales after demand is gone" behavior, since
`max_over_time` replays a stale peak for the whole window. The invariant that
`validate-scaling-owner.sh` and the surrounding comments describe is therefore
**not yet enforced**.

The fix is to cap on the MIG's real `targetSize` rather than a heartbeat-derived
proxy. The gateway already holds the GCE client and the IAM permission, so it
can read `targetSize` and export it as `sandbox_mig_target_size`, making the
ceiling exact and removing the need for the `hosts_live`/watermark maximum
entirely. Not yet implemented.

One further defect is visible at 36.4 s: a resumed standby worker
(`32dc09d1`) first heartbeats advertising release `a97b68f` with
`release_compatible:false`, and only re-heartbeats as `a223889` 2.5 s later.
That is [improvement 7](#7-keep-standby-workers-release-compatible) — standby
workers suspended before a roll come back stale and are briefly ineligible.

### Current request path

For a create that fits on an existing worker:

```text
client
  → gateway
    → reserve capacity in memory
    → proxy POST /sandboxes directly to one worker
      → acquire worker create semaphore
      → allocate tap/IP/port and insert the SQLite row
      → reflink the golden rootfs
      → create an unbridged tap
      → load and resume the golden Firecracker snapshot
      → guest adopts its new IP/MAC from MMDS
      → wait for the guest's gratuitous ARP announcement
      → attach the tap to br-fc
      → persist VM runtime state
      → wait for sandboxd to answer
  ← return 201
```

The relevant implementation is:

- gateway reservation and bounded queue:
  [`internal/gateway/gateway.go`](../internal/gateway/gateway.go);
- grow-only GCE MIG client:
  [`internal/gcemig/scaler.go`](../internal/gcemig/scaler.go);
- golden clone and reidentification path:
  [`internal/server/snapshot.go`](../internal/server/snapshot.go)
  (`bringUpClone` and `finishClone`);
- tap lifecycle:
  [`internal/provisioner/provisioner.go`](../internal/provisioner/provisioner.go);
- per-worker create concurrency:
  [`internal/server/server.go`](../internal/server/server.go);
- golden adoption/build:
  [`internal/server/golden.go`](../internal/server/golden.go).

The rootfs clone is already copy-on-write on the fleet's XFS data disk. The
remaining hot-create work is mainly process/runtime setup, network identity,
registry operations, and readiness.

### Current overflow path

When every registered slot is reserved:

```text
create enters gateway queue
  → Prometheus scrapes queued demand
  → Nomad Autoscaler computes the target and submits the resize
  → GCE resumes suspended standby, starts stopped standby, or creates a VM
  → Nomad allocation/serve process becomes current-release compatible
  → worker adopts or builds its golden snapshot
  → worker heartbeat advertises slots_free > 0
  → gateway wakes queued requests
  → normal per-worker create path
```

Nomad Autoscaler owns both directions of the production MIG target. The
gateway's optional direct scaler is not enabled on GCP, maintaining a
single-writer invariant.

## What Modal's result means

Modal's published million-sandbox test created one million sandboxes in under
a minute and sustained tens of thousands of create requests per second. That
result primarily demonstrates **scheduling and sandbox-start throughput over
an already acquired worker fleet**. It does not show one million cloud VMs
being acquired from zero in under a minute.

Modal's create-request distribution stops when a scheduler has assigned the
sandbox to a worker and startup has begun; its separate time-to-interactivity
measurement runs until user code can execute. This project's synchronous
`POST /sandboxes` has the stricter second meaning: it does not return until
`sandboxd` answers. Compare the Modal TTI distribution, not only its scheduling
distribution, with this project's create latency.

Their public design has several relevant properties:

- a horizontally scaled fleet of scheduling servers performs an in-memory
  load-balancing decision;
- schedulers contact workers directly, and workers accept or reject the
  optimistic placement based on their actual free resources;
- each worker is authoritative for its own state and publishes updates
  asynchronously;
- no durable datastore is on the sandbox creation path;
- control operations that would otherwise be one RPC per sandbox are batched;
- networking was redesigned after simultaneous container starts contended on
  Linux's global `rtnl` lock.

Source: [Scaling to 1 million concurrent sandboxes in seconds](https://modal.com/blog/scaling-to-1-million-concurrent-sandboxes-in-seconds).

Cloud host acquisition is a separate system. Modal says new cloud servers can
take minutes, so it maintains idle capacity that bursts can enter immediately
while acquiring replacement capacity in the background. Its resource solver
optimizes provider, instance type, availability, and price, but the latency
mechanism is still a buffer of ready compute.

Source: [Linear programming for fun and profit](https://modal.com/blog/resource-solver).

Modal also has a different execution boundary. Its Sandboxes use gVisor
containers, a lazy/network-backed filesystem with worker caches, and optional
memory snapshots. This project deliberately uses Firecracker microVMs, giving
each sandbox a dedicated kernel at a higher per-sandbox setup and resource
cost. The useful comparison is therefore the architecture around capacity and
scheduling, not an assumption that both runtimes have identical costs.

Sources:
[container launches](https://modal.com/blog/speeding-up-container-launches),
[memory snapshots](https://modal.com/blog/mem-snapshots), and
[sandbox networking/security](https://modal.com/docs/guide/sandbox-networking).

Fly describes the same operational principle for Firecracker Machines:
prepare machines and images ahead of demand, then keep the request path close
to a direct local Firecracker start.

Source: [Fly Machines: an API for fast-booting VMs](https://fly.io/blog/fly-machines/).

## The latency/cost boundary

No reactive scaler can provide sub-second capacity from an infrastructure
operation that takes 8–9 seconds. For requests above current ready capacity,
the best possible latency is approximately:

```text
overflow TTI =
  scale decision
  + provider resume/start
  + worker eligibility
  + queue drain
  + sandbox create
```

To remove a term from the user-visible path, pay it before the request:

| Capacity state | Cost while idle | Approximate response behavior |
| --- | --- | --- |
| Fresh VM not created | lowest | provider create + full worker boot |
| Stopped standby | disk | provider start + worker startup |
| Suspended standby | memory/storage policy dependent | current 8–9 s resume floor |
| Running empty worker | full host | sandbox create only |
| Prepared/paused sandbox | host + sandbox resources | claim/resume only |

Consequently, the SLO must drive the amount and kind of buffer. If 160
simultaneous requests must all be ready in under one second, there must already
be close to 160 prepared execution slots. If an 18-second overflow is
acceptable, suspended standby is much cheaper.

## Improvement plan

### 1. Define the SLO and size ready headroom

**Operational; no code required initially.**

The checked-in fleet defaults set `HEADROOM_SLOTS` to one 48-slot worker, while
`LEAD_SECONDS` adds a create-rate forecast to the Prometheus reconciliation
path. See
[`infra/gcp/config.env.example`](../infra/gcp/config.env.example) and
[`infra/gcp/prometheus/rules.yml.tpl`](../infra/gcp/prometheus/rules.yml.tpl).
Size the ready buffer from observed demand rather than picking a round number:

```text
ready_headroom =
  p99 creates arriving during the worker reaction window
  + capacity unavailable during one worker failure
  + safety margin
```

The reaction window should be measured separately for suspended, stopped, and
fresh workers. The current suspended value is about 9 seconds. Scheduled RL
rollouts or known agent batches should pre-scale before their start time rather
than waiting for the first queued create.

For the existing 160-create benchmark:

- two running workers expose 96 immediately usable slots;
- four running workers expose 192 slots and keep the worker-resume path out of
  the benchmark;
- the resulting test measures worker-local burst throughput rather than GCE
  resume latency.

Keep suspended standby behind the running buffer for larger, less frequent
overflow. This is a two-tier capacity policy:

```text
running ready headroom → suspended overflow → stopped/fresh overflow
```

Success criterion: the chosen percentile of normal bursts never enters
`sandbox_create_queue_depth`; exceptional bursts still complete within
`QUEUE_WAIT`.

### 2. Make direct scale-out level-triggered

**Proposed; small/medium implementation.**

Today direct scaling is triggered only when queue depth changes from zero to
one. The 50 ms debounce sizes the action from the demand visible at that
moment. If demand continues to grow while the queue remains non-empty, there is
no second edge; the Prometheus/Nomad loop must eventually reconcile it.

Replace the edge trigger with a grow-only desired-capacity watermark:

```text
demand_slots =
  occupied
  + in-flight reservations
  + queued creates
  + predicted near-term creates
  + headroom

desired_workers = ceil(demand_slots / slots_per_host)

if desired_workers > last_requested_workers:
    request MIG resize(desired_workers)
```

Recompute when:

- a request enters the queue;
- queue depth crosses a worker-sized boundary;
- reservations grow materially;
- a previous resize finishes or fails;
- a worker becomes eligible or expires;
- an explicit forecast changes.

Serialize resize requests and coalesce them to the highest desired value.
`gcemig.Scaler` should retain its grow-only behavior. It can cache the last
successfully requested target to avoid a MIG GET on every event, with a
periodic provider read as reconciliation.

Tests should cover:

- demand arriving in several waves while the queue never returns to zero;
- concurrent triggers coalescing to one highest target;
- desired capacity falling while a resize is in flight (never shrink);
- provider target already larger than the gateway's cached target;
- retry after timeout without issuing a smaller resize;
- `MIG_MAX` saturation.

This change improves large and rolling bursts. It does not remove the provider
resume time for the first overflow worker.

### 3. Add create-stage latency metrics

**Proposed; do before changing the worker pipeline.**

Worker boot phases are already measured, but the service does not export HTTP
request-duration or hot-create stage histograms. Add histograms for:

- gateway queue wait;
- gateway placement/proxy time;
- worker create-semaphore wait;
- registry allocation;
- rootfs reflink;
- tap create;
- Firecracker process/configure/load/resume;
- reidentification announcement;
- bridge attach;
- final registry write;
- agent readiness.

Record the create path (`hot`, `cold`, or later `prepared_pool`) as a bounded
label. Do not label metrics with sandbox ID, tap, IP, snapshot ID, or host name
inside the worker; those are unbounded-cardinality values.

Useful outputs:

```text
sandbox_create_duration_seconds{path="hot"}
sandbox_create_stage_duration_seconds{path="hot",stage="reidentify"}
sandbox_gateway_queue_duration_seconds
sandbox_gateway_create_duration_seconds
```

The gateway already federates per-host metrics with a `host` label, so worker
histograms can be aggregated fleet-wide without adding dynamic Prometheus
targets.

Success criterion: p50/p95/p99 can be attributed to stages during both a
single-create test and the 160-create burst. Optimization decisions should use
these histograms rather than log timing from one run.

### 4. Pre-create and recycle tap devices

**Proposed; medium implementation.**

Every hot create currently forks `ip` for tap creation, link-up, and bridge
attachment. A burst therefore creates process overhead and repeated operations
under the kernel's `rtnl` lock. Modal independently identified the same lock as
a major tail-latency source.

At worker startup, prepare one tap for each runnable slot and recycle taps
rather than deleting them:

```text
startup:
  create slot tap → keep down/detached or otherwise isolated

create:
  reserve tap in registry → bring up/attach at the safe identity point

destroy:
  detach, flush state, return tap to free pool
```

Use a native netlink library instead of spawning `ip` processes, but treat that
as a secondary improvement: it removes fork/exec overhead, not kernel lock
contention.

Design constraints:

- a golden clone must remain off `br-fc` until it has shed the snapshot's baked
  IP, or duplicate identities can briefly appear on the bridge;
- startup reconciliation must distinguish an intentionally idle pre-created
  tap from stale state;
- deleting a sandbox must remove neighbor/FDB state before returning a tap;
- partial startup failure must not leak a tap reservation;
- the prepared pool must agree with registry pool names and `SLOTS_PER_HOST`.

An initial implementation can pre-create unbridged taps and retain the existing
GARP gate. A later slot-specific snapshot can eliminate reidentification too.

Success criterion: the tap-related stage p99 remains nearly flat as create
concurrency rises, and no duplicate-IP traffic appears in packet-capture or
stress tests.

### 5. Pipeline worker creation by bottleneck

**Proposed; medium implementation after stage metrics.**

The current `createSem` protects the entire bring-up. This is safe, but a
request holds its permit while waiting for operations with very different
resource profiles.

Split the flow into bounded stages:

| Stage | Suggested policy |
| --- | --- |
| SQLite allocation | short transaction; serialize only where required |
| XFS reflink | high concurrency |
| Firecracker load/resume | limit from measured CPU/KVM pressure |
| netlink/tap mutation | low concurrency due to `rtnl` |
| GARP/agent wait | asynchronous; do not hold the netlink permit |

Retain an overall admission bound so a burst cannot create an unbounded number
of Firecracker processes or readiness goroutines. The goal is not unlimited
parallelism; it is to avoid holding a scarce network or CPU permit while
waiting on an unrelated guest event.

Success criterion: a 48-slot worker drains a full burst faster without
increasing agent timeouts, create failures, host CPU saturation, or network
identity failures.

### 6. Maintain a prepared microVM pool

**Proposed; large implementation and the largest worker-local latency win.**

The existing measurements show that the primitive is viable:

- same-identity hibernation wake: about 49 ms server-side;
- pure Firecracker snapshot resume: about 14 ms;
- 1:1 restore: roughly 84–212 ms end to end depending on the run;
- identity-neutral golden clone: roughly 200–500 ms when unsaturated.

Maintain a small pool of already-restored, agent-ready microVMs per worker:

```text
background replenisher
  → clone golden snapshot
  → assign final slot tap/IP/MAC
  → wait for sandboxd
  → pause VM
  → publish as prepared

create request
  → atomically claim prepared VM
  → assign public sandbox ID/name/TTL/request metadata
  → resume
  → verify agent
  → return 201
  → replenish pool asynchronously
```

This moves Firecracker load, guest reidentification, bridge attachment, and
agent startup off the request path. A prepared VM consumes real host resources,
so this is a latency/cost trade rather than a free optimization.

#### Identity and registry model

A prepared VM must not appear as a user sandbox before it is claimed. Store it
in a separate pool table or explicit internal state, not as a normal
`running` sandbox returned by list APIs. Claiming must atomically:

1. remove the pool entry;
2. create the user-visible registry row;
3. transfer ownership of tap/IP/rootfs/socket/PID;
4. install TTL, name, and hibernation policy;
5. add it to `machines` and activity tracking.

On a server crash, normal reconciliation can destroy unclaimed pool VMs. Pool
entries do not need durability.

#### Resource shapes

Firecracker snapshots bake vCPU and memory configuration. Either:

- maintain pools only for the default resource shape and let overrides
  cold-boot, matching current behavior; or
- maintain a bounded pool per popular `(vcpus, mem_mib, image generation)`
  shape.

Do not create arbitrary shape pools from untrusted request values. Use demand
history and an explicit memory budget.

#### Slot-specific snapshots

An alternative or complement is one golden derivative per slot identity. Each
snapshot has its final tap/IP/MAC baked in, so create can use the faster
identity-preserving restore rather than MMDS reidentification and GARP.
Artifacts can share the same underlying memory/rootfs data where Firecracker
and the filesystem permit it.

This avoids keeping every prepared VM process alive but increases snapshot
generation and lifecycle complexity. Prototype both approaches and compare:

- claim latency;
- idle RSS/PSS and file descriptors;
- replenish throughput;
- rollout invalidation cost;
- artifact disk usage;
- behavior across server restart.

Success criterion: prepared-pool claim p99 is below the normal hot-create p50,
and pool misses fall back safely to the existing golden-clone path.

### 7. Keep standby workers release-compatible

**Operational/deployment improvement.**

A suspended worker can resume an old Nomad allocation. The gateway correctly
advertises zero free capacity until the worker reports the expected release,
but replacing that allocation adds work to the resume path.

For a rollout:

1. replenish or resume standby workers;
2. start the new worker allocation and validate golden readiness;
3. suspend them again;
4. only then switch the gateway's expected worker release;
5. roll active workers.

The invariant is that standby capacity should already contain the release it
will advertise after resume. An immutable worker/golden generation can make
this easier: bake `sandboxd`, compatible golden artifacts, and worker startup
requirements together, while continuing to roll the server binary only when
the golden format remains compatible.

Success criterion: a standby resume does not require an allocation replacement
before `slots_free` becomes nonzero.

## Scaling the control plane further

The current gateway already implements several properties of Modal's design:

- placement uses in-memory heartbeat state;
- it reserves capacity before proxying;
- the selected worker is contacted directly;
- a worker may reject the request and the gateway can try another host;
- there is no shared fleet database in the placement path.

For the current target of roughly 1,000 concurrent sandboxes, the singleton
gateway and per-host SQLite are unlikely to be the first latency bottlenecks.
Measure before redesigning them.

At tens of thousands of workers or very high create rates, evolve toward:

```text
API load balancer
  → many stateless placement schedulers
    → direct worker RPC

workers
  → asynchronous state stream
    → routing index, billing, durability, and observability consumers
```

Required changes at that scale:

- partition or replicate the sandbox-ID routing index;
- make schedulers tolerate stale worker state and retry rejected placements;
- remove synchronous durable metadata writes from "interactive";
- batch lifecycle/control traffic;
- shard worker-state streams before any single stream becomes a bottleneck;
- separate the admission response ("assigned and starting") from the readiness
  response if API semantics allow it.

This is intentionally not the next implementation step. It improves aggregate
throughput and availability, not the current 8–9 second provider resume span.

## Recommended order

1. Choose burst and steady-state SLOs; tune running headroom to meet them.
2. Export per-stage create and gateway queue histograms.
3. Replace the queue-edge direct scaler with a desired-capacity watermark.
4. Pre-create/recycle taps and isolate netlink concurrency.
5. Split worker bring-up into separately bounded stages.
6. Prototype a small default-shape prepared microVM pool.
7. Make suspended standby release-compatible before suspension.
8. Revisit scheduler sharding only after gateway load tests show a real limit.

## Benchmark matrix

Run the same workload after every material change:

| Scenario | What it isolates |
| --- | --- |
| Sequential hot creates on one worker | uncontended sandbox floor |
| 48 simultaneous creates on one empty worker | worker drain and network contention |
| 160 creates with four ready workers | fleet placement + worker throughput |
| 160 creates with two ready + suspended standby | full reactive scale-out |
| Two-stage burst while queue stays nonzero | scaler watermark behavior |
| Burst during worker rollout | release gate and standby compatibility |
| Pool hit / pool miss | prepared-pool benefit and fallback |

Report p50/p90/p95/p99/max, failures by class, queue duration, each create
stage, resize request time, worker capacity-advertised time, and total time
until `sandboxd` answers. "Created" must continue to mean interactive, not
merely assigned.

Raw results belong under `sdk/typescript/benchmarks/results/`, and summarized
evidence belongs in [Benchmarks](benchmarks.md).
