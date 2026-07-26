# GCP production-readiness validation — 2026-07-26

Status: **passed for remediation release `prod-fixes-20260726-1`**

## Remediation validation

The three blockers found in the earlier `p1-api-20260726-2` campaign were
fixed in focused commits and deployed together as `prod-fixes-20260726-1`
(`75ecb6b`):

- `4434dc9` serializes Firecracker loads that share a baked snapshot rootfs
  path, covering hot create, restore, and fanout;
- `5754d65` bounds graceful shutdown and forces a VMM that does not exit
  promptly;
- `75ecb6b` makes Nomad Autoscaler the sole production MIG resize writer.

The post-deploy gates all passed:

| Gate | Result |
|---|---|
| HTTP v1 contract probe | Pass |
| TypeScript SDK v1 fleet probe | Pass |
| Full API/SDK compatibility suite | **64/64** in 225.8 s |
| Lifecycle after pause/resume, 25 iterations | terminate p50/p95/max **876/952/978 ms**; no 30 s tail |
| Snapshot-source batches N=1,2,4,8,16,32 | **63/63 usable**, every operation `succeeded` |
| 500-job churn at concurrency 96 | **500/500**, 0 capacity/pool/agent/other errors; 50.0 s; 10.0 jobs/s |
| Scaling ownership | gateway direct-scale counter stayed **0**; no direct-scale log entries |
| Worker security gate | Pass on both baseline workers |
| Cleanup | zero sandboxes and only the server-managed golden snapshot |

During churn, Prometheus desired capacity peaked at 10 while the sole Nomad
writer moved the MIG target from 3 to 4. Once demand drained, desired returned
to the floor; target 4 is retained temporarily by the configured 15-minute
scale-down window. The prior dual-writer ratchet to 11 workers did not recur.
Nomad's short-circuited GCE stability confirmation still reports its documented
retry-limit message while the standby pool replenishes; the resize itself
lands, readiness is heartbeat-driven, and no second writer retries or inflates
the target.

Local validation before deployment also passed full Go tests, race tests for
server/gateway/v1, 43 SDK tests, TypeScript checks, OpenAPI regeneration,
Linux cross-build, shell syntax checks, and the single-writer infrastructure
validator.

## Earlier failed campaign: `p1-api-20260726-2`

The earlier campaign validated worker release `p1-api-20260726-2` in
`ratio-experiments/asia-south1-a` through the fleet gateway. The campaign
started and ended at the normal physical floor:

- 2 compatible running workers;
- 48 free slots per worker (96 total);
- 6 suspended standbys;
- zero sandboxes, snapshots, queued creates, or release mismatches.

The full correctness suite passed before the benchmark campaign (**64/64**,
269.4 seconds). The updated TypeScript SDK and benchmark unit suite passed on
the GCP control node (**43/43**). The campaign stopped before the dedicated
held-burst and autoscaling scenario groups because a resource correctness gate
failed.

## Result summary

| Workload | Result | Important measurements |
|---|---|---|
| Full API/SDK correctness | Pass | 64/64 |
| SDK and benchmark unit tests | Pass | 43/43 |
| Lifecycle, 25 iterations | Correctness pass, latency fail | create p50/p95 346/392 ms; pause 109/132 ms; resume-to-exec 468/528 ms; terminate 950/31,102 ms |
| Snapshot-source create, 25+25 | Pass | default p50 334 ms; snapshot p50 144 ms; 2.32x speedup |
| Churn, 500 jobs at concurrency 96 | Pass | 500/500; 0 capacity/pool/agent/other errors; 58.7 s; 8.5 jobs/s |
| Snapshot batch create | **Fail** | N=1,2,4 passed; N=8 returned `partially_succeeded`, 4/8 failed |
| Dedicated held burst / traffic scenarios / sawtooth | Not run | stopped at the failed correctness gate |

## Production blockers

### P0 — concurrent snapshot-source batch creation is unreliable

Run `gcp-20260726T063507Z-snapshot-batch-v2` used the hardened v1 batch
benchmark with bounded readiness polling and deterministic cleanup.

| N | Max parallelism | Operation status | Usable |
|---:|---:|---|---:|
| 1 | 1 | `succeeded` | 1/1 |
| 2 | 2 | `succeeded` | 2/2 |
| 4 | 4 | `succeeded` | 4/4 |
| 8 | 8 | `partially_succeeded` | **4/8** |

Failed indexes 2, 3, 6, and 7 contained no sandbox and returned HTTP problem
code `batch_item_failed`, status 500, detail `all clones failed to start`.
This was not transient command readiness: the asynchronous server operation
itself reported the four failures. All successful partial results and the
source snapshot were subsequently deleted and verified absent.

