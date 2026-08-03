# Docs

Self-hosted Firecracker microVM sandboxes with an e2b-style API. Start here:

| Guide | For |
| --- | --- |
| [Quickstart](quickstart.md) | Create your first sandbox in 5 minutes (SDK, CLI, or curl) |
| [Concepts](concepts.md) | How it works: hot creates, snapshots & fan-out, ports, TTLs, multi-host |
| [HTTP API reference](http-api.md) | Every endpoint, request/response shapes, errors, limits |
| [HTTP API v1](api-v1.md) | Versioned resource contract, standard vocabulary, idempotency, and migration routes |
| [Self-hosting](self-hosting.md) | Run it on your own hardware: single host or a multi-host fleet |
| [Autoscaling latency](autoscaling-latency.md) | Current burst critical path, Modal comparison, and implementation roadmap |
| [Ona-style devboxes](devboxes-roadmap.md) | Design roadmap for repository-aware, editor-accessible development environments |
| [Production readiness plan](production-readiness-plan.md) | Runtime hardening plus the versioned HTTP API and TypeScript SDK migration |
| [Usage metering plan](usage-metering-plan.md) | Design for per-sandbox CPU-hours and RAM-hours, durable enough to bill from |
| [Audit log plan](audit-log-plan.md) | Design for a durable who-did-what record: forensics, dispute resolution, compliance |
| [GCP validation — 2026-07-26](gcp-validation-20260726.md) | Latest production-readiness campaign, blockers, evidence, and rerun gate |
| [P0.2 jailer design](p0-jailer-design.md) | Shared launcher, per-VM identity, chroot path translation, and cgroup v2 rollout |

Also:

- [TypeScript SDK](../sdk/typescript/README.md) — the recommended client (`Sandbox.create()`, e2b-compatible)
- [Benchmarks](benchmarks.md) — latest measured numbers + comparison vs hosted providers ([interactive report](benchmark-report.html)); runnable suites in [`sdk/typescript/benchmarks/`](../sdk/typescript/benchmarks/README.md)
