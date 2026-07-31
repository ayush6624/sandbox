# Production benchmarks

**Updated 2026-07-29 · release `9b6a9fc`.**

This report contains the current production release's lifecycle, burst,
source, batch, fleet, workload, density, and cleanup measurements. Historical
and provider-comparison figures remain in Git history and older raw artifacts,
but they are not mixed into the current tables.

Interactive version: [`benchmark-report.html`](./benchmark-report.html).

## Result

Release `9b6a9fc` is healthy at 32- and 64-sandbox fleet sizes, but the extensive
campaign found two failures that block an unconditional production pass:

- The 128-way default run created and exercised **80/128** sandboxes. The other
  48 creates were sent to worker `10.160.0.7` and failed while configuring the
  jailed VM cgroup: its `io.max` referenced a block device absent on that host.
- The 64-way fsync run created all 64 sandboxes, but one in-guest workload failed
  with SQLite `database is locked`, for **63/64** workload success.

All created resources were deleted and individually verified. A final
gateway-wide check reported zero sandboxes.

## Environment

| Component | Configuration |
|---|---|
| Fleet | Elastic GCP `n2-standard-16` workers; 7 registered during the campaign |
| Warm capacity | 2 workers with 8 ready VMs each at preflight |
| Worker capacity | 48 running slots per worker |
| Guest | 2 vCPU, 1 GiB RAM, Ubuntu 24.04 |
| VMM | Firecracker v1.15.0, nested KVM |
| Storage | XFS reflink |
| Isolation | Jailer, dedicated UID/GID, PID namespace, cgroup leaf, seccomp |
| Gateway and workers | release `9b6a9fc` |
| SDK | TypeScript `sandbox` 1.0.0, API v1 |

A successful create means the returned VM is ready for an in-guest command,
not merely that Firecracker started.

## Lifecycle latency

Twenty-five sequential production cycles exercised create, pause, resume, and
terminate through the gateway. Create includes the full request-to-ready path;
resume includes an in-guest `echo ready` probe.

| Operation | Mean | p50 | p90 | p95 | Min | Max | Samples |
|---|---:|---:|---:|---:|---:|---:|---:|
| Create | 215 ms | **200 ms** | 237 ms | **304 ms** | 197 ms | 404 ms | 25 |
| Pause | 395 ms | 375 ms | 434 ms | 490 ms | 356 ms | 583 ms | 25 |
| Resume + ready probe | 1,113 ms | 1,090 ms | 1,218 ms | 1,222 ms | 1,060 ms | 1,269 ms | 25 |
| Terminate | 1,077 ms | 1,074 ms | 1,216 ms | 1,226 ms | 924 ms | 1,269 ms | 25 |

The sub-500 ms creation objective passed at p95 in this lifecycle run. It is a
ready-capacity result, not a promise for an exhausted pool.

## Ready-capacity burst

A 16-way hold burst was **16/16 ready-pool hits**, with zero misses, capacity
errors, tap-pool errors, agent timeouts, other errors, or pool-build failures.

| Clock and location | Create p50 | Create p95 | Create max | Outcome |
|---|---:|---:|---:|---:|
| Client-side, Greece → India gateway | 426 ms | 600 ms | 600 ms | 16/16 |
| Control VM → gateway, run 1 | 71 ms | 130 ms | 130 ms | 16/16 |
| Control VM → gateway, run 2 | **82 ms** | **118 ms** | **118 ms** | 16/16 |

The committed client-side burst also ran the in-guest workload and termination
phases: workload mean **4,401 ms**, p50 **4,644 ms**, p95 **4,853 ms**;
terminate mean **385 ms**, p50 **396 ms**, p95 **459 ms**. Its complete
create/exec/terminate wall time was **5.926 s (5,926 ms)**, with peak
concurrency 16.

The gap between the client and control-VM rows is transport
RTT/tunneling—not extra VM work. A create that installed a fresh Ed25519 client
public key completed in **92 ms** from the control VM.

### Launch-path timing

These measurements use narrower internal clocks than the lifecycle and burst
tables, so they explain the path but must not be added to the end-to-end
percentiles:

