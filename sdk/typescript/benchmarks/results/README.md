# Benchmark results

Raw per-run JSON from the suites in [`../`](../). Ad-hoc results are
**gitignored** because they are machine- and date-specific. Files prefixed
`production_` are deliberately versioned release evidence; they must identify
the deployed release and run ID in metadata and contain no credentials. The
curated, human-readable numbers live in
[`docs/benchmarks.md`](../../../../docs/benchmarks.md).

## File naming

| Pattern | Producer | Shape |
| --- | --- | --- |
| `production_*.json` | Any suite below | Curated, committed production-release evidence |
| `snapshot_source_<ts>.json` | `npm run bench:snapshot-source` | default-source vs snapshot-source create latency; retains deprecated `cold_boot` and `restore` keys |
| `snapshot_batch_<ts>.json` | `npm run bench:snapshot-batch` | snapshot-source batch operation/readiness scaling with per-item diagnostics and cleanup errors; retains deprecated `perCloneMs` and `fanout` aliases |
| `lifecycle_<ts>.json` | `npm run bench:lifecycle` | create/pause/resume-to-usable/terminate latency; retains deprecated lifecycle aliases |
| `fleet_<mode>_<n>_<ts>.json` | `benchmarks/fleet-bench.ts` | N sandboxes via the gateway: compatible create/workload percentiles plus per-resource cleanup proof and failure diagnostics |
| `burst_*.json` | `npm run bench:burst -- --output ...` | create/exec/terminate churn or held-burst results; retains deprecated `killMs` |
| `<mode>_<ts>.json` | `npm run bench` (`run-bench.ts`) | in-guest SQLite+fs workload (upstream `sandbox-sqlite-bench` shape) |
| `extensive/` | `scripts/bench-extensive.sh` | one full sweep: `latency.json`, `fanout.json`, `fleet_*.json`, `mem.json` (PSS/RSS density) |

Every new host-side result includes a `metadata` object identifying its schema,
workload, run, API/SDK, deployed release, and redacted target. Set
`SANDBOX_RELEASE` (or `BENCH_RELEASE`) and `BENCH_RUN_ID` for attributable runs.

## Headline results (July 2026, GCP n2-standard-8, guests 2 vCPU / 1 GB)

- **Restore vs cold boot:** p50 212 ms vs 3463 ms — 16.3× (`extensive/latency.json`)
- **Snapshot-source batch create:** historically called fan-out; 32 sandboxes in
  2.68 s (84 ms/sandbox), 64/64 usable at N=64 (`extensive/fanout.json`)
- **Memory density @ N=64:** 925 MB PSS fan-out vs 10.1 GB cold-boot (`extensive/mem.json`)
- **Hot create:** 199–271 ms server-side (measured live, not a suite output)
- **Production ready-pool create (`9b6a9fc`, 2026-07-29):** lifecycle p50
  200 ms / p95 304 ms over 25 iterations; 16-way held burst completed 16/16
  with create p50 426 ms / p95 600 ms from the remote benchmark client

Full context, comparison against hosted providers, and caveats:
[`docs/benchmarks.md`](../../../../docs/benchmarks.md).

## Regenerate

```bash
cd sdk/typescript
export SANDBOX_API_URL=http://<host>:8080 SANDBOX_API_KEY=<token>
npm run bench:snapshot-source
npm run bench:snapshot-batch
npm run bench:burst -- --count 500 --concurrency 96 --retry-ms 250 --output results/burst.json
HOST_IP=<ip> SSH_HOST=<ip> HOST_TOKEN=... GATEWAY_TOKEN=... bash ../../scripts/bench-extensive.sh
```

Only promote a result into the tracked `production_` namespace after verifying
all benchmark resources were cleaned up and the ready pool returned to its
pre-run capacity.

`bench:restore` and `bench:fanout` remain deprecated aliases for existing
automation.
