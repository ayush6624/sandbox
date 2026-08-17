# Absorbing a 20–100 sandbox arrival burst

Status: proposed (2026-08-17). Companion to `warm-templates-plan.md`, which
covers making *template* creates as fast as default ones. This document is about
the burst itself.

## The capacity model

Per worker (`WORKER_MACHINE_TYPE="n2-standard-16"` — 16 vCPU / 64 GiB):

| Constant | Value | Where |
|---|---|---|
| Slots (tap/IP pool) | 48 | `SLOTS_PER_HOST`, config.env |
| Committed memory per slot | 1180 MiB | `MEM_PER_SLOT_MIB` default (commented out in config.env) |
| `mem_budget_mib` | 56,640 MiB (55.3 GiB) | `SLOTS × MEM_PER_SLOT` |
| Nomad task cgroup | 58,640 MiB (57.3 GiB) of 64 GiB | `TASK_MEMORY` |
| Default guest | 2 vCPU / 1024 MiB | `configs/devbox.json` |
| Concurrent bring-ups | 24 | `CREATE_CONCURRENCY` default, deploy-job.sh:94 |
| Ready pool | 8 | `warm_pool_size` |
| Max clones per fanout call | 8 | `fanoutParallelism` |

Slots and memory agree exactly at 48: a default guest commits 1024 + 156 MiB
overhead = 1180 MiB, so 48 running sandboxes saturate both ceilings at once.
That is deliberate and worth preserving when either knob moves.

**Ready rows consume burst capacity.** A warm VM is a real running VM holding a
slot, tap, IP, and committed memory that `FreeSlots` counts. So a host at
`warm_pool_size: 8` offers 8 instant creates and **40** further slots, not 48.
This is the central tension in everything below.

## What actually happens at each size

Assume the documented floor of 2 hosts, default template, pools full.
Fleet-instant capacity = 2 × 8 = **16 ready rows**; fleet slots = 2 × 48 = **96**.

### 20 requests — already fine
16 claims at **7–16 ms**, 4 fall through to ordinary clones at ~700 ms, pool
refills in the background. Sub-second overall. No change needed.

### 50 requests — latency-bound, works, with a tail
16 claims at ~10 ms. 34 clones against 48 fleet create permits, so they run
essentially in parallel: ~700 ms–1.4 s. Slots fit (50 ≤ 96).

The risk is not throughput, it is **guest CPU**. 34 concurrent bring-ups across 2
hosts is ~17 per 16-vCPU host, each doing a guest-side thaw, `eth0`
reconfiguration and Ed25519 keygen — plus 16 pool refills competing for the same
`createSem`. `finishClone` already carries a *second* 1.5 s reidentify margin
specifically because a CPU-starved guest misses the first one, which is direct
evidence this regime degrades. Expect a tail, not failures.

### 100 requests — capacity-bound, and this is the cliff
96 fleet slots, of which 16 are already held by ready rows. So ~96 of the 100 can
be placed and the remainder **cannot** — they enter the bounded queue
(`--queue-wait`/`--queue-max`, 240 s/4096) and wait for a scale-out.

That wait is the whole problem. Scale-out costs a **~189 s blackout** (measured
2026-07-25; the "150 s → 21 s" claim was disproved — 21 s is only when the
confirm error logs, and consecutive `calculating scaling target` lines measured
188.87 s and 188.94 s). So the last handful of requests in a 100-burst wait
*minutes* while the first 96 completed in ~1 s.

**The failure is at the margin, not in the middle.** Optimizing clone latency
does nothing for it.

### Same bursts from a user template (today)
Zero ready rows, so *every* request is a ~700 ms clone, plus a first-touch GCS
pull on each host that doesn't hold the template yet. That is what
`warm-templates-plan.md` fixes; it is orthogonal to this document.

## Principle: bursts are absorbed by headroom, not by reacting to them

A ~189 s scale-out can never be on a request's critical path. So the goal is to
make scale-out a *background* activity that keeps a target amount of free
capacity ahead of arrivals, rather than something a burst triggers and waits for.

Restated as an invariant:

> Keep `fleet_free_slots ≥ H` at all times, where H is the largest burst we
> intend to absorb without queueing.

For H = 100 that is ~3 spare hosts' worth of slots beyond steady demand. That is
a **cost decision, not an engineering one** — spell out the bill and let the
operator choose H.

## Changes, in order of value

### 1. Headroom-triggered scale-out (the one that matters)

Today the gateway scales on `fleetDemand()` — worker-reported `demand_slots` plus
queued `mem_mib`-weighted overrides. Queue depth is a **lagging** signal: by the
time it is nonzero, requests are already waiting through the blackout.

Add a leading term: scale out when `fleet_free_slots < H`, independent of whether
anything is queued. `H` becomes a config knob (`FLEET_HEADROOM_SLOTS`, default
= `SLOTS_PER_HOST`, i.e. one spare host).

