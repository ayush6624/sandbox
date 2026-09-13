# Create and fork measurements, September 13, 2026

The current development build passed separate warm-claim and snapshot-pool-miss
measurements, followed by concurrent snapshot forks. All commands succeeded.
Cleanup verified that all 39 created sandboxes and the disposable snapshot were
absent. Both workers finished with eight ready VMs and no running or starting VMs.

## Results

Times are milliseconds observed from the user's workstation through an SSH
tunnel to the development gateway. Each sequential row contains five samples.

| Source and path | API return, median | Successful command, minimum | Successful command, median | Successful command, maximum |
| --- | ---: | ---: | ---: | ---: |
| Default source, ready-pool claim | 86 | 163 | 169 | 197 |
| Disposable snapshot, ready-pool miss | 309 | 373 | 402 | 1,057 |

The first snapshot miss took 1,057 ms through the command probe. The remaining
four took 373–421 ms. The run did not reset caches, so it does not establish a
cold-cache latency or explain the first sample's extra delay.

| Concurrent snapshot forks | Operation accepted | Operation completed | First command completed | All commands completed |
| --- | ---: | ---: | ---: | ---: |
| 4 | 69 | 561 | 655 | 717 |
| 8 | 62 | 713 | 816 | 874 |
| 16 | 65 | 1,527 | 1,647 | 1,774 |

The runner starts each command probe when that child's result becomes visible.
Batch times include polling every 100 ms plus network time. They are elapsed
times from batch submission, not per-VM service times.

## Evidence and limits

The [raw report](../sdk/typescript/benchmarks/results/development_create_paths_20260913.json)
contains individual timings, operation results, metrics around each request or
batch, cleanup IDs, and observed build identities before and after the run.
The gateway and both workers ran `fb09662-dev-20260906174946` throughout.
Post-run `sha256sum` on both workers confirmed the guest filesystem at
`/mnt/sandbox-data/base/devbox-rootfs.ext4` has SHA-256
`f128719bdcbdcb61533b9a127c4e83b73d8225e17c7b248d96c90975bdcc2fa3`.
The report includes that observation separately and uses it for the guest-image
metadata field. No guest image changed during the run.

Every default-source sample increased the aggregate warm-claim counter by one
and left the miss counter unchanged. Snapshot phases increased misses and had
no warm claims. The snapshot had no ready-pool target. These are different
immutable sources; this is not a controlled comparison of warm and empty pools
for the same template. Batch miss counters are phase evidence and do not assign
a path to individual children. The observed counter changes were all on one
worker, so these measurements do not establish cross-worker throughput.

The command is `printf benchmark-ready`. It proves the guest can execute a
command, while the [prepared-workspace report](prepared-workspace-2026-09-13.md)
measures a repository test workflow. Five sequential samples and one batch at
each size do not establish tail latency, sustained capacity, or a launch SLO.
No cache, pool, autoscaling, or deployment settings changed for this run.

## Reproduce

Run from `sdk/typescript` with a reachable gateway URL and credential in
`SANDBOX_API_URL` and `SANDBOX_API_KEY`. Set `BENCH_RELEASE`,
`BENCH_GUEST_IMAGE_SHA256`, `BENCH_RUNNER_REGION`, and `BENCH_CACHE_STATE` from
observed deployment conditions. Use an otherwise quiet development fleet so
unrelated creates do not contaminate aggregate counter deltas.

```sh
BENCH_OUTPUT=benchmarks/results/create-paths.json npm run bench:create-paths
npm run bench:validate -- benchmarks/results/create-paths.json
```

The runner creates five default-source samples, one snapshot source, five
snapshot-source samples, and batches of 4, 8, and 16 children. It assigns a run
ID and ten-minute TTL to sandboxes. It waits for snapshot durability before
forking, discovers run-owned resources during cleanup, and checks GET returns
404 for every observed sandbox and the snapshot. An ambiguous path, failed
command, incomplete batch, or cleanup failure makes the report fail.
