# GCP production-readiness validation — 2026-07-26

Status: **hardened-image correctness/security passed; rigorous workload matrix
passed; autoscaling p95 and remaining traffic campaigns are open**

## Hardened-image validation update — 2026-07-27

Repository/control-plane head is `ea0f707`. Workers run release `a223889`.
The active immutable fleet is:

- worker image `sandbox-worker-20260727-000120` (image ID
  `4020148980465518867`);
- golden data image `sandbox-golden-data-20260727-001237` (image ID
  `4459982300482619317`);
- instance template `sandbox-workers-tpl-20260727-002100`.

### Confirmed evidence

| Gate | Result |
|---|---|
| Full worker security gate | Pass on both rebuilt active workers at `a223889` |
| Server-crash recovery | Pass |
| Host-reboot recovery | Pass |
| Full API/SDK/e2e suite | **64/64** in 360.7 s |
| Port-forward suite | **8/8** |
| Local Go suite | **207 tests passed** |
| Full Go race suite | Pass |
| TypeScript SDK suite | **49/49 tests passed** |
| SDK typecheck and build | Pass |
| Deterministic OpenAPI generation | Pass |
| Shell syntax validation | **29 scripts passed** |
| Bounded resource exhaustion | Pass on disposable worker |
| Live client-token rotation | Pass with exact restoration |
| Runtime npm audit | **0 vulnerabilities** |

The security gate covered live jailer UID/GID allocation, per-VM chroot and
namespaces, cgroup controls, seccomp, VMM output bounds, non-root guest/SSH
access, unique independent and snapshot-derived identities, guest isolation
with permitted egress, and expected/VMM-crash cleanup. The recovery campaign
then separately proved server-process crash recovery and host reboot
reconciliation.

The rebuilt-image rerun is complete. The transient restored-guest
`ssh.service` failure was fixed in `a223889`; the new image then passed the
complete contract, SDK/e2e, security, server-crash, and host-reboot sequence.
The first rerun also found an advertised port-forward host parsing defect,
fixed in gateway commit `1ae90f7`.

### Immutable release metadata

- Host: Ubuntu 24.04, kernel
  `6.17.0-1021-gcp #24~24.04.1-Ubuntu`, Firecracker and jailer `v1.15.0`.
- Guest kernel: `6.18.36+`; kernel SHA-256
  `25576852390f4883c913ba26c42e6de5569c8a0cff6769800924db8631a3b6c3`.
- Rootfs: Ubuntu 24.04 LTS, ext4 UUID
  `ac1c575a-3944-44ad-8953-030155a71aef`, SHA-256
  `13d7b4a1cccd6f60c2f9b3d9877bee35f2903a4ea7d9fa84747945802cea46c7`.
- Golden manifest SHA-256
  `bf6e04142b602943197086b48afe41674d7acb0fe1f471f297f54450d8cf591b`;
  golden snapshot ID `93372ba7-f355-4df7-b58b-935f93da3061`.
- Worker artifact SHA-256
  `30d97aaa8771300282ca6c75a299e116cce1d643992a8b842e4cd8544c7a6a8d`;
  gateway artifact SHA-256
  `73f7a6215247342668eeef040862266533bc63c14af3cf5cbb87d7a84eb3d55b`.

Those two binaries reported `vcs.modified=true`. Their hashes and GCP/GCS object
generations identify the deployed bytes exactly, but they were not clean builds.

**Resolved 2026-07-28.** Go stamps `vcs.modified=true` whenever
`git status --porcelain` is non-empty, untracked files included, so leftover
worktree and editor state was enough to dirty every release. Those paths are now
ignored (`af3833a`), and the release built from that clean tree stamps:

- `vcs.revision=af3833af3908362f06876518b11b6bf3ae205120`,
  `vcs.time=2026-07-28T06:46:56Z`, **`vcs.modified=false`**, Go 1.25.3;
- `sandbox` SHA-256
  `2d693a64fa087dd7459a17574c894e26486a523e3f07e6b788ac6b94b035a715`,
  22,196,862 bytes, GCS generation `1785221285677119`;
- `sandboxd` SHA-256
  `eb7eab6eba42d502532012ef918d61c0050a35b14db6386edaf65c24f7e52cc6`,
  9,855,960 bytes, GCS generation `1785221270889960`;
- published at `gs://ratio-experiments-sandbox-releases/releases/af3833a/`.

The control plane runs this release. Workers still run `a223889` — the gateway
change is control-plane only, and `worker-release` persists across the gateway
restart, so no worker was release-gated during the deploy. Rebaking the worker
image onto a clean-stamped `sandboxd` remains outstanding.

### 2026-07-27 rigorous workload matrix

All rows below completed with exact sandbox cleanup:

| Workload | Result |
|---|---|
| Snapshot-source latency | default p50 1.906 s; snapshot p50 1.927 s (0.99x) |
| Snapshot batch create | N=1,2,4,8,16,32,48 all usable; N=48 21.371 s total / 445 ms per sandbox |
| Fleet default | 32/32 (create p95 5.912 s), 64/64 (5.398 s), 128/128 (5.109 s) |
| Fleet fsync | 64/64; workload p95 50.350 s |
| Fleet large | 64/64; workload p95 229.021 s |
| Sustained churn | 500/500 in 100.092 s; create p95 22.119 s; terminate p95 206 ms |
| Memory density | N=48 snapshot-source PSS 4,708 MiB vs default 5,227 MiB, 9.9% lower |

The successful 128 and large-workload runs executed directly on the control
VM. Laptop runs through one SSH port forward saturated that tunnel and were
discarded as transport artifacts, not product results.