The gateway is already the sole MIG writer and already has every input
(per-host `slots_free` from heartbeats), so this is a new term in an existing
decision, not new machinery. Two constraints to respect:
- Scale-in must use the **same** headroom figure or the fleet will oscillate:
  scale-out at `free < H`, scale-in only when `free > H + SLOTS_PER_HOST`.
- Never cap the autoscaler on `hosts_live` (an existing, documented trap).

### 2. Do not over-pool — and prove the sizing

The tempting fix for a 50-burst is `warm_pool_size: 25`. Resist it: 25 ready rows
per host is ~29 GiB resident doing nothing and **reduces the host's remaining
slots from 40 to 23**, so a large burst gets *less* capacity, not more. Ready
rows help the first N arrivals and hurt everything after.

Rule of thumb to encode: size the pool for the *arrival rate over one clone
latency* (~700 ms), not for the total burst. A 50-request burst arriving over 2 s
needs roughly 8–16 ready rows fleet-wide, which is what we already have.
`sandbox_warm_misses_total` is the signal; raise the pool only when misses are
sustained, and re-check `slots_free` after every change.

### 3. Measure the guest-CPU ceiling before touching concurrency

`CREATE_CONCURRENCY` defaults to 24 on a 16-vCPU host. (Note: benchmark artifacts
from release `c0d0c0f` recorded `create_concurrency: 12`, so the effective fleet
value must be confirmed — the two disagree and the spec below assumes whatever is
actually deployed.)

Before raising 24 *or* `fanoutParallelism`, measure reidentify latency as a
function of in-flight bring-ups. The instrumentation already exists — the
per-clone phase log prints `reidentify=`, and the second margin firing is
recorded. If reidentify at 24-way is materially above the measured 333–399 ms
baseline, the host is already past its useful concurrency and **the correct move
is more hosts, not more permits**. Publish the curve; do not guess at it.

### 4. Confirm graceful degradation is actually graceful

The pieces exist and mostly need verification under burst rather than new code:
- Queue: `--queue-wait` 240 s / `--queue-max` 4096, depth exported as
  `sandbox_create_queue_depth`, 503 + `Retry-After` on expiry.
- Capacity-class failover: a 503/429 or connection failure fails over to the next
  best host (≤3 attempts, failing host penalized ~2 heartbeats); genuine host
  errors return 502 without retry.
- A host still warming advertises `slots_free=0`, so it is never handed a create
  it cannot serve.

What to verify: that a burst exceeding capacity produces **queueing then
Retry-After**, never 500s, never a partially-created sandbox, and that
`sandbox_create_queue_depth` rises (a rejected create that never enqueues leaves
the scale-out signal at 0 — exactly the defect that cost 28 of 89 trials on the
Terminal-Bench sweep).

### 5. Reduce the blackout itself (harder, still worth it)

189 s matches the background standby-replenish window rather than
`cooldown = "1m"`. Until that is understood, headroom (change 1) is the mitigation
rather than the cure. **Re-verify any blackout claim by the gap between
consecutive `calculating scaling target` lines, never by the error timestamp** —
that mistake has already been made once in this repo.

## Test plan

A burst harness driven **from the control VM** (a laptop tunnel adds hundreds of
ms of RTT that reads as VM-creation cost):

| Case | Assert |
|---|---|
| 20, default, pools full | p95 < 100 ms; ≥16 pool hits |
| 50, default | all 50 usable; record clone p50/p95 and the reidentify distribution |
| 100, default, 2 hosts | no 500s; the overflow queues and either lands or gets Retry-After; `queue_depth` > 0; scale-out fires |
| 100, default, with headroom ≥ 100 | no queueing at all — the point of change 1 |
| 50, user template, pool 0 vs pool 8 | quantifies `warm-templates-plan.md` |
| 100 then idle | fleet returns to baseline; no leaked taps/IPs/rootfs; scale-in does not oscillate |

Every case also checks cleanup, since a burst that leaks resources fails the next
burst instead of its own.

## What is measured vs. derived here

**Measured** (release `c0d0c0f`, from the in-VPC control VM): ready-pool claims
7–16 ms; refill-bound creates 734 ms / 984 ms / 1.381 s; snapshot-source create
p50 696 ms; 16-way hold burst 16/16 at p50 79 ms / p95 114 ms; 128-way fleet
burst 128/128; reidentify 333–399 ms; SSH identity 125–136 ms. Scale-out blackout
~189 s (2026-07-25).

**Derived, not measured** — every burst estimate above (the 50-request 1–1.4 s,
the 100-request cliff arithmetic, the CPU-contention tail). They follow from the
constants table, but no 20/50/100 burst has been run against the current release
with the snapshot-lock fix in place. Treat the numbers as the hypothesis the test
plan exists to falsify.
