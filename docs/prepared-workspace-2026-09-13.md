# Prepared workspace verification

The public SDK workflow now prepares a pinned repository, runs its tests,
saves a durable snapshot, and verifies independent attempts. Start with the
[workspace example](../sdk/typescript/examples/workspace/README.md).

The workflow passed on the existing US development fleet on September 13, 2026.
Error-path verification exposed three runtime issues. Their fixes pass local
tests but have not been deployed. Automatic approval review rejected the shared
development rollout pending explicit user authorization. This is not a launch
certification.

## Environment and workload

Both workers and the gateway ran `fb09662-dev-20260906174946`. The workers were
`sandbox-workers-us-frr2` and `sandbox-workers-us-qslx`, both `n2-standard-16`
in `us-central1-a`. Each reported eight warm VMs and 40 free slots before the
campaign. No fleet capacity was added. Requests originated on the control VM
with Node 18.19.1 and SDK 2.9.1.

Both deployed base images were independently hashed and matched
`f128719bdcbdcb61533b9a127c4e83b73d8225e17c7b248d96c90975bdcc2fa3`.
Successful reports observe gateway and worker builds before and after each run
and pass the existing provenance validator. The default guest has two vCPUs,
1,024 MiB RAM, and Python 3.12. These runs use the default snapshot path;
they do not exercise async migration or force cold cross-worker placement.

The workload is itsdangerous 2.2.0, pinned to commit
`096c8d42545d3b68ea21a4f890fb2b2d8979c0bd`. Preparation downloads that commit,
creates a venv, installs its pinned upstream test requirements and Flit build
backend, and installs the repository in editable mode. Verification runs all
297 upstream tests, rejects skips and failures, compares installed dependencies,
and hashes every tracked repository file.

## Results

Each row is one run, not a latency percentile or a sustained-capacity result.
Fresh preparation uses a new guest and repository download. Hosts and warm
pools stay running, and caches are not evicted. Batch command and test timings
start at submission and include operation polling.

| Measurement | Two attempts | Eight attempts |
| --- | ---: | ---: |
| Fresh create through first command | 0.158 s | 0.177 s |
| Repository and dependency preparation | 10.193 s | 10.447 s |
| Fresh create through completed tests | 11.970 s | 12.228 s |
| Snapshot capture | 0.912 s | 0.903 s |
| Additional wait for durability | 5.634 s | 6.141 s |
| Reused attempts through first command, range | 1.572–1.591 s | 2.839–2.860 s |
| Reused attempts through completed tests, range | 4.210–4.212 s | 5.860–6.097 s |
| Successful attempts | 2/2 | 8/8 |

Raw reports: [two attempts](../sdk/typescript/benchmarks/results/development_workspace_n2_20260913.json)
and [eight attempts](../sdk/typescript/benchmarks/results/development_workspace_n8_20260913.json).
Snapshot setup is paid before the reuse measurements. The observations do not
establish a provider ranking or predict performance for larger repositories.

Each run edits a tracked Python source file in one child and reruns all tests.
Every sibling and the original source retain the original file. Another child
created afterward from the saved snapshot retains the original full repository
hash. The reports include the changed hash and the identities checked.

The eight-attempt run briefly retained its snapshot. A separate SDK process
then created another workspace and passed all 297 tests after the preparation
runner had deleted its source and children. The file and dependency hashes
matched the original. The snapshot was subsequently deleted and returned 404.
Final inventory contained exactly the two preexisting paused sandboxes.

The workspace occupied 20,209,664 allocated bytes inside each guest. This is
not an accounting of snapshot artifacts, shared memory chunks, bases, or GCS
retention. Reference-aware storage reclamation and physical retained-byte
accounting remain open.

## Failures and fixes

Two initial preparation runs failed because the example omitted Flit, then
Freezegun. The preparation script now installs the upstream pinned test
requirements instead of a hand-selected test dependency list. Both failed runs
preserved command output and cleaned up their source VMs.

The live missing-source probe then found `500 sandbox_create_failed` for
nonexistent snapshots and templates, in both single and batch creates. Error
`request_id` fields were empty, including on replay. Local fixes now:

- Preserve a missing-source result through local/GCS lookup and record
  `404 source_not_found`. Deleted snapshots also return 404. Corrupt metadata
  and unavailable peers with unfinished backups remain server errors.
- Persist the original HTTP request ID atomically with create acceptance.
  Single-create errors, batch item errors, and replays retain it after restart.
  Acceptance logs connect it to the operation ID. Migration preserves older
  records without inventing their missing request IDs.
- Subscribe to capacity notifications before checking placement. This fixes a
  separate queue race found during the broader tests, where already-available
  capacity could be missed until the 250 ms fallback poll.

These fixes have failing-before and passing-after regression tests:

- `TestCreateCommandSourceErrors`
- `TestCreateFailureRetainsRequestIDAfterReopen`
- `TestAwaitHostChecksCapacityBeforeWaitingForNotification`

`TestRequestIDMigrationPreservesExistingOperations` checks upgrade and repeated
reopen/replay. The full Go suite passed, as did targeted race tests for the
changed server, gateway, operation, and API paths. The Linux gateway/worker
binary built successfully. SDK validation passed 80 tests, typecheck, and build.

The repeatable [public error verifier](../scripts/verify-create-source-errors.mjs)
records single and batch failures, checks replay, and cleans up tagged resources.
Its before-fix fleet run failed as expected. It has not yet passed against a
deployed fixed binary. After an approved rollout, run from `sdk/typescript`:

```bash
node --import tsx ../../scripts/verify-create-source-errors.mjs source-errors-after.json
```

Run the standard rollout smoke and workspace example again against that same
observed release. Local raw failure, reuse, and final-inventory records are in
`docs/verification/prepared-workspace-2026-09-13/`.

## Remaining product work

This example closes the missing end-to-end repository-workspace demonstration.
Its coordinator does not resume after a process restart. The next parallel
workflow must preserve every attempt's result across coordinator interruption,
build on durable operation replay, and measure a bounded concurrency sweep.

Warm-hit versus pool-miss measurements, fixed-arrival burst testing,
reference-aware storage cleanup, and durable placement/restore/readiness stage
diagnostics remain open. The migration-readiness investigation, network
namespace integration, and shared-service tenant controls remain deferred.

ASCII's current API still documents managed agent prompts, an event feed,
desktop access, and account-scoped keys. Runtime snapshot and command support
does not establish parity with those product workflows. No new parity or price
ranking is claimed here. [ASCII public API](https://docs.ascii.dev/box/api/v1).
