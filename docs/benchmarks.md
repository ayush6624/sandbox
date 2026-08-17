# Production benchmarks

**Updated 2026-08-01 · release `c0d0c0f`.**

This report contains the current production release's lifecycle, burst, source,
batch, fleet, and cleanup measurements. Historical and provider-comparison
figures remain in Git history and older raw artifacts, but they are not mixed
into the current tables.

Interactive version: [`benchmark-report.html`](./benchmark-report.html).

## Result

Release `c0d0c0f` passes the full matrix with **no failures in any run**. Both
defects that blocked the previous release's campaign are resolved:

- The 128-way default run created, exercised, and verifiably deleted
  **128/128** sandboxes. On release `9b6a9fc` this run produced 80/128 because
  one worker's jailer `io.max` referenced a block device absent on that host;
  the `startup-worker.sh` data-disk admission fix is now confirmed in
  production.
- The 64-way fsync run passed **64/64** workloads. On `9b6a9fc` one in-guest
  workload failed with SQLite `database is locked` for 63/64.

Every created resource was deleted and individually verified — 352 sandboxes
across the fleet matrix, `cleanup verified` equal to `requested` in all five
runs. The final fleet count returned to its pre-campaign baseline.

## Environment

| Component | Configuration |
|---|---|
| Fleet | Elastic GCP `n2-standard-16` workers; 5 registered during the campaign |
| Warm capacity | 8 ready VMs per active worker |
| Worker capacity | 48 running slots per worker (40 free with the ready pool resident) |
| Guest | 2 vCPU, 1 GiB RAM, Ubuntu 24.04 |
| VMM | Firecracker v1.15.0, nested KVM |
| Storage | XFS reflink |
| Isolation | Jailer, dedicated UID/GID, PID namespace, cgroup leaf, seccomp |
| Gateway and workers | release `c0d0c0f` |
| SDK | TypeScript `sandbox` 2.0.0, API v1 |

A successful create means the returned VM is ready for an in-guest command,
not merely that Firecracker started.

Gateway-facing runs were driven from the in-VPC control VM. Figures measured
across a laptop tunnel are transport-bound and are not used for the headline
tables.

## Lifecycle latency

Twenty-five sequential production cycles exercised create, pause, resume, and
terminate through the gateway. Create includes the full request-to-ready path;
resume includes an in-guest `echo ready` probe.

| Operation | Mean | p50 | p90 | p95 | Min | Max | Samples |
|---|---:|---:|---:|---:|---:|---:|---:|
| Create | 16 ms | **12 ms** | 14 ms | **15 ms** | 10 ms | 107 ms | 25 |
| Pause | 262 ms | 255 ms | 284 ms | 293 ms | 241 ms | 316 ms | 25 |
| Resume + ready probe | 829 ms | 831 ms | 861 ms | 870 ms | 787 ms | 871 ms | 25 |
| Terminate | 889 ms | 876 ms | 938 ms | 945 ms | 812 ms | 1,133 ms | 25 |