| Measured segment | Current-release observation |
|---|---:|
| Direct ready-row claim at worker API | 8–15 ms for 22/25 samples |
| Hot jailed launch (`prepare` + process-to-API) | 24–47 ms |
| Cold first staging pass | about 123 ms |
| Resumed guest network re-identification | 333–399 ms |
| SSH identity readiness | 125–136 ms |
| Ordinary secure clone after pool exhaustion | about 734 ms end-to-end |

The jailer itself is therefore not a 390 ms tax. The dominant exhausted-pool
floor is guest resume/re-identification plus SSH readiness, while an ordinary
secure clone remains above the sub-500 ms objective. Ready capacity is the
mechanism that makes the latency target reliable without weakening isolation.

## Single-host source latency

Twenty-five default-source creates and 25 snapshot-source creates ran directly
against a warm production worker.

| Source | Mean | p50 | p90 | Min | Max | Outcome |
|---|---:|---:|---:|---:|---:|---|
| Default | 237 ms | **10 ms** | 1,756 ms | 8 ms | 2,132 ms | 25/25 |
| Snapshot | 1,878 ms | 1,897 ms | 2,049 ms | 955 ms | 2,138 ms | 25/25 |

The default distribution is bimodal: **22/25** creates were ready-pool hits in
8–15 ms, while three refill-bound creates took 1.756–2.132 s. Snapshot-source
creation did not use the ready-pool fast path and was slower than default-source
at every reported percentile.

This means the sub-500 ms objective is met for ready-pool hits, not for arbitrary
creates once the pool is exhausted.

## Snapshot batch scaling

Each batch used one immutable snapshot source and waited for every returned VM
to execute `echo benchmark-ready`.

| Count | Operation | Command-ready wall | Per sandbox | Usable |
|---:|---:|---:|---:|---:|
| 1 | 2,536 ms | 2,583 ms | 2,583 ms | 1/1 |
| 2 | 1,015 ms | 1,067 ms | 534 ms | 2/2 |
| 4 | 3,027 ms | 3,078 ms | 770 ms | 4/4 |
| 8 | 4,040 ms | 4,107 ms | 513 ms | 8/8 |
| 16 | 6,051 ms | 6,175 ms | 386 ms | 16/16 |
| 32 | 14,633 ms | 14,898 ms | 466 ms | 32/32 |

The same test's 32-way default-source baseline completed in **6,486 ms**
(203 ms per sandbox), versus 14,898 ms for snapshot source. All batch resources
were usable and cleaned up, but snapshot source is not currently a latency
optimization.

## Fleet workloads

The gateway created sandboxes with concurrency 12, staged the TypeScript
SQLite/filesystem benchmark in every successful VM, ran all workloads
concurrently, then verified each deletion returned `404`.

| Run | Created | Workload success | Fleet wall | Create p50 | Create p95 | Cleanup |
|---|---:|---:|---:|---:|---:|---:|
| 32 default | **32/32** | **32/32** | 40.299 s | 97 ms | 5.020 s | 32/32 |
| 64 default | **64/64** | **64/64** | 67.085 s | 2.221 s | 4.719 s | 64/64 |
| 128 default | **80/128** | 80/80 created | 144.447 s | 80 ms | 5.150 s | 80/80 |
| 64 fsync | **64/64** | **63/64** | 45.403 s | 95 ms | 4.699 s | 64/64 |
| 64 large | **64/64** | **64/64** | 165.596 s | 133 ms | 5.477 s | 64/64 |

The create percentiles mix ready-pool claims with secure refill launches.
Consequently, a low p50 can coexist with a multi-second p95.

### Failure attribution

The 128-way failure was not a gateway-capacity rejection. All 48 failed creates
reported the same host-specific security setup error:

```text
configure VM cgroup io.max: .../io.max: no such device
```

Read-only host inspection found the full chain. The worker's
`/etc/fstab` contained five stale `/mnt/sandbox-data` UUID entries from earlier
data disks. The current `/dev/sdb` was XFS but unmounted, so
`/mnt/sandbox-data` resolved to the boot partition (`/dev/sda1`, `8:1`) instead
of the healthy-worker data disk (`/dev/sdb`, `8:16`). The startup script's
`mount /mnt/sandbox-data` did not establish the mount, `xfs_growfs` then
reported “not a mounted XFS filesystem,” but `|| true` suppressed that error.
The script still stamped `data_disk_ready` and started Nomad, allowing the
worker to advertise 48 slots. This is a startup/admission bug.

