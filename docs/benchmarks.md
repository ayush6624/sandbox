# Benchmarks

**Updated 2026-07-28.** The current hardened-release matrix is recorded in
[2026-07-27 hardened-release validation](#2026-07-27-hardened-release-validation);
its autoscaling latency gap was closed on 2026-07-28 by moving scale-out to the
gateway (see the held-burst table in that section).
The older instrumented headline/detail numbers were measured 2026-07-01 →
2026-07-12 (server release `b801d6d`+); a full end-to-end SDK re-run on that
fleet (commit `06f5c16`) is in
[2026-07-23 fleet re-run](#2026-07-23-fleet-re-run-commit-06f5c16), followed by
the historical [2026-07-24 stopped-worker burst](#2026-07-24-stress-and-autoscaling-burst-commit-1cefc65)
and the release-gated
[2026-07-25 suspended-standby burst](#2026-07-25-suspended-standby-and-event-driven-scale-out-release-releasegate-20260725-1-commit-0b049db).
The latest correctness and traffic-pattern validation is
[the 2026-07-25 autoscaling traffic suite](#2026-07-25-autoscaling-traffic-suite-gateway-52a94cc-workers-820c3e4).
Interactive version: [`benchmark-report.html`](./benchmark-report.html)
(published at <https://claude.ai/code/artifact/f14de3c5-96c3-45d1-bc7d-1a4ce4ccf6b3>).

## Headline

Only the burst-churn and autoscaling-held-burst rows below are current
2026-07-27 hardened-release results. The other rows are historical headlines
retained for comparison.

| Metric | Result | Clock |
|---|---|---|
| Hot create (golden-snapshot clone) | **199–271 ms** server-side, ~0.55 s end-to-end via gateway | create request → in-guest agent answers |
| Hibernation wake, same identity | **49 ms** server-side | wake start → agent answers |
| Wake-on-connect (forwarded port) | **133 ms** | TCP connect to host port → guest responds, incl. wake |
| Snapshot-source create (legacy 1:1 route) | **212 ms p50** (VMM resume ~14 ms; rest is rootfs reflink) | create request → agent answers |
| Snapshot batch create | **83 ms/sandbox** amortized (32 in 2.68 s), 64/64 usable at N=64 | batch request → all agents answer |
| Diff snapshot write | **123 ms** (vs ~1.5 s full); uploads ~24× smaller | pause → snapshot written |
| Cold boot (baseline) | 3.46 s p50 (GCP), ~2.2 s (Hetzner bare metal) | create request → agent answers |
| Burst churn | **500/500**, 0 errors, 5.0 creates/s sustained | 500 create→exec→terminate @ concurrency 96, hardened release |
| Autoscaling held burst | **160/160**, 0 errors; p50 16.004 s / **p95 26.074 s** / max 26.995 s | correctness, 60 s maximum, and the 30 s p95 SLO all pass |
| Autoscaling traffic validation | **896/896 creates**, **31,256/31,256 connect+identity-exec probes**, 0 failures | standby-boundary, second-wave, long-lived reconciliation, and churn traffic |

Historical headline environment: GCP `n2-standard-8` hosts (8 vCPU / 32 GB,
nested KVM), guests 2 vCPU / 1 GB, Firecracker v1.15.0, XFS reflink storage;
client on the same tailnet. "Usable" always means the in-guest agent answers —
never "the create call returned".

## 2026-07-27 hardened-release validation

This is the current production-readiness baseline. The worker artifact was
release `a223889`; the final gateway/autoscaler held-burst run was commit
`ea0f707`. Tests ran on the GCP production fleet's `n2-standard-16` workers
with 48 slots per worker, 2 vCPU / 1 GiB guests, nested KVM, and XFS reflink
storage. The direct-worker matrix used the authenticated private management
path; fleet tests used the gateway. All counts below include an in-guest
usability check, not merely a successful create response.

All benchmark sections dated before 2026-07-27 are retained as **historical**
comparisons. They describe different releases and must not be used as the
current production acceptance result.

### Snapshot source and snapshot batch create

Twenty-five paired creates showed no latency advantage for a snapshot source
in this hardened configuration:

| Source | Mean | p50 | p90 | Min | Max |
|---|---:|---:|---:|---:|---:|
| Default source | 1.989 s | 1.906 s | 2.614 s | 1.432 s | 3.776 s |
| Snapshot source | 2.031 s | 1.927 s | 2.761 s | 1.448 s | 3.582 s |

Snapshot batch create used `max_parallelism=32`. Every operation succeeded,
every returned sandbox became command-ready, and every sandbox and snapshot
was cleaned up:

| N | Batch operation | Readiness | Total | Per-sandbox | Usable |
|---:|---:|---:|---:|---:|---:|
| 1 | 2.247 s | 0.226 s | 2.473 s | 2.473 s | 1/1 |
| 2 | 2.262 s | 0.406 s | 2.668 s | 1.334 s | 2/2 |
| 4 | 2.943 s | 0.419 s | 3.362 s | 0.841 s | 4/4 |
| 8 | 3.631 s | 0.412 s | 4.043 s | 0.505 s | 8/8 |
| 16 | 7.239 s | 0.851 s | 8.090 s | 0.506 s | 16/16 |
| 32 | 13.251 s | 1.068 s | 14.319 s | 0.447 s | 32/32 |
| 48 | 20.475 s | 0.896 s | 21.371 s | 0.445 s | 48/48 |

The N=48 default-source batch baseline completed in 18.100 s, or 0.377 s per
sandbox. Snapshot batch create therefore remains a correctness and shared-source
abstraction in this release, not a demonstrated create-latency optimization.

### Fleet workload matrix

Every fleet run created the requested count, completed its verified in-guest
workload, and proved per-sandbox cleanup. The 128-sandbox run scaled beyond the
two-worker floor.

| Count / mode | Create p50 | Create p95 | Create max | Workload wall p50 / p95 | Fleet wall | Result / cleanup |
|---|---:|---:|---:|---:|---:|---:|
| 32 default | 3.472 s | 5.912 s | 6.425 s | 49.240 / 51.265 s | 63.313 s | 32/32, 32/32 |
| 64 default | 3.258 s | 5.398 s | 5.605 s | 83.138 / 87.954 s | 107.874 s | 64/64, 64/64 |
| 128 default | 2.973 s | 5.109 s | 6.045 s | 77.267 / 81.184 s | 116.053 s | 128/128, 128/128 |
| 64 fsync | 3.323 s | 5.347 s | 5.890 s | 48.369 / 50.350 s | 177.097 s | 64/64, 64/64 |
| 64 large | 3.437 s | 5.456 s | 6.319 s | 222.478 / 229.021 s | 249.565 s | 64/64, 64/64 |

The cleanup verdict is resource-level: every created sandbox was verified
absent. Immediate host heartbeats can retain stale occupancy while deletion
reconciliation completes, so they are not used as the cleanup clock.

### Memory density

At N=48, snapshot-source sandboxes used 4,708 MiB RSS / 4,708 MiB PSS versus
5,228 MiB RSS / 5,227 MiB PSS for default-source sandboxes. That is 520 MiB
less RSS and 519 MiB less PSS, about a 10% reduction (1.11× density), with all
48 Firecracker processes present in both arms and zero remaining afterward.

### Sustained churn burst

The 500-sandbox create → exec → terminate burst at concurrency 96 completed
**500/500** with zero capacity, pool, agent-readiness, workload, or termination
errors. Wall time was 100.092 s (5.0 completed sandboxes/s).

| Phase | Mean | p50 | p90 | p95 | p99 | Max |
|---|---:|---:|---:|---:|---:|---:|
| Create | 9.600 s | 9.435 s | 13.216 s | 22.119 s | 24.656 s | 26.466 s |
| Exec | 8.393 s | 8.478 s | 12.095 s | 13.748 s | 14.658 s | 15.923 s |
| Terminate | 103 ms | 91 ms | 165 ms | 206 ms | 312 ms | 531 ms |

### Autoscaling held burst

The hardened campaign exposed an under-scaling interaction, proved the
committed demand-sizing fix, and then repeated the canonical 160-create run at
gateway/autoscaler commit `ea0f707`:

| Run | Creates / errors | Wall | p50 | p90 | p95 | p99 | Max | Acceptance |
|---|---:|---:|---:|---:|---:|---:|---:|---|
| Initial hardened run (`2ca73c9`) | 160/160, 0 | 253.805 s | 15.820 s | 229.977 s | 231.297 s | 232.534 s | 232.699 s | correctness pass; p95 and max fail |
| Committed sizing fix (`e0b3588`) | 160/160, 0 | 58.599 s | 15.737 s | 35.165 s | 36.468 s | 37.089 s | 37.268 s | correctness/max pass; p95 fail |
| Autoscaler-only canonical run (`ea0f707`) | 160/160, 0 | 59.728 s | 15.739 s | 35.056 s | 36.162 s | 37.528 s | 38.422 s | correctness/max pass; p95 fail |
| Gateway-owned scale-out (`af3833a`) | **160/160, 0** | **48.2 s** | **16.004 s** | — | **26.074 s** | **26.805 s** | **26.995 s** | **all pass, incl. the 30 s p95 SLO** |

In the initial run, the two warm workers served 96 requests and the first
scale action added only one 48-slot worker. Sixteen requests then remained
queued for roughly 3.8 minutes. The sizing fix requested enough capacity in
one action, removed that pathological tail, and preserved zero-error cleanup.

The `ea0f707` run passed correctness and the create-max ≤60 s limit but missed
the create-p95 ≤30 s SLO by 6.162 s. Its latency was cleanly bimodal — 96 fast
creates (4.411–17.664 s) placed on the ready workers, and 64 queued creates
(24.328–38.422 s) waiting on scale-out, separated by a 6.664 s empty gap — so
p95 was determined entirely by the queued group. No more than ~10.2 s of the queued path
was autoscaler decision latency (the MIG poll lags, so that is an upper bound),
~13.0 s worker resume to advertised capacity, and up to 14.1 s per-worker create
drain.

`af3833a` moves scale-out to the gateway (level-triggered, grow-only) and caps
the Nomad autoscaler to scale-in, removing that decision latency. The result is **p95 26.074 s, a 10.088 s
improvement**, max 26.995 s (−11.427 s), wall 48.2 s (−11.5 s), still
160/160 with zero errors and verified cleanup. One resize was issued for the
whole burst (`sandbox_direct_scale_out_total 1`, zero failures), logged as
`direct scale-out requested 5 workers (live=2 occupied=96 queued=64)`.

The distribution is no longer bimodal: the largest gap between adjacent
samples fell from 6.664 s to 1.297 s, and the queued group shrank from 64
samples to 39 — new capacity now arrives fast enough that queued creates blend
into the placed ones. Measured from demand, usable new capacity arrived
24.7 s → 16.5 s sooner, which accounts for most of the p95 gain; that span is
now essentially pure worker resume time and the hard floor for anything
exceeding ready capacity.

Evidence bundle `tests/results/autoscale-20260728-gateway-scaleout/`
(`SHA256SUMS` verified). Full decomposition of both runs in
[Autoscaling and burst-start latency](autoscaling-latency.md#production-path-today--autoscaler-only-2026-07-27-ea0f707).

The targeted traffic group and the isolated three-cycle sawtooth campaign have
not yet been rerun on the hardened release, and the post-burst return to the
fleet floor under the new capped scale-in policy is still being observed. The
successful 2026-07-25 traffic results below are historical evidence only.

## Our numbers in detail

### Create

| Path | Latency | Notes |
|---|--:|---|
| Hot create (default) | 199–271 ms server-side | golden-snapshot clone + GARP readiness; 8 concurrent in 1.1 s |
| Hot create, end-to-end | ~0.55 s | via gateway, client on tailnet |
| Hot create, diff-snapshots enabled | 454 ms | measured 2026-07-06, unregressed vs full-snapshot golden |
| Cold boot | 3.46 s p50 | also the fallback path if the golden snapshot is missing |
| Cold boot with `vcpus`/`mem_mib` override | ~3.7 s | overrides always cold-boot (resources are baked into snapshots) |

### Snapshot source and batch creation

Snapshot-source create p50 **212 ms** (mean 212, p90 219, 25 iters) vs cold boot p50 3463 ms —
**16.3×**. The actual Firecracker resume is ~14 ms (load+resume 12 ms, agent
2 ms — the agent is already running in restored, lazily-faulted memory); the
rest is the rootfs reflink copy. Cross-host snapshot-source create from GCS
(owner host dead):
**~180 ms** once the base image is cached (rootfs cp 8 ms + load 35 ms + agent
139 ms); first pull of a 2.1 GiB base costs a one-time 13.2 s per host.

Snapshot batch scaling (single host, measured 2026-07-01, pre-GARP for N>1 —
single-sandbox latency now matches hot create):

| N | batch (ms) | per-sandbox (ms) | usable |
|--:|--:|--:|--:|
| 1 | 1695 | 1695 | 1/1 |
| 8 | 1797 | 225 | 8/8 |
| 16 | 1994 | 125 | 16/16 |
| 32 | 2675 | 84 | 32/32 |
| 48 | 3435 | 72 | 48/48 |
| 64 | 5719 | 89 | 64/64 |

64 snapshot-source sandboxes in 5.7 s vs 72.8 s of default-source cold boots
(**12.7×**); memory density at N=64 is **925 MB PSS vs 10.1 GB** (10.9×)
because snapshot-source sandboxes mmap the same snapshot memory file. Runnable
suite: [`sdk/typescript/benchmarks/`](../sdk/typescript/benchmarks/README.md).

### Hibernation

Idle sandboxes are paused + full-snapshotted (~2.5 s), the VM killed, tap/IP
returned to the pools. Wake is transparent on any API call or forwarded-port
connection:

| Path | Latency |
|---|--:|
| Wake on API request, same identity (common case) | **49 ms** server-side |
| Wake on TCP connect to a forwarded port | **133 ms** incl. guest dial |

Frozen background processes survive. A 2026-07-24 stress run found an
intermittent regression in the same-identity wake clock re-step: 3/5 targeted
reproductions returned the guest clock approximately 11 s stale after a 10 s
freeze. Hot-create clock sync remained 5/5. Wake latency and state preservation
still passed, but the clock-correctness claim is not currently deterministic.

### Burst & fleet

500-sandbox churn (create → exec → kill, concurrency 96) against 3 workers
× 24 slots: **499/500 succeeded** in 72 s (6.9 creates/s) with client
retry+backoff; 0 pool-exhaustion errors after gateway reserve-at-pick, clean
503s under overload. Sustained overload (500 held alive) ramped the autoscaler
3→8 hosts over ~450 s and cleanly rejected the rest. Full write-up:
<https://claude.ai/code/artifact/0cfd2df8-177f-4793-b415-0f4260b51b8b>.

### 2026-07-23 fleet re-run (commit `06f5c16`)

Full SDK re-run on the current autoscaling fleet — **2 workers × 48 slots** (GCP
nested-KVM, XFS reflink), guests 2 vCPU / 1 GB, Firecracker v1.15. The client ran
**on the control VM, in the same VPC as the workers** (sub-ms network), so these
are SDK **end-to-end** wall-times — "time until you can `exec`" — with the WAN /
tailnet hop stripped out. They complement, not replace, the instrumented
server-side numbers above.

Default-source and snapshot-source create latency (worker-local, 20 iters):

| Path | p50 | mean | p90 | max |
|---|--:|--:|--:|--:|
| Hot create (`Sandbox.create`, golden clone) | 478 ms | 479 ms | 509 ms | 518 ms |
| Snapshot-source create (historical `Sandbox.restore`) | **84 ms** | 95 ms | 88 ms | 305 ms |

Snapshot-source create is **5.7×** faster than a default-source hot create
end-to-end; the hot create itself beats the ~0.55 s tallied from a tailnet
client, since this run has no WAN hop (server-side is still ~0.25 s).

Snapshot batch create (one host, N sandboxes from one snapshot):

| N | batch | per-sandbox | usable |
|--:|--:|--:|--:|
| 1 | 337 ms | 337 ms | 1/1 |
| 8 | 663 ms | 83 ms | 8/8 |
| 16 | 1.28 s | 80 ms | 16/16 |
| 32 | 2.66 s | **83 ms** | 32/32 |

vs 32 default-source creates at 147 ms/sandbox (4.69 s batch) → **1.8×**, all
snapshot-source sandboxes usable.

Fleet & burst (via the gateway, bin-packed across both hosts):

| Test | Result |
|---|---|
| 64 concurrent creates + in-guest workload | 64/64 created, 64/64 workload ok; create p50 **1.28 s** / p95 2.0 s; **0 failures** |
| Churn burst 200 @ concurrency 96 | 200/200 ok, **7.9 creates/s** sustained (25.5 s wall); **0** capacity/pool/agent-timeout errors; create p50 5.2 s under saturation, kill p50 186 ms |
| Single-sandbox SQLite+FS workload | create 0.52 s; full suite 4.10 s mean/run (batch-insert 0.83 s, LIKE scan 1.42 s, 64 MB fs write 84 ms) |

The burst's create p50 rising to 5.2 s at 96-in-flight — with **zero** 503s, pool
exhaustion, or agent timeouts — is the per-host create semaphore working as
designed: a flood queues instead of boot-storming the hosts into timeouts. Raw
JSON: `sdk/typescript/benchmarks/results/06f5c16/`.

### 2026-07-25 autoscaling traffic suite (gateway `52a94cc`, workers `820c3e4`)

Final post-fix validation of physical-host autoscaling under four traffic
shapes. Every successful create was held and repeatedly reconnected to; each
probe also executed an identity check inside the guest, so this tests continued
route and VM usability rather than only HTTP create status.

| Scenario | Creates | Survivability probes | Create p95 / max | Peak hosts | Result |
|---|---:|---:|---:|---:|---|
| Standby-refill boundary | 160 | 24,160 | 19.439 s / 20.360 s | 9 | pass |
| Second wave during scale-out | 208 | 3,328 | 8.433 s / 8.975 s | 8 | pass |
| Long-lived during reconciliation | 168 | 3,048 | 7.984 s / 8.556 s | 9 | pass |
| Create → exec → kill churn | 360 | 720 | 6.593 s / 8.006 s | 9 | pass |
| **Total** | **896** | **31,256** | — | — | **4/4 scenarios, 0 failures** |

The final gateway state was zero sandboxes and zero used slots, with no worker
release mismatches. The gateway ran `52a94cc`; all serving and standby workers
ran release `820c3e4`.

The boundary scenario deliberately kept live sandboxes running while the
autoscaler replenished its standby pool. All five newly created refill workers
reached GCE `SUSPENDED` state. Suspension occurred 176.9–198.1 seconds after
the first MIG observation. Two workers were later resumed and became eligible
at 216.140 and 222.962 seconds, both after the configured 210-second placement
quarantine; the other three never advertised eligible capacity. No live
sandbox disappeared across this transition.

This suite closed three correctness gaps found by progressively stronger
traffic tests:

1. An initial 160-create resume test returned 59 `404`s. On Nomad 1.7 a
   suspended allocation was treated as lost, so its resumed process briefly
   accepted traffic before a replacement allocation reconciled those routes
   away. The legacy disconnect policy now preserves the allocation, and every
   standby worker was refreshed onto the current release.
2. Concurrent deletion could combine routed and free-slot counts from two
   different SQLite snapshots, briefly reporting impossible capacity. Worker
   heartbeats now derive all capacity fields atomically, and the gateway clamps
   malformed or old reports.
3. The broad pre-fix suite passed sawtooth, held-burst, and gradual-ramp traffic,
   then caught a fresh standby-refill worker accepting sandboxes before GCE
   suspended it. Fresh workers now advertise zero placeable slots for the
   initial-delay boundary. The final second-wave scenario passed 208 creates
   and 3,328 continued-usability probes after this fix.

Latency did not improve uniformly, because the fixes intentionally trade a
small amount of placement availability for correctness. The physical
suspended-host resume remains the approximately 8–9 second lower bound; the
standby-boundary p95 is 19.439 seconds versus 17.936 seconds in the earlier
simple held burst. The improvement is that capacity is never exposed during an
unsafe lifecycle transition, and the varied final workload completed without
404s, route loss, or partial-success listings.

Scale-in is deliberately slower than scale-out. With
`SCALE_DOWN_WINDOW=15m`, observed cleanup-to-floor time was approximately
17–20 minutes: the 15-minute max-over-time window is followed by the
one-host-per-minute action cooldown and asynchronous GCE suspend /
reconciliation. A three-cycle sawtooth precursor run completed 480 creates and
5,120 continued-usability probes with zero failures, peaked at 10 hosts, and
returned to the stable 2-running + 6-suspended floor after every cycle.

The remaining improvement areas are:

- keep more running headroom when the SLO cannot absorb the 8–9 second GCE
  resume floor;
- reduce sandbox queue-drain and worker-local create contention, which account
  for the rest of the 19–20 second overflow tail;
- improve autoscaler action confirmation: Nomad Autoscaler can exhaust its
  20–30 second retry budget even though the asynchronous GCE operation later
  succeeds;
- shorten scale-in only if the extra host cost is worth changing the 15-minute
  stability window and per-action cooldown.

Driver:
[`tests/autoscale-traffic.ts`](../tests/autoscale-traffic.ts), wrapped by
[`tests/autoscale-benchmark.sh`](../tests/autoscale-benchmark.sh). Raw results
are intentionally gitignored; the canonical run directory is
`tests/results/autoscale-targeted-proof-820c3e4-20260725T182047Z/`.

### 2026-07-25 suspended standby and event-driven scale-out (release `releasegate-20260725-1`, commit `0b049db`)

Independent post-deploy validation of direct scale-out, immediate warm
heartbeats, higher create concurrency, and worker-release gating. The gateway
started at its **2-worker floor × 48 slots = 96 available slots**, with
suspended standby behind it. The driver issued **160 simultaneous creates** and
held them until the full burst had settled.

| Result | Measurement |
|---|---:|
| Creates | **160/160 succeeded**, 0 capacity, pool, agent-timeout, or other failures |
| Create latency | min 1.463 s · mean 9.069 s · p50 **8.159 s** · p90 17.419 s · p95 17.936 s · p99 18.569 s · max **18.653 s** |
| Create phase | **18.653 s** from first request until the last create returned |
| Full harness | 29.697 s including the in-guest workload and deletion |
| Demand → resize request | **≤1.095 s**; this upper bound includes benchmark-client startup |
| Resumed worker → registered capacity | **8.037–9.161 s** |
| Gateway registration → ready | **27–28 ms** |
| Queued creates | 64 → 16 on the first new host; drained after the second, observed within the trace's **≤0.73 s** effective sampling interval |
| Cleanup | 0 used slots, 0 routes, 0 queued creates, 0 release mismatches |

The effective scale-up timeline was:

| Event | UTC |
|---|---:|
| Demand marker | 11:14:24.741 |
| MIG resize requested | 11:14:25.836 |
| MIG resize completed | 11:14:26.308 |
| Suspended workers began resuming | 11:14:27.208 / 11:14:27.425 |
| Current-release capacity registered | 11:14:35.245 / 11:14:36.586 |
| Last create returned | within **18.653 s** of the first request |

The trace loop was configured for 100 ms, but its SSH-backed samples took about
310 ms per endpoint; the observed queue-drain bound is therefore 0.73 s, not
100 ms. The capacity-registration clock is based on worker lifecycle and
gateway logs and is not limited by that sampling cadence.

Release gating also prevented resumed allocations from the previous worker
release (`v49`) from serving creates while Nomad replaced them with the current
release (`v50`): every create landed on `v50`. A separate controlled stale
heartbeat against the production gateway advertised 48 slots but was exposed
as `release_compatible: false` with `free: 0`; it expired normally after the
20-second heartbeat TTL, returning the gateway to zero routes, queued creates,
used slots, and release mismatches.

Progress across the three comparable held-burst runs:

| Capacity strategy | Create phase | p50 | p90 | p99 |
|---|---:|---:|---:|---:|
| Stopped workers, control-loop scale-out (2026-07-24) | 50.020 s | 8.380 s | 48.320 s | 49.820 s |
| Suspended standby, control-loop scale-out | 31.106 s | 6.919 s | 28.906 s | 30.803 s |
| Suspended standby, event-driven scale-out + release gate | **18.653 s** | 8.159 s | **17.419 s** | **18.569 s** |

The latest create phase is **12.453 s (40.0%) faster** than the clean
suspended-standby run, with p90 down **39.7%**. The p50 is slightly higher
because the existing workers still admit creates in bounded waves; the large
win is removing control-loop delay and promptly making resumed capacity
eligible.

Driver:
[`sdk/typescript/benchmarks/burst-bench.ts`](../sdk/typescript/benchmarks/burst-bench.ts).

### 2026-07-24 stress and autoscaling burst (commit `1cefc65`)

Validation after the fast-scale changes (baked golden adoption, 10 s control
loop, create-rate lead term, and corrected occupancy accounting). The client
ran on the control VM against the gateway, starting with an empty fleet at its
**2-worker floor × 48 slots = 96 immediately available slots**.

Held burst: 160 create requests were issued simultaneously with auto-hibernation
disabled, deliberately leaving 64 requests queued until the fleet added
capacity.

| Result | Measurement |
|---|---:|
| Creates | **160/160 succeeded**, 0 failures |
| Create latency | min 1.31 s · p50 **8.38 s** · p90 48.32 s · p99 49.82 s · max 49.91 s |
| Whole burst | **50.02 s** |
| Queue | peaked at **64**, drained to zero |
| Placement | 48 + 48 on the original workers; 48 + 16 on two resumed workers |
| Usability | **24/24** sampled sandboxes executed `echo ok` |
| Cleanup | **160/160** deleted, 0 kill failures; gateway returned to 0 sandboxes |

Scale-up timeline, relative to the first create:

| Event | Time |
|---|---:|
| `sandbox:workers_desired` first changed 1 → 4 | ~6 s |
| Autoscaler issued MIG resize 2 → 4 | **9.8 s** |
| Two stopped workers began resuming | ~19 s |
| All 160 creates completed | **50.0 s** |

The desired metric briefly reached 5, but the first 2→4 action supplied 192
slots—enough for the 160-request burst—so the cooldown correctly avoided a
second resize. This is the end-to-end proof of the fast-scale path: the old
held-overload run took roughly 450 s to ramp 3→8 hosts; this smaller,
capacity-targeted burst detected demand in one control-loop interval and
finished without a 503, pool-exhaustion error, or agent timeout.

The broader stress suite completed **30/31 checks in 138.5 s**. Concurrency,
churn, load, snapshots, snapshot batch creation, hibernation state
preservation, ports, and hot-create clock correctness passed. The sole failure was
`clock :: hibernate + wake resteps the clock`; it reproduced 3/5 times after
the suite and is tracked as a wake-only timing regression rather than being
folded into the successful burst result.

Driver: [`tests/burst.ts`](../tests/burst.ts).

## Versus hosted sandbox providers

Comparing sandbox latencies across providers is mostly an exercise in noticing
that nobody's clock measures the same thing. Vendor "start" numbers variously
mean server-side VM resume, API-to-ready, or "the create call returned" (which
can precede the guest being usable). The most comparable independent dataset is
the [ComputeSDK sandbox leaderboard](https://www.computesdk.com/benchmarks/sandboxes/)
(run 2026-07-17, 100 iterations/provider), which measures **TTI =
`create()` → first successful command**. Ours is self-hosted, so our closest
equivalent is end-to-end create via the gateway from a tailnet client — no WAN
hop to a provider API, which is a real advantage of self-hosting but also means
the numbers aren't apples-to-apples. Both are shown.

### Create → command running

| Provider | Measured (median TTI) | Vendor claim | Claim caveat |
|---|--:|--:|---|
| **This project (self-hosted)** | **~0.55 s** end-to-end · 0.25 s server-side | — | our fleet, tailnet client |
| Vercel Sandbox | 0.40 s (p95 0.59) | none published | fastest on the July 2026 board |
| e2b | 0.48 s (p95 0.81) | "80 ms" / "<200 ms" | same-region, default template only |
| Daytona | 0.58 s (p95 1.12) | "sub-90 ms" | warm pre-pulled images, best case |
| Modal | 0.62 s (p95 0.77) | <0.5 s median | honest self-definition: client → code running |
| Cloudflare Sandboxes | 4.26 s (p95 5.10) | "milliseconds" | claim is the V8-isolate layer; containers are 1–3 s by their own docs |
| CodeSandbox / Together | 6.37 s (p95 8.60) | 2.7 s p95 cold | leaderboard likely hits the non-hibernated path |
| Fly Machines | — | start ~300 ms · create ~5–20 s | start = existing stopped machine, server-side |
| Firecracker (raw VMM) | — | ~125 ms | kernel→init only; the floor everyone builds on, not a product |

### Resume / wake from pause

| Provider | Number | Status |
|---|--:|---|
| **This project** | **49 ms** same-identity · **133 ms** wake-on-connect | measured on our fleet |
| Morph Cloud | <250 ms | vendor claim, combined "snapshot, branch, restore" figure |
| Fly Machines | "a few hundred ms" | official docs; ≤2 GB RAM recommended, snapshot not guaranteed to persist |
| CodeSandbox / Together | 500 ms p95 | vendor claim; 2024 eng blog says "within a second" |
| Vercel Sandbox | p75 <1 s, p95 <10 s | published percentiles; ~6 s penalty on snapshot cache miss |
| e2b | ~1 s | official docs; pause costs 4 s per GiB of RAM |
| Modal (Sandboxes) | not published | Functions memory-snapshot restore: 0.69–1.05 s |
| Daytona, Cloudflare | not published | |

### Fork / clone a running or snapshotted VM

| Provider | Number | Status |
|---|--:|---|
| **This project** | **84 ms/sandbox** amortized snapshot batch create (32 in 2.68 s); one snapshot-source create ≈250 ms | measured |
| Morph Cloud | <250 ms; "dozens of instances in milliseconds" | vendor claim / press |
| CodeSandbox / Together | ~0.5 s fork overhead (docs); 2 s in the 2022 eng blog | vendor docs + eng blog |
| Modal | supported (N restores from one snapshot) | no latency published |
| e2b, Daytona, Vercel, Fly, Cloudflare | no memory-fork primitive or no number | Fly's `clone` copies config+volume, not memory |

### Honest caveats

- **Network position.** Our end-to-end numbers ride a tailnet to our own
  gateway; hosted providers' numbers include WAN + their API edge. Self-hosting
  earns that advantage, but it is an advantage of *position*, not implementation.
- **Hardware.** Our fleet is nested-KVM GCP VMs — bare metal (Hetzner) measured
  *faster* (cold boot ~2.2 s vs 3.5 s), so these are not best-case numbers.
- **Warm pools.** Hosted providers keep warm capacity; our hot path is a golden
  snapshot per host, built at startup. Both are "warm" strategies; neither
  column is a cold-metal number except the cold-boot rows.
- **Claims vs measurements.** Every number labeled *claim* comes from vendor
  marketing or docs without published methodology. Where an independent
  measurement exists it disagrees with the claim in every case except Modal's.

### Sources

- ComputeSDK sandbox leaderboard (independent TTI, 2026-07-17): <https://www.computesdk.com/benchmarks/sandboxes/>
- e2b: <https://e2b.dev/> · persistence docs <https://e2b.dev/docs/sandbox/persistence>
- Modal: 1M-sandboxes post (2026-07-16) <https://modal.com/blog/scaling-to-1-million-concurrent-sandboxes-in-seconds> · memory snapshots <https://modal.com/blog/mem-snapshots>
- Daytona: <https://www.daytona.io/> · third-party test <https://pixeljets.com/blog/ai-sandboxes-daytona-vs-microsandbox/>
- Morph Cloud: <https://cloud.morph.so/docs/developers>
- Fly Machines: launch post <https://fly.io/blog/fly-machines/> · suspend/resume docs <https://fly.io/docs/reference/suspend-resume/>
- Vercel: snapshot-optimization post (2026-04-02) <https://vercel.com/blog/optimizing-vercel-sandbox-snapshots>
- CodeSandbox / Together: <https://www.together.ai/sandbox> · VM-clone post <https://codesandbox.io/blog/how-we-clone-a-running-vm-in-2-seconds> · memory decompression <https://codesandbox.io/blog/how-we-scale-our-microvm-infrastructure-using-low-latency-memory-decompression>
- Cloudflare: <https://www.cloudflare.com/products/sandboxes/> · independent cold-boot study (2026-07-01) <https://alchemy.run/blog/2026-07-01-microvm-cold-starts/>
- Firecracker: <https://firecracker-microvm.github.io/>

## Reproduce

```bash
cd sdk/typescript
npm run bench:snapshot-source -- --iterations 25
npm run bench:snapshot-batch -- --counts 1,2,4,8,16,32,48 --baseline
npm run bench:burst -- --count 500 --concurrency 96 --retry-ms 250 \
  --output results/burst-500.json

cd ../..
# Requires HOST_URL, GATEWAY_URL, SANDBOX_HOST_KEY, SANDBOX_API_KEY,
# SSH_HOST, and SANDBOX_RELEASE.
SINGLE_HOST_COUNT=48 bash scripts/bench-extensive.sh
```

End-to-end correctness and stress: `cd tests && npm test`. Focused load sizing
uses `STRESS_BURST`, `STRESS_LOAD_N`, `STRESS_FANOUT_N`,
`STRESS_CHURN_CYCLES`, `STRESS_CHURN_ROUNDS`, and `STRESS_CHURN_BATCH`;
`STRESS_FANOUT_N` is the legacy environment-variable name for snapshot batch
create size.
Raw run JSON lands in `sdk/typescript/benchmarks/results/` and `tests/results/`
(gitignored; each folder's README describes the files).

The comparable held burst runs from the Linux control VM with a clean
2×48-slot floor and suspended standby:

```bash
export EXPECTED_WORKER_RELEASE=<deployed-release>
export LIVE_AUTOSCALE_BENCHMARK=I_UNDERSTAND_THIS_CREATES_REAL_VMS
export BURST_COUNT=160
./tests/autoscale-benchmark.sh
```

After the fleet returns to its exact floor, the still-pending hardened-release
campaigns are run separately:

```bash
export TRAFFIC_SCENARIOS="standby-refill-boundary held-burst gradual-ramp second-wave long-lived-reconcile create-exec-kill-churn"
./tests/autoscale-benchmark.sh

# Wait for the exact floor again.
export TRAFFIC_SCENARIOS="sawtooth-scale-cycle"
./tests/autoscale-benchmark.sh
```

### Safe GCP campaign order

Run from the control VM so API traffic, Nomad, GCE observations, and timestamps
share the same VPC and clock. The committed production shape assumes a clean
floor of 2 running workers × 48 slots, at least 2 suspended standbys, standard
`n2-standard-16` workers, and a hard `MIG_MAX=22`. A full autoscaling campaign
can therefore keep multiple large workers and their disks allocated for the
test plus the 15-minute scale-down window. The three-cycle sawtooth alone can
take roughly 70 minutes; the wrapper's three-hour timeout is a failure
backstop, not a target duration.

Use this sequence, never overlapping two campaigns:

1. Run `/v1`, SDK, quick lifecycle, then full correctness/stress suites. Stop
   before autoscaling if any correctness test fails or cleanup does not return
   the gateway to zero sandboxes.
2. Run the 160-create legacy held burst to establish the comparable latency
   baseline. Wait for a stable 2-running floor and replenished suspended pool.
3. Run the targeted traffic group:
   `standby-refill-boundary held-burst gradual-ramp second-wave
   long-lived-reconcile create-exec-kill-churn`.
4. Wait again for the clean physical floor, then run
   `sawtooth-scale-cycle` alone. This isolates its three 15-minute scale-in
   windows and makes abort/cost decisions straightforward.

Before every invocation verify:

- the gateway owns zero sandboxes and reports queue depth zero;
- exactly two compatible, empty, current-release hosts expose 48 free slots;
- the MIG has exactly two `RUNNING` instances and the expected suspended
  standby pool;
- `EXPECTED_WORKER_RELEASE` matches the deployed artifact and no rollout is in
  progress;
- `MIG_MAX`, machine type, slot count, benchmark demand cap, and the GCP budget
  are intentional.

The live wrapper now enforces the acknowledgement for every mode, bounds
runtime and demand, verifies exact worker release, and fails on uncertain
cleanup. Traffic acceptance is zero correctness failures, no duplicate or
disappearing routes, no release/capacity invariant violation, no more than 22
alive hosts, create p95 ≤30 seconds, create max ≤60 seconds, and proven final
cleanup. Treat any of these as a stop condition; do not continue to the next
stage to “get more data.”

Results land under `tests/results/autoscale-<UTC>/`. Preserve the entire
directory. `run.json` must show exit code 0 and `cleanup_ok: true`; verify
`SHA256SUMS` before copying or publishing measurements. After each stage also
confirm zero gateway sandboxes, zero queue depth, zero release mismatches, and
no fresh gateway/worker panic logs. A nonzero result, timeout (124), signal
(130/143), or cleanup-proof failure (70) ends the campaign.
