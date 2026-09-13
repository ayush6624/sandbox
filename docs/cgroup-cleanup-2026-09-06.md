# Cgroup cleanup verification, September 6, 2026

The cleanup fix is deployed to the gateway and both development workers as
`fb09662-dev-20260905195558`. Six snapshot/terminate cycles passed across the
two workers. A seventh cycle passed at all 48 default-size VM reservations.
Every run returned to eight populated warm-guest cgroups, totaling 9440 MiB,
with no empty reservation leaves or OOM kills.

The run occurred on September 5 in UTC, September 6 in Asia/Kolkata. This
fleet is used for development, with no production traffic or SLA/SLO target.

## Failure and fix

A syscall trace caught `unlinkat(..., AT_REMOVEDIR)` returning `EBUSY` after
source termination while its snapshot upload was pending. The old cleanup
discarded the error, deleted the jail, and released the identity. Kernel
teardown eventually left an empty cgroup, but its 1180 MiB memory reservation
remained. The old-release verification found ten such leaves, totaling
11,800 MiB of stale reservations.

The fix retries `EBUSY` every 10 ms for up to two seconds. Cleanup removes the
cgroup before deleting the jail and releasing its identity. A persistent
failure logs the error and retains that recovery state. Startup reconciliation
also removes empty cgroups with canonical VM UUID names when their jail is
missing. Populated or unverifiable orphan cgroups stop reconciliation.

Memory admission still counts empty reservation leaves. A starting VM can
reserve a cgroup before its process enters it, so omitting empty leaves from
accounting would permit overcommit.

See the [syscall trace](verification/cgroup-cleanup-2026-09-06/rmdir-before.log)
and [memory model](cgroup-memory-model.md#releasing-reservations-after-termination).

## Live results

Each cycle starts a default 1024 MiB sandbox, continuously touches 256 MiB of
random guest memory, captures a snapshot, observes its `local` state while
upload is pending, then deletes the source and snapshot. The verifier checks
HTTP 404 after deletion and reads actual kernel cgroup limits and population.

| Run | Captured VM reservations | Result after cleanup | Evidence |
| --- | --- | --- | --- |
| Old release, one cycle | Baseline already contained ten empty leaves | Failed, 21,240 MiB remained reserved | [Before](verification/cgroup-cleanup-2026-09-06/before-verified.json) |
| Updated source worker, three cycles | Nine, including eight warm guests | Passed, 9440 MiB and zero empty leaves after each cycle | [Source](verification/cgroup-cleanup-2026-09-06/after-28.json) |
| Updated target worker, three cycles | Nine, including eight warm guests | Passed, 9440 MiB and zero empty leaves after each cycle | [Target](verification/cgroup-cleanup-2026-09-06/after-35.json) |
| Updated source, 39 idle fillers and one source | 48, totaling 56,640 MiB | Capture passed, then all fillers were deleted and reservations returned to 9440 MiB | [Full occupancy](verification/cgroup-cleanup-2026-09-06/full-occupancy.json) |

The full-occupancy check fills the reservation budget. It does not dirty every
guest's full memory allowance or establish a throughput limit. Kernel
`memory.high` events increased during capture as expected from snapshot
throttling. `oom` and `oom_kill` stayed at zero.

Deployment created new Nomad task cgroups. The before/after baseline therefore
does not, by itself, prove startup orphan recovery. Separate real-kernel tests
verify orphan recovery, refusal to remove a live orphan, transient busy retry,
and bounded failure without killing a live process. All four passed. Package
tests passed for `internal/vm`, `internal/server`, and `cmd/sandbox`. A focused
regression also verifies that failed cleanup retains the jail and identity.

The full gateway smoke test passed under Node 22, including create, exec,
PTY shell and exit, deletion, and the deleted-ID WebSocket 4404 response.
The rollout's system-Node-18 smoke had used its REST-only fallback.

Review also found that mismatched API and SSH addresses could make the
verifier watch an unchanged worker. The verifier now requires one new
populated cgroup and increased reserved memory on the observed worker during
capture. It compares identities against each cycle's start because warm-guest
identities change between cycles. A [deliberately mismatched run](verification/cgroup-cleanup-2026-09-06/mismatch-rejected.json)
failed and cleaned its resources. A [matching run](verification/cgroup-cleanup-2026-09-06/verifier-confirmed.json)
passed. Three further cycles with the final per-cycle identity assertion
[also passed](verification/cgroup-cleanup-2026-09-06/verifier-final.json), each
returning to 9440 MiB and zero empty leaves.

The [final inventory](verification/cgroup-cleanup-2026-09-06/final-inventory.json)
records both worker releases, eight warm guests per worker, and zero active
sandboxes. The pre-existing September 3 hibernated sandbox and August 26
snapshot remain. No verification resources remain. The per-cycle report's
`release` field is null because the worker process does not export
`SANDBOX_RELEASE`; the fleet inventory supplies the observed release.

## Repeat the checks

From the control VM checkout, load the existing worker credential without
printing it. The worker SSH identity needs permission to read its cgroups
through `sudo python3`.

```bash
. infra/gcp/fleet-secrets.env
export SANDBOX_API_KEY="$HOST_TOKEN"
python3 scripts/verify-cgroup-cleanup.py \
  --api-url http://10.128.0.28:8080 \
  --worker-ssh ayush@10.128.0.28 \
  --identity "$HOME/.ssh/google_compute_engine" \
  --cycles 3 --output /tmp/cgroup-cleanup.json
```

For this fleet's 48-slot configuration with eight warm guests, add
`--fillers 39 --cycles 1` to test full occupancy. Fillers use the default
guest size. Run against an otherwise idle worker so unrelated activity does
not change the reservation baseline.

Run the kernel regressions on a Linux host with writable cgroup v2:

```bash
go test -c -o /tmp/cgroup-vm.test ./internal/vm
sudo env SANDBOX_TEST_CGROUP_ROOT=/sys/fs/cgroup \
  /tmp/cgroup-vm.test -test.run TestCgroupCleanupKernel -test.v
```

The tests create and remove their own cgroups and test processes under the
selected root. Without `SANDBOX_TEST_CGROUP_ROOT`, they skip.

## Next architectural work

The cleanup and full-occupancy checks close the immediate follow-up from the
[development canary](development-canary-2026-09-05.md). Durable public
operations and upload retry/reconciliation remain the next implementation
work from the [ASCII review](ascii-competitive-review-2026-09-05.md).