### Autoscaling finding and fix

The first canonical 160-create held burst was lossless but failed latency:
160/160 succeeded with zero residual sandboxes, while create p95/max were
231.297/232.699 s. Prometheus counted only heartbeat-visible `slots_used`, not
gateway reservations, so 96 assigned plus 64 queued requests initially
requested only three workers. The remaining 16 then waited behind the MIG
standby-refill stability window.

Commit `e0b3588` adds the per-host-clamped `sandbox_slots_committed` metric and
uses it in the desired-worker rule. The next run remained lossless and reduced
p95/max to 36.468/37.268 s. Commit `ea0f707` also tightened the cheap
gateway/rule/autoscaler cadence to 5 seconds. Its release-attributed final run
again passed correctness and cleanup (160/160, zero errors, zero listed
sandboxes; p50/p90/p95/p99/max
15.739/35.056/36.162/37.528/38.422 s), but the p95 acceptance bound is 30 s.

Decomposing that run showed ~10.2 s of the queued-create path was
Nomad-autoscaler decision latency, ~13.0 s worker resume, and the rest
per-worker drain. Commit `af3833a` therefore makes the **gateway** the sole
scale-out writer — level-triggered on every demand change, with a grow-only
watermark bounded at demand+1 — and caps the autoscaler to scale-in only via
the `sandbox:workers_scale_in_ceiling` recording rule. Both PromQL branches
were verified against the live fleet before deploying.

The rerun on the same floor and worker release `a223889` passes the SLO:
160/160, zero errors, verified cleanup, p50/p95/p99/max
**16.004/26.074/26.805/26.995 s**, wall 48.2 s. Exactly one resize was issued
(`sandbox_direct_scale_out_total 1`, zero failures). Demand → usable capacity
fell from ~23.2 s to ~13.0 s, which is now pure worker resume time.

Autoscaling correctness, the 60-second maximum, and the 30-second p95 bound
therefore all pass.

**One defect was found in the same run.** The scale-in cap does not actually
confine the autoscaler to scale-in: ~2.5 minutes after the burst drained it
logged `from=5 to=6 reason="scaling up because metric is 6"`. The ceiling is
`max(sandbox_hosts_live, sandbox_scale_out_requested)`, and `hosts_live` (8)
exceeded the MIG `targetSize` (5) because resumed standby workers heartbeat to
the gateway without being counted in that target — so a latched
`max_over_time` peak of 6 was a legal scale-up. This did not affect the p95
result, but the single-writer invariant is not yet enforced and the fleet can
still over-scale after demand is gone. The fix is to cap on the MIG's real
`targetSize`, which the gateway can read and export directly; not yet
implemented.

Still open: that fix, the targeted traffic group, the isolated sawtooth
campaign, and a confirmed return to the fleet floor.

## Remediation validation

The sections from here through “Earlier failed campaign” are retained as
historical evidence and are superseded by the rebuilt-image results above.

The three blockers found in the earlier `p1-api-20260726-2` campaign were
fixed in focused commits and deployed together as `prod-fixes-20260726-1`
(`75ecb6b`):

- `4434dc9` serializes Firecracker loads that share a baked snapshot rootfs
  path, covering hot create, resume, and snapshot batch creation;
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
- `6a08870` — recover run-owned resources after operation-poll disconnects and
  make snapshot cleanup bounded and retryable.
- `3d237a6`, `74a80de`, `e8dd721` — use public fleet inventory, explicit
  request budgets, and stage workloads before timed execution.
- `a850ee1`, `e9308be`, `f548fa8` — make density cleanup idempotent, stress
  concurrent operation JSON polling, and retry malformed poll responses.
- `dfa6d19`, `2ca73c9` — normalize evidence paths and update the SDK
  development runner.
- `e0b3588`, `ea0f707` — count in-flight create reservations in autoscaling
  demand and tighten the scale-out control loop.

These are in addition to the v1 terminology/API migration and live-campaign
guard commits immediately preceding them.

## Evidence

Raw results remain on the GCP control node under:

`/home/ayush/web-sandbox/tests/results/gcp-20260726T063507Z/`

Current local gitignored evidence is under:

- `tests/results/run_2026-07-27T07-44-47-400Z.json`;
- `sdk/typescript/benchmarks/results/gcp-20260727-extensive-rerun/`;
- `tests/results/autoscale-20260727-legacy-held-rerun/`;
- `tests/results/autoscale-20260727-legacy-held-committed-fix/`.

The final `ea0f707` autoscale bundle remains on the control VM at
`/home/ayush/web-sandbox/tests/results/autoscale-20260727-legacy-held-final/`;
copying it locally was blocked when the GCP command budget expired. Every
autoscale bundle includes `run.json`, `timeline.jsonl`, `benchmark.log`, and
`SHA256SUMS`.

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

## Remaining sequence

The old steps 1–5 are complete. Continue only after GCP command capacity is
available:

1. Copy the final `ea0f707` autoscale evidence bundle locally and inspect its
   precise demand-to-resize/readiness timeline.
2. Close or explicitly revise the p95 ≤30 s autoscaling SLO; do not treat the
   current 36.162 s p95 as a pass.
3. Return to the exact two-running/six-suspended floor, then run the targeted
   traffic group followed by the isolated sawtooth campaign.
4. Rotate the worker-control and host credentials exposed by a diagnostic
   Nomad inspection, roll the job safely, and repeat the credential-domain
   gate.
5. Produce clean, reproducible worker and gateway binaries so release metadata
   no longer reports `vcs.modified=true`.
