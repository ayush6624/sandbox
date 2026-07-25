# Docs

Self-hosted Firecracker microVM sandboxes with an e2b-style API. Start here:

| Guide | For |
| --- | --- |
| [Quickstart](quickstart.md) | Create your first sandbox in 5 minutes (SDK, CLI, or curl) |
| [Concepts](concepts.md) | How it works: hot creates, snapshots & fan-out, ports, TTLs, multi-host |
| [HTTP API reference](http-api.md) | Every endpoint, request/response shapes, errors, limits |
| [Self-hosting](self-hosting.md) | Run it on your own hardware: single host or a multi-host fleet |
| [Autoscaling latency](autoscaling-latency.md) | Current burst critical path, Modal comparison, and implementation roadmap |
| [Ona-style devboxes](devboxes-roadmap.md) | Design roadmap for repository-aware, editor-accessible development environments |
| [Production readiness plan](production-readiness-plan.md) | Runtime hardening plus the versioned HTTP API and TypeScript SDK migration |
| [P0.2 jailer design](p0-jailer-design.md) | Shared launcher, per-VM identity, chroot path translation, and cgroup v2 rollout |

Also:

- [TypeScript SDK](../sdk/typescript/README.md) — the recommended client (`Sandbox.create()`, e2b-compatible)
- [Benchmarks](benchmarks.md) — latest measured numbers + comparison vs hosted providers ([interactive report](benchmark-report.html)); runnable suites in [`sdk/typescript/benchmarks/`](../sdk/typescript/benchmarks/README.md)
