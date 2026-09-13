# Development fleet canary

Run date: September 5, 2026 UTC, continuing into September 6 in India.
The existing US fleet is now used for development. There is no production
serving requirement or SLA/SLO for this campaign. This report follows the
[ASCII review](ascii-competitive-review-2026-09-05.md).

The gateway and both workers were deployed through `rollout-remote.sh --fast
--yes` to `fb09662-dev-20260905182000`. Before the rollout, the gateway ran
`fb09662` and workers ran `8284f21-dev-20260828112753`. No runtime optimization
was introduced in this campaign; these measurements establish a current
baseline with corrected benchmark timing and provenance.

Both workers are `n2-standard-16` instances in `us-central1-a`, with a resident
eight-VM warm pool and 48 configured slots each. Requests originate on the US
control VM, using SDK 2.9.1 and a user-local Node 22.18.0 installation. The
system Node 18 installation remains available. Both actual base rootfs files
have SHA-256 `f128719bdcbdcb61533b9a127c4e83b73d8225e17c7b248d96c90975bdcc2fa3`.
Reports observe running gateway and worker releases before and after each run.
The active worker configuration uses 2 vCPUs, 1,024 MiB guest RAM, create
concurrency 16, and `uffd_restore=false`. This campaign does not exercise the
lazy UFFD restore path.

The full deployed smoke passed create, exec, WebSocket PTY connection, shell
output, terminal exit, destroy, and a typed 4404 response for a deleted sandbox.

## Lifecycle

All 25 sequential gateway iterations passed, including cleanup. These are
warm-fleet samples, not sustained-load or tail-latency guarantees.

| Measurement | p50 | p95 |
| --- | ---: | ---: |
| Create API return | 16 ms | 19 ms |
| Create through successful command | 43 ms | 48 ms |
| Pause | 279 ms | 299 ms |
| Resume through successful command | 668 ms | 741 ms |
| Terminate | 916 ms | 960 ms |

[Raw lifecycle report](../sdk/typescript/benchmarks/results/development_lifecycle_20260905.json).

## Repeated snapshot fan-out through the gateway

The workload holds 256 MiB of continuously dirtied anonymous memory, a
384 MiB incompressible file, 5,000 small files, and approximately 32 MiB of
SQLite data. Each clone must preserve the live process, complete another
memory sweep, verify the large-file hash and small-file count, and pass SQLite
integrity checking. Small-file contents are not individually verified.

All 18 clones passed. Counts run in order 1, 8, 8, 1 against one snapshot;
later rows can benefit from snapshot and filesystem cache reuse. These rows
do not prove cold cross-host placement.

| Round / count | All commands ready | All workload checks complete |
| --- | ---: | ---: |
| 1 / 1 | 4.143 s | 24.211 s |
| 1 / 8 | 1.250 s | 32.022 s |
| 2 / 8 | 1.360 s | 31.051 s |
| 2 / 1 | 0.483 s | 19.380 s |

Verification time includes the workload's own hashing and SQLite work as well
as restore stalls. It is not an isolated measurement of page hydration or
storage throughput. The source snapshot took 3.008 s to capture, outside the
table's batch timers.

[Raw working-set report](../sdk/typescript/benchmarks/results/development_working_set_20260905.json).

## Cold-target GCS and peer transport

All 36 clones passed across two rounds. Every case uses a fresh snapshot ID
on the same source and destination workers; the immutable base is already
available. The order is GCS then peer in round one, reversed in round two.
The source guest is terminated after capture, and the runner waits for
durability before starting the batch timer. Capture took 1.934–3.847 s and
the subsequent durability wait took 12.912–18.242 s. Neither is included below.

| Count / path | Batch creation, two-run range | Complete workload checks, two-run range |
| --- | ---: | ---: |
| 1 / GCS | 11.879–12.043 s | 31.783–32.651 s |
| 1 / peer | 10.921–11.000 s | 30.876–31.455 s |
| 8 / GCS | 13.110–13.442 s | 44.093–45.355 s |
| 8 / peer | 11.214–11.624 s | 42.225–42.910 s |

Each peer case observed one target population, three source artifact serves,
no peer failure, and no GCS fallback. The 1,318–1,344 MiB byte counter measures
uncompressed allocated snapshot payload, not wire traffic. These are direct
worker component measurements using a private peer hint, not public gateway
latency. Two samples per condition establish no tail distribution or provider
ranking.

[Raw transport report](../sdk/typescript/benchmarks/results/development_transport_20260905.json).

## Pending-upload setup failure

The first attempt reserved 39 default 1-GiB guests on the source. Together
with the source workload and warm pool, this left no unreserved memory for
capture. The worker rejected the snapshot, reporting a need for another
1,087 MiB beyond the source VM's 1,180-MiB limit. No batch was submitted.
The source and all fillers were removed from the API without cleanup errors.

The retry uses 1-vCPU, 256-MiB idle guests as placement reservations. That
separates slot exhaustion from capture-memory exhaustion.