Likely cause, based on code inspection:

1. `internal/apiv1.Handler.runBatch` runs snapshot items concurrently.
2. Each item calls the legacy `/snapshots/{id}/fanout` endpoint with
   `count=1`.
3. Every independent fanout request stages the snapshot's baked rootfs at the
   same source path, loads its clone, and then unlinks that shared path.
4. Concurrent requests can therefore remove or replace the shared staged path
   while peers are loading it.

The legacy multi-clone fanout path avoids this particular race because one
handler stages the path once around all phase-one clone loads. The fix should
coordinate baked-rootfs staging across requests (or give v1 batch creation one
server-side multi-clone operation) and add a concurrent snapshot batch
integration test at N=8 and N=32.

### P0 — terminate has a repeatable 30-second tail after pause/resume

The first lifecycle run failed at the SDK's 30-second default request timeout.
After increasing only the benchmark control timeout, 25/25 iterations
completed, but 3/25 terminate calls took approximately 31 seconds:

- 31,138 ms;
- 30,999 ms;
- 31,102 ms.

Gateway request logs confirm the DELETE handlers themselves took that long and
returned 204; this is not client-side measurement noise. The duration matches
the 30-second graceful guest shutdown timeout in `Server.destroy`. The normal
terminate range was 843–1,022 ms.

The destroy path should force-stop after a short, explicit grace period and
bound the subsequent VMM wait. Add a regression test for
create → pause → resume → exec → terminate and an acceptance threshold well
below the SDK's default request timeout.

### P1 — two independent MIG scaling writers overshoot after churn

The clean two-worker fleet processed all 500 churn jobs successfully, but
scale-out continued after the gateway returned to zero sandboxes. Observed
states included:

- gateway direct-scale requests for 4, 5, then 6 workers;
- Nomad Autoscaler acting on the same MIG while it was unstable;
- an autoscaler retry-limit error while waiting for MIG stability;
- 11 running plus 5 suspending workers at one observation;
- a stable target of 11 running workers despite a continuously calculated
  desired floor of 2.

After the configured cooldown, GCP converged autonomously 11 → 7 → 2 and was
stable at 16:43:53 UTC. No manual resize was used. Correctness and cleanup
passed, but the overshoot and paid-capacity tail show that gateway direct
scaling and Nomad Autoscaler need a single-writer protocol or explicit
coordination before production.

## Benchmark-suite changes made during validation

- `d49f2d0` — make lifecycle control timeout explicit and record it.
- `7762b9f` — clamp batch parallelism to the API limit, poll readiness,
  preserve indexed operation errors, verify partial cleanup, and fail closed.
- `f5927ab` — validate fleet workload arguments, persist partial evidence,
  retry teardown, verify deletion, and fail closed.

These are in addition to the v1 terminology/API migration and live-campaign
guard commits immediately preceding them.

## Evidence

Raw results remain on the GCP control node under:

`/home/ayush/web-sandbox/tests/results/gcp-20260726T063507Z/`

| File | SHA-256 |
|---|---|
| `lifecycle-rerun.json` | `46dc7d092f8f3aac5eb2500916acbd999ab5585c965d1cb26b73fe308676b42b` |
| `snapshot-source.json` | `8a3bf9187a753f397a8a34607a65639fdf80b0cef5bbf942485b440bf75eb28a` |
| `churn-500.json` | `cb4a74456856e04cb755b7673f3908834485f2d1fbc2c1fe20ff45b39c76198b` |
| `snapshot-batch-v2.json` | `cad2e738dee99416a0eeac8164ed3949a6d6cd12c5bda6e434f222462de95488` |

The initial failed batch run was intentionally superseded by
`snapshot-batch-v2.json`, which records indexed failures and cleanup evidence.
Worker journal collection was not available from the control service account,
so the next diagnostic run should collect worker logs through an authorized
path while reproducing N=8.

## Required sequence before resuming the campaign

1. Fix and integration-test concurrent snapshot-source batch creation.
2. Fix and latency-test termination after pause/resume.
3. Coordinate the gateway and Nomad MIG scaling writers.
4. Deploy a new worker/gateway release and rerun the 64-test correctness gate.
5. Rerun lifecycle, snapshot source, snapshot batch N=1..48, 500-job churn, and
   the hardened fleet filesystem modes.
6. Only after every resource gate and cleanup proof passes, run the 160 held
   burst, targeted autoscaling scenarios, and isolated sawtooth campaign in the
   order documented in `docs/benchmarks.md`.
