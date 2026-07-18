# Docs

Self-hosted Firecracker microVM sandboxes with an e2b-style API. Start here:

| Guide | For |
| --- | --- |
| [Quickstart](quickstart.md) | Create your first sandbox in 5 minutes (SDK, CLI, or curl) |
| [Concepts](concepts.md) | How it works: hot creates, snapshots & fan-out, ports, TTLs, multi-host |
| [HTTP API reference](http-api.md) | Every endpoint, request/response shapes, errors, limits |
| [Self-hosting](self-hosting.md) | Run it on your own hardware: single host or a multi-host fleet |

Also:

- [TypeScript SDK](../sdk/typescript/README.md) — the recommended client (`Sandbox.create()`, e2b-compatible)
- [Benchmarks](benchmarks.md) — latest measured numbers + comparison vs hosted providers ([interactive report](benchmark-report.html)); runnable suites in [`sdk/typescript/benchmarks/`](../sdk/typescript/benchmarks/README.md)
- Internals deep-dives: [snapshot fan-out findings](snapshot-fanout-m0-findings.md), [fan-out benchmark report](snapshot-fanout-bench.md)