The post-campaign fix now replaces every fstab row for the mountpoint, mounts
the named disk explicitly, validates its backing major:minor, XFS type, and
read-write options before and after a mandatory `xfs_growfs`, and keeps Nomad
disabled and stopped until admission succeeds. The fix was deployed in
instance template `sandbox-workers-tpl-20260729-093155`: both active workers
passed direct serial-log validation, all six standby workers registered with
Nomad before suspending, the MIG stabilized at 2 running + 6 suspended, and a
create/delete smoke left zero sandboxes. Release `9b6a9fc` predates the
startup-script fix; another 128-way run is still required before changing the
measured result above.

The fsync failure occurred inside the guest benchmark when concurrent SQLite
readers encountered `database is locked`; VM creation and cleanup both
succeeded.

## Memory density

Density was measured on one pinned production worker with 8 warm VMs plus one
anchor VM. The table reports the delta for 32 additional test VMs, subtracting
that nine-process baseline.

| Source | Added RSS | Added PSS | PSS per VM |
|---|---:|---:|---:|
| Snapshot | 2,460 MiB | **2,460 MiB** | 76.9 MiB |
| Default | 2,913 MiB | **2,912 MiB** | 91.0 MiB |

Snapshot source saved **452 MiB PSS (15.5%)** across 32 VMs. PSS was essentially
equal to RSS in both arms, so this release is not receiving a large shared-page
benefit; `uffd_restore` was disabled in the measured production configuration.
The host returned from 41 Firecracker processes to its nine-process baseline
before the anchor was deleted.

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

The 128-way result demonstrates why the security path must be benchmarked as
part of fleet admission: a host-specific cgroup assumption can turn otherwise
available capacity into deterministic create failures.

## Committed evidence

Current-release lifecycle and burst artifacts:

- [`production_lifecycle_9b6a9fc_20260729.json`](../sdk/typescript/benchmarks/results/production_lifecycle_9b6a9fc_20260729.json)
- [`production_burst_9b6a9fc_20260729.json`](../sdk/typescript/benchmarks/results/production_burst_9b6a9fc_20260729.json)

Latest campaign directory:
[`production_extensive_9b6a9fc_20260729/`](../sdk/typescript/benchmarks/results/production_extensive_9b6a9fc_20260729/)

- [`snapshot-source.json`](../sdk/typescript/benchmarks/results/production_extensive_9b6a9fc_20260729/snapshot-source.json)
- [`snapshot-batch.json`](../sdk/typescript/benchmarks/results/production_extensive_9b6a9fc_20260729/snapshot-batch.json)
- [`fleet_32_default.json`](../sdk/typescript/benchmarks/results/production_extensive_9b6a9fc_20260729/fleet_32_default.json)
- [`fleet_64_default.json`](../sdk/typescript/benchmarks/results/production_extensive_9b6a9fc_20260729/fleet_64_default.json)
- [`fleet_128_default.json`](../sdk/typescript/benchmarks/results/production_extensive_9b6a9fc_20260729/fleet_128_default.json)
- [`fleet_64_fsync.json`](../sdk/typescript/benchmarks/results/production_extensive_9b6a9fc_20260729/fleet_64_fsync.json)
- [`fleet_64_large.json`](../sdk/typescript/benchmarks/results/production_extensive_9b6a9fc_20260729/fleet_64_large.json)
- [`memory-density.json`](../sdk/typescript/benchmarks/results/production_extensive_9b6a9fc_20260729/memory-density.json)

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
export SANDBOX_RELEASE=9b6a9fc
export BENCH_RUN_ID=<unique-run-id>
export SINGLE_HOST_COUNT=32

bash scripts/bench-extensive.sh
```

Run phases sequentially because a worker's tap, IP, port, and slot pools are
shared. Only promote a result into the tracked `production_` namespace after
verifying cleanup, release compatibility, and the absence of credentials.
