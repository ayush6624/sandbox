# Product roadmap

Proposed September 7, 2026 from the [capability map](product-capability-map.md).
The focus is coding-agent workspaces and parallel experiments. No customer
commitment, schedule, production launch, or SLA is implied.

## Verified workflow: a reusable coding workspace

September 13 update: the [SDK example](../sdk/typescript/examples/workspace/README.md)
passed two- and eight-attempt runs plus reuse from a separate process on the
development fleet. It runs all 297 tests in a pinned repository and checks
sibling, source, and saved-snapshot isolation. The
[verification report](prepared-workspace-2026-09-13.md) separates measured setup
and reuse time. Runtime fixes for missing-source errors, request IDs, and a
capacity-notification race pass locally and await rollout approval. Physical
retained-storage accounting remains open.

**User outcome:** prepare a repository and its dependencies once, then start a
fresh attempt and run its tests through the public SDK.

Most runtime capabilities already exist. The feature work is to join them into
one documented, repeatable workflow and close only the gaps that workflow exposes.
It does not require a new template engine or cross-machine async migration.

The first delivery includes a pinned small repository, a preparation script,
a reusable snapshot, an SDK example, and a report comparing fresh preparation
with reuse. It records setup cost separately from create latency and test
completion time. Existing benchmark fixtures and metadata validation are reused.

Acceptance:

- A developer can follow the example without logging into a worker host.
- The saved workspace creates independent attempts with the expected files and
  dependencies. A deliberate mutation in one attempt does not change its sibling
  or the saved source.
- The repository tests produce checked results, and failed preparation or create
  requests produce inspectable errors.
- Timings include first useful command and completed tests, with warm and cold
  conditions disclosed. Errors remain in the report and temporary resources are
  cleaned up.

Impact: gives users a concrete reason to use snapshots and gives us a workload
against which to choose performance improvements. Completion requires a working
example, not a target speedup invented before measurement.

## Then: parallel attempts from the same starting state

**User outcome:** start several independent coding or evaluation attempts from
one prepared workspace and collect every result.

This builds on the first feature and the existing batch API. The delivery is an
SDK workflow that labels attempts, collects output and test results, retains
partial failures, and cleans up the group. Existing Harbor integration can serve
as a separate integration check; rewriting its adapter is not a prerequisite.

Acceptance:

- Every requested attempt has a result or an explicit error, including when a
  request response is lost or the coordinator restarts.
- Sibling changes stay isolated, and a failed attempt does not discard successful
  results from the rest of the group.
- The report separates each attempt's readiness from the time the entire group
  finishes and includes resource usage where coverage is available.
- A bounded concurrency sweep establishes useful capacity before pool tuning or
  larger evaluation runs.

Impact: makes stateful branching useful for an actual agent workflow and reveals
whether the fleet offers worthwhile parallel throughput.

## Then: explain a slow or failed operation

**User outcome:** inspect one operation and see whether it is waiting for
capacity, restoring state, waiting for the guest, complete, or failed.

The delivery extends existing operation records and SDK inspection. It includes
stage timestamps and terminal reasons that survive the relevant service restart.
A dashboard or webhook service is not required for this first version.

Acceptance:

- API and SDK inspection show the last completed stage and the current wait or
  terminal error for a create operation.
- A request ID connects user-visible state to operator diagnostics without
  exposing host credentials or internal filesystem paths.
- An injected capacity delay and a controlled readiness failure are distinguishable.
- Restart does not erase recorded progress or falsely report success.

Impact: shortens both user troubleshooting and our investigations. If the first
two features repeatedly encounter opaque failures, move this feature ahead of
the parallel-attempt work.

## Supporting work and later candidates

Storage reclamation supports retained workspaces. Account for retained bytes in
the first workflow, then implement reference-aware chunk cleanup before treating
long-running retention as a supported promise. Preserve references from active
lazy readers, snapshots, and handoffs.

Remote template builds and a packaged Python SDK are later product candidates.
Prioritize remote builds when host-only image preparation blocks the selected
workflow; prioritize Python packaging when adapter users need a supported general
client. Neither requires rebuilding the runtime capability underneath it.

The intermittent migration readiness bug and network namespace integration stay
deferred as requested. Managed desktop sessions, per-sandbox egress policies,
and shared-service accounts have no near-term delivery commitment.

## How work moves

Keep one main feature in progress. Each feature contains implementation tasks,
verification, documentation, and a runnable user example. The
[development backlog](development-backlog.md) supplies supporting tasks rather
than defining the product direction.

A feature moves from proposed to active when selected, to verification when the
user workflow exists, and to done when its acceptance evidence is recorded.
Deferred bugs retain their impact, evidence, and the condition for reopening.
At each feature review, update the capability map and select the next feature
based on observed user friction and workload results.