Create is an order of magnitude below the 500 ms objective at p95. This is a
ready-capacity result, not a promise for an exhausted pool — see
[Single-host source latency](#single-host-source-latency).

## Ready-capacity burst

A 16-way hold burst was **16/16**, with zero capacity errors, tap/IP/port pool
errors, agent timeouts, and other errors.

| Phase | Mean | p50 | p95 | Max |
|---|---:|---:|---:|---:|
| Create | 82 ms | **79 ms** | **114 ms** | 114 ms |
| In-guest workload | 3,991 ms | 3,988 ms | 4,195 ms | 4,195 ms |
| Terminate | 199 ms | 230 ms | 246 ms | 246 ms |

Complete create/workload/terminate wall time was **4.6 s** at peak concurrency
16.

## Single-host source latency

Twenty-five default-source creates and 25 snapshot-source creates ran directly
against a warm production worker.

| Source | Mean | p50 | p90 | Min | Max | Outcome |
|---|---:|---:|---:|---:|---:|---|
| Default | 132 ms | **9 ms** | 734 ms | 7 ms | 1,381 ms | 25/25 |
| Snapshot | 735 ms | 696 ms | 734 ms | 673 ms | 1,608 ms | 25/25 |

The default distribution is bimodal: **22/25** creates were ready-pool hits in
7–16 ms, while three refill-bound creates took 734 ms, 984 ms, and 1.381 s.
Snapshot-source creation does not use the ready-pool fast path and is slower
than default-source at every reported percentile.

The sub-500 ms objective is therefore met for ready-pool hits, not for
arbitrary creates once the pool is exhausted. Size the ready pool for the
latency-critical arrival burst.

## Snapshot batch scaling

Each batch used one immutable snapshot source and waited for every returned VM
to execute `echo benchmark-ready`.

| Count | Operation | Command-ready wall | Per sandbox | Usable |
|---:|---:|---:|---:|---:|
| 1 | 1,015 ms | 1,057 ms | 1,057 ms | 1/1 |
| 2 | 1,519 ms | 1,565 ms | 783 ms | 2/2 |
| 4 | 3,034 ms | 3,088 ms | 772 ms | 4/4 |
| 8 | 6,048 ms | 6,122 ms | 765 ms | 8/8 |
| 16 | 12,094 ms | 12,219 ms | 764 ms | 16/16 |
| 32 | 24,191 ms | 24,456 ms | 764 ms | 32/32 |

Per-sandbox cost is flat at roughly **764 ms** from N=4 upward, so batch
snapshot creation scales linearly rather than degrading. The same test's 32-way
default-source baseline completed in **6,464 ms** (202 ms per sandbox), so
snapshot source remains a state-fidelity feature, not a latency optimization.

## Fleet workloads

The gateway created sandboxes with concurrency 12, staged the TypeScript
SQLite/filesystem benchmark in every successful VM, ran all workloads
concurrently, then verified each deletion.

| Run | Created | Workload success | Fleet wall | Create p50 | Create p95 | Cleanup verified |
|---|---:|---:|---:|---:|---:|---:|
| 32 default | **32/32** | **32/32** | 15.171 s | 51 ms | 94 ms | 32/32 |
| 64 default | **64/64** | **64/64** | 42.980 s | 51 ms | 4.473 s | 64/64 |
| 128 default | **128/128** | **128/128** | 66.182 s | 45 ms | 3.571 s | 128/128 |
| 64 fsync | **64/64** | **64/64** | 28.337 s | 52 ms | 4.586 s | 64/64 |
| 64 large | **64/64** | **64/64** | 92.405 s | 51 ms | 4.249 s | 64/64 |

The create percentiles mix ready-pool claims with secure refill launches, which
is why a 45–52 ms p50 coexists with a multi-second p95 once a run exceeds
fleet-wide ready capacity. The 32-way run fits inside ready capacity and shows
no such tail.

### What changed since the previous release

Both `9b6a9fc` failures are closed, and one further defect was found and fixed
during this campaign:

- **128-way (was 80/128).** A worker's `/etc/fstab` had accumulated stale
  `/mnt/sandbox-data` UUID entries, so the data disk was unmounted and the
  mountpoint silently resolved to the boot partition; `xfs_growfs` failed and
  `|| true` suppressed it, yet the script still stamped `data_disk_ready` and
  started Nomad. `startup-worker.sh` now replaces every fstab row for the
  mountpoint, mounts the named disk explicitly, validates its backing
  major:minor, XFS type, and read-write options around a non-optional
  `xfs_growfs`, and keeps Nomad stopped until admission succeeds.
- **Bulk delete verification.** An interim run of this campaign measured
  **10/64** delete-then-verify reads failing with `503 not resolvable yet
  (adopt in flight or deferred); retry` — for sandboxes the gateway had just
  deleted itself. Requiring a proven absence before answering 404 is correct,
  but it left an adopt probe as the only proof, and that probe is rate-limited,
  so a bulk teardown degraded to retryable errors. A completed destroy now
  records the absence directly. The tables above are from the fixed release.

## Memory density

**Not re-measured for this release.** `scripts/mem-density.sh` requires the
target worker to have zero sandboxes *and* zero Firecracker processes so it can
subtract a clean baseline. Every production worker now keeps a resident pool of
8 ready VMs, so that precondition cannot be met without draining or
reconfiguring a live worker, which this campaign deliberately did not do.

The last measured figures, on release `9b6a9fc`, were 76.9 MiB PSS per VM for
snapshot source versus 91.0 MiB for default source — a **452 MiB (15.5%)**
saving across 32 VMs, with PSS essentially equal to RSS in both arms
(`uffd_restore` disabled). Treat those as indicative for the current release
rather than as current evidence.

To re-measure, point `SSH_HOST`/`API` at a worker started with
`warm_pool_size: 0`, or teach the script to record and subtract a non-zero
baseline.

## Security properties

No benchmark bypassed production security. Every created VM used:

- jailer exec and chroot
- dedicated UID/GID and PID namespace
- per-VM cgroup leaf and resource limits
- seccomp policy
- independent tap, IP, and rootfs
- guest network re-identification and clock synchronization
- in-guest agent readiness checks
- freshly rotated Ed25519 SSH host keys

The previous release's 128-way result is why the security path must be
benchmarked as part of fleet admission: a host-specific cgroup assumption
turned otherwise available capacity into deterministic create failures, and
only a full-size run surfaced it.

## Correctness gates run alongside this campaign

These are pass/fail suites rather than latency measurements, run against the
same release:

| Gate | Result |
|---|---|
| `tests/` fleet e2e (10 suites) | 64/64 |
| Stress suites (concurrency, churn, load) on repeat | 12/12 |
| 64-way and 128-way create bursts | 64/64 and 128/128 |
| Churn burst-bench, 96 short-lived sandboxes | 96/96, zero errors in every class |
| PTY/WebSocket stress, 48 shells across churn rounds | 48/48 |
| `ssh_pubkey` create plus real SSH login | 12/12 keys, unique host keys, root refused |
| v1 contract and SDK v1 fleet probes | passed |

## Per-sandbox utilization

**Release `9abf627`, 2026-08-17**, measured from the control VM with
`tests/metrics-bench.ts` (27/27 checks, artifact
`tests/results/metrics-bench-final.json`). Every figure is CAUSED by a known
workload in a real guest and compared against what that workload implies, not
against a constant.

| Property | Measured |
|---|---|
| First sample available after create | **2.03 s** (create itself 77 ms) |
| One busy core, 2 allocated vCPUs | **50.6%** (expected ~50%) |
| Both cores busy | **100.0%** |
| Idle sandbox | **0.8%** |
| 256 MiB guest write → rootfs growth | **+256.1 MiB** |
| 256 MiB guest write → guest `disk_used` | **+256 MiB** |
| 64 MiB guest download → guest `net_rx` | **+64.3 MiB** (tx +0.07 MiB) |
| 384 MiB touched then freed → `host_mem_bytes` | **+816 MiB**, released **0.9 MiB** |
| Same moment, guest `mem_used` | **194 / 985 MiB** |
| Metrics read latency, 8 sandboxes × 10 rounds | p50 **8 ms** / p95 **13 ms** |
| `limit=1` current-reading call | **4 ms** |
| Idle hibernation with sampling on (60 s window) | froze at **60.07 s** |
| `vmm_generation` across freeze/wake | 1 → 2, counters restart (0.38 s < 8.89 s) |

Four properties this campaign pinned down, each of which had been asserted
incorrectly somewhere before it was measured:

- **`host_mem_bytes` is a high-water mark, not live usage.** A guest that
  touched 384 MiB and freed it released 0.9 MiB of the host's charge. Without a
  balloon device the pages never come back, which is why the guest-reported
  `mem_used_bytes` exists and why the two must never be conflated.
- **`rootfs_alloc_bytes` counts extents still shared with the golden base**, so
  it reads ~2.1–2.2 GiB before a sandbox writes anything. Only its growth is
  that sandbox's own consumption; summing the level across a host counts the
  shared base once per sandbox.
- **Sampling does not suppress idle hibernation.** A sandbox with a 60 s idle
  window and no client traffic beyond the metrics reads themselves froze at
  60.07 s, then produced no further samples and was not woken by repeated reads.
  This is the regression the whole design is arranged to avoid.
- **Guest polling must run on every tick.** Polling every second tick left the
  guest fields on alternating samples, putting holes in any chart and making
  `limit=1` a coin flip. The poll is three `/proc` reads and a `statfs`;
  completeness is worth far more than the cost avoided.

Fleet health during the campaign: `sandbox_guest_stat_failures_total` was **0**
on both workers, and all 27 checks ran with the fleet returning to zero
sandboxes afterwards.

## Committed evidence

Current-release lifecycle and burst artifacts:

- [`production_lifecycle_c0d0c0f_20260801.json`](../sdk/typescript/benchmarks/results/production_lifecycle_c0d0c0f_20260801.json)
- [`production_burst_c0d0c0f_20260801.json`](../sdk/typescript/benchmarks/results/production_burst_c0d0c0f_20260801.json)

Latest campaign directory:
[`production_extensive_c0d0c0f_20260801/`](../sdk/typescript/benchmarks/results/production_extensive_c0d0c0f_20260801/)

- [`snapshot-source.json`](../sdk/typescript/benchmarks/results/production_extensive_c0d0c0f_20260801/snapshot-source.json)
- [`snapshot-batch.json`](../sdk/typescript/benchmarks/results/production_extensive_c0d0c0f_20260801/snapshot-batch.json)
- [`fleet_32_default.json`](../sdk/typescript/benchmarks/results/production_extensive_c0d0c0f_20260801/fleet_32_default.json)
- [`fleet_64_default.json`](../sdk/typescript/benchmarks/results/production_extensive_c0d0c0f_20260801/fleet_64_default.json)
- [`fleet_128_default.json`](../sdk/typescript/benchmarks/results/production_extensive_c0d0c0f_20260801/fleet_128_default.json)
- [`fleet_64_fsync.json`](../sdk/typescript/benchmarks/results/production_extensive_c0d0c0f_20260801/fleet_64_fsync.json)
- [`fleet_64_large.json`](../sdk/typescript/benchmarks/results/production_extensive_c0d0c0f_20260801/fleet_64_large.json)

Artifacts include release and run IDs, workload parameters, sample data,
failure diagnostics, host observations, and cleanup proofs. They contain no
credentials.

## Reproduce

```bash
export HOST_URL=http://<direct-worker>:8080
export GATEWAY_URL=http://<gateway>:9090
export SANDBOX_HOST_KEY=<worker-client-token>
export SANDBOX_API_KEY=<gateway-client-token>
export SSH_HOST=<direct-worker>
export SANDBOX_RELEASE=c0d0c0f
export BENCH_RUN_ID=<unique-run-id>
export SINGLE_HOST_COUNT=32

bash scripts/bench-extensive.sh
```

Run phases sequentially because a worker's tap, IP, port, and slot pools are
shared. Drive gateway-facing phases from inside the VPC — a laptop tunnel adds
hundreds of milliseconds of transport RTT that is easily mistaken for VM
creation cost. Only promote a result into the tracked `production_` namespace
after verifying cleanup, release compatibility, and the absence of credentials.
