# Development backlog

Updated September 13, 2026. The fleet is a development environment with no current
production serving, SLA, or SLO. This is a proposed order of work, not a deployment
commitment.

The [product capability map](product-capability-map.md) records what users can
do today. The [product roadmap](product-roadmap.md) groups the tasks below into
user-facing features. This list is supporting engineering work, not a separate
product roadmap.

## Next tasks

| Order | Task | Impact | Completion evidence |
| --- | --- | --- | --- |
| 1 | Benchmark the current create and fork implementation | Shows where users actually wait and which optimization deserves work next. | Baseline complete on the September 6 build: five warm claims, five snapshot pool misses, and 4/8/16 concurrent forks passed, with command probes, counter evidence, build and guest identities, and verified cleanup. [Report](create-paths-2026-09-13.md). Same-source pool depletion, tail latency, and sustained capacity remain unmeasured. |
| 2 | Complete rollout verification for the prepared-workspace fixes | Makes missing-source errors inspectable and avoids a capacity-notification delay. | The pinned repository workflow passed 2- and 8-attempt fleet runs. Missing-source classification, durable request IDs, and queue notification fixes pass local tests. Rollout approval and after-deploy verification remain pending. [Evidence](prepared-workspace-2026-09-13.md). |
| 3 | Bound memory-chunk cache and object storage growth | Prevents repeated snapshots from filling worker disks or accumulating storage cost indefinitely. | The [bounded worker cache](chunk-cache.md) passes local tests; deployment remains pending. [Generation ownership](chunk-storage-design.md), fenced uploads, and handoff readers are implemented with new-format writes off by default. Reader recovery uses a durable local journal and process-lock proof. Exclusive publisher attempts resume useful backups after process exit and preserve live producers through registry closure. Automatic collection, unrecoverable-source/volume-loss recovery, legacy migration, and fleet reclaim/restore verification remain open. |
| 4 | Expose lifecycle progress and failure reasons | Makes a slow operation explainable and reduces time spent correlating raw logs. | Local startup-failure retention, durable worker delivery, coordinator dispatch/retry records, and public single/batch API/SDK progress and default CLI progress are implemented. Tests cover lost acknowledgements, reopen, ownership checks, legacy upgrades, and success waiting for route reconciliation. [Behavior](create-operation-recovery.md#worker-stage-journal-awaiting-delivery-and-deployment). Worker terminal outcomes now have one stored copy, recovery polls use a selective index, and operation listing reads indexed pages with stable continuation. A total history storage policy and fleet verification remain open. Default CLI creation now accepts independent resource overrides and disabled idle hibernation; older servers require explicit --legacy. |
| 5 | Measure burst capacity and tune warm capacity | Reveals how many simultaneous starts the fleet can absorb and the idle RAM cost of keeping starts fast. | The fixed-arrival runner passed 60 creates at 5/sec with command probes, metrics, and verified cleanup. All were warm claims; this did not reach saturation. [Report](open-loop-2026-09-13.md). The 20/sec stage needs explicit load approval. Aggregate queue-duration telemetry is implemented locally; deployment and per-request stage timing remain open. Change capacity only after identifying the limiting stage. |

The prepared-workspace measurement is recorded in the
[September 13 report](prepared-workspace-2026-09-13.md). It does not complete
task 1's warm-hit/pool-miss separation; the [create-path report](create-paths-2026-09-13.md)
now records that baseline. Task 5 follows that baseline so
capacity changes have a measured justification. These tasks do not require
resolving the intermittent migration stall first.

## Deferred

- **Per-VM network namespace integration:** explicitly backlogged. Existing
  provisioning and jailer support is not wired into the create paths. Revisit
  when fresh timings justify the change. See the
  [identity cost plan](guest-identity-cost-plan.md).
- **Intermittent guest readiness failure after async migration:** unresolved,
  paused at the user's request. A failed VM showed a guest CPU spinning, but no
  exact failing instruction was captured. The last 60 moves passed. No runtime
  fix was deployed. Preserve the
  [investigation record](verification/thaw-readiness-2026-09-07/README.md) and
  resume with a specific hypothesis or a new failure capture.

## Existing work to reuse

Durable create operations, snapshot upload recovery, lazy cross-worker adoption,
peer transfer, and async handoff already have implementations and verification
records. The evaluation adapters also exist. This backlog does not propose
rebuilding them or imply that async migration is fully reliable.