A later cgroup inspection found nine empty leaves on the source, each still
reserving 1,180 MiB, for 10,620 MiB total. The task limit is 59,868 MiB.
`parentReservableBytes` sums finite child limits, including empty leaves, and
`cleanupJail` ignores errors when removing a leaf. These are concrete places
to investigate the reservation leak. The exact failed removal and its errno
were not captured, so the original rejection cannot yet be attributed solely
to the configured headroom. No cgroup-accounting fix was deployed.

[Raw rejected-capture report](../sdk/typescript/benchmarks/results/development_gateway_pending_20260905.json).

## Public restore while upload is pending

The retry passed all nine clones. The source holds 39 idle 256-MiB guests;
setup and teardown of those reservations are outside the measured interval.
Snapshot capture passes through the gateway, which records ownership before
returning. The runner then observes `state=local` and submits a public batch
without a private routing hint. The source stays alive through verification.

| Count | All commands ready after submission | All checks complete after submission | Capture start through all checks |
| --- | ---: | ---: | ---: |
| 1 | 13.144 s | 33.072 s | 35.102 s |
| 8 | 13.821 s | 45.311 s | 48.320 s |

For each snapshot, every child was verified on the intended destination,
exactly one peer population occurred, and GCS fallback and peer failures were
zero. This proves the public pending-upload path works in these conditions.
Placement and zero failed pulls were enforced by the runner; this raw report
does not store those checks as separate fields. The runner now persists them
for future runs. The original measurements have not been rewritten.
It does not measure a matched speedup against GCS: source occupancy and
source lifetime differ from the direct transport experiment. Upload may
finish after the pre-submission state check. One case per count is a canary,
not a latency distribution.

[Raw pending-upload report](../sdk/typescript/benchmarks/results/development_gateway_pending_small_20260905.json).

## Verification-cost diagnostic

A separate one-clone run adds guest-side stage timers without changing the
verification work. Command readiness took 4.225 s and all checks finished at
23.328 s after batch submission. Within the guest verifier:

| Stage | Time |
| --- | ---: |
| Wait for another complete memory sweep | 4.116 s |
| Hash the 384-MiB file | 4.399 s |
| Count small files | 0.011 s |
| SQLite integrity and row-count checks | 9.337 s |
| Total in the verifier | 17.864 s |

Command startup and transport account for additional time outside the guest
timer. A lazy-memory change cannot be judged by the combined verification
number alone; the SQLite and disk phases need their own I/O and CPU evidence.

[Raw stage-timing report](../sdk/typescript/benchmarks/results/development_verification_profile_20260905.json).

## Final state and next work

Follow-up: the [cgroup cleanup fix](cgroup-cleanup-2026-09-06.md) is now deployed.
Repeated termination checks passed on both workers, followed by a successful
snapshot capture at all 48 default-size reservations. The state below records
the end of the original canary before that fix.

Final API inventory shows zero campaign sandboxes and no campaign snapshot
IDs on either worker. Both workers are alive on the candidate release, each
with eight warm guests and 40 advertised free slots. A hibernated sandbox
created on September 3 and a snapshot created on August 26 remain. Both
predate this campaign. The empty cgroup leaves
described above are an outstanding host-resource cleanup issue, so API
deletion alone is insufficient proof that all host reservations were released.

The full SDK suite passed 73 tests; the eight runner fixtures also passed
after the smaller-filler change. Typecheck and build passed. Stage timing was
verified against the live guest. The rejected-capture report correctly fails
the provenance promotion command because the experiment failed; the other
five reports pass.

The next implementation should reproduce and fix stale cgroup cleanup and
verify that repeated snapshot/terminate cycles return memory reservations to
baseline. Then test capture at full occupancy. Durable public operations and
upload retry/reconciliation remain the next architectural work from the
ASCII review. Lazy remote memory and network namespace work need separate
measurements before choosing a performance change.

## Reproduce

Run from the control VM checkout with Node 22 or later on `PATH`, the SDK's
locked dependencies installed, and the selected fleet's existing secrets file.

```bash
export SANDBOX_GCP_CONFIG=infra/gcp/config.us.env
export SANDBOX_RELEASE=<observed-running-release>
export BENCH_GUEST_IMAGE_SHA256=<checksum-of-actual-worker-rootfs>
export BENCH_CACHE_STATE=<actual-cache-conditions>
bash scripts/bench-dev-canary.sh lifecycle \
  benchmarks/results/development_lifecycle_<date>.json --iterations 25
bash scripts/bench-dev-canary.sh snapshot-working-set \
  benchmarks/results/development_working_set_<date>.json --counts 1,8 --rounds 2
bash scripts/bench-dev-canary.sh snapshot-peer-transfer \
  benchmarks/results/development_gateway_pending_<date>.json \
  --source-url http://<source-worker>:8080 --target-url http://<target-worker>:8080 \
  --gateway-url http://<gateway>:9090 --modes gateway-pending \
  --counts 1,8 --rounds 1 --memory-mib 256 --source-fillers 39
```

The wrapper loads credentials without printing them, requires a GCE host,
rejects Node versions below 22, and validates the resulting report. It leaves
exploratory failures available for diagnosis. Use a fresh run ID for each
invocation; the default generates a UUID.
