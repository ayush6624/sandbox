# Docs

Self-hosted Firecracker microVM sandboxes with an e2b-style API. Start here:

| Guide | For |
| --- | --- |
| [Product capability map](product-capability-map.md) | Current user capabilities, evidence, limitations, and deferred gaps |
| [Product roadmap](product-roadmap.md) | Proposed user-facing features and their acceptance criteria |
| [Development backlog](development-backlog.md) | Next tasks, expected impact, and explicitly deferred work |
| [Quickstart](quickstart.md) | Create your first sandbox in 5 minutes (SDK, CLI, or curl) |
| [Prepared coding workspace](../sdk/typescript/examples/workspace/README.md) | Prepare a pinned repository once, run its tests, and verify independent attempts from a snapshot |
| [Prepared workspace verification](prepared-workspace-2026-09-13.md) | Two- and eight-attempt results, discovered runtime fixes, and remaining launch work |
| [Reading the code](reading-the-code.md) | New to the source: a trace-by-trace route through the Go, and how to read AI-generated code |
| [Concepts](concepts.md) | How it works: hot creates, snapshots & fan-out, ports, TTLs, multi-host |
| [HTTP API reference](http-api.md) | Every endpoint, request/response shapes, errors, limits |
| [HTTP API v1](api-v1.md) | Versioned resource contract, standard vocabulary, idempotency, and migration routes |
| [Self-hosting](self-hosting.md) | Run it on your own hardware: single host or a multi-host fleet |
| [Autoscaling latency](autoscaling-latency.md) | Current burst critical path, Modal comparison, and implementation roadmap |
| [Production readiness plan](production-readiness-plan.md) | Runtime hardening plus the versioned HTTP API and TypeScript SDK migration |
| [Usage metering plan](usage-metering-plan.md) | Design for per-sandbox CPU-hours and RAM-hours, durable enough to bill from |
| [Worker memory model](cgroup-memory-model.md) | How cgroups, the 156 MiB overhead, page cache, and physical RAM fit together — read this first |
| [Snapshot peer transfer](snapshot-peer-transfer.md) | Peer-first sparse snapshot replication, GCS fallback, security boundary, and canary gate |
| [Snapshot upload recovery](snapshot-upload-recovery.md) | Persisted retries, worker restart recovery, durable waits, and deletion fencing |
| [Create operation recovery](create-operation-recovery.md) | Durable single and batch creates, request replay, worker assignment, and restart verification |
| [P0.2 jailer design](p0-jailer-design.md) | Shared launcher, per-VM identity, chroot path translation, and cgroup v2 rollout |
| [Tensorlake architecture notes](tensorlake-architecture-notes.md) | Tensorlake blog survey, overlap with this runtime, and a prioritized adoption roadmap |

Also:

- [TypeScript SDK](../sdk/typescript/README.md) — the recommended client (`Sandbox.create()`, e2b-compatible)
- [Benchmarks](benchmarks.md) — latest measured numbers + comparison vs hosted providers ([interactive report](benchmark-report.html)); runnable suites in [`sdk/typescript/benchmarks/`](../sdk/typescript/benchmarks/README.md)
- [RL environments & agent benchmarks](rl-environments-plan.md) — running Terminal-Bench 2.0 (Harbor), `verifiers` rollouts, τ³-bench and AutomationBench on the fleet; adapters in [`examples/rl-environments/`](../examples/rl-environments/README.md)

- [Lazy cross-worker adoption](lazy-cross-worker-adoption.md): memory loading, publication, and dev verification.
- [Bounded chunk cache](chunk-cache.md): disk budget, hydration reservations, metrics, and remaining remote-storage work.
- [Remote chunk storage design](chunk-storage-design.md): generation ownership, runnable models, conditional-write checks, and implementation gates.
- [Peer hibernation transfer](peer-hibernation-transfer.md): direct worker transfers, durable fallback and generation cleanup.
- [Asynchronous peer handoff](async-peer-handoff.md): planned moves before cloud backup completes, exclusive ownership, and source retention.
