# Benchmark results

Raw per-run JSON from the suites in [`../`](../). Everything here except this
README is **gitignored** — results are machine- and date-specific, so they're
regenerated rather than versioned. The curated, human-readable numbers live in
[`docs/benchmarks.md`](../../../../docs/benchmarks.md).

## File naming

| Pattern | Producer | Shape |
| --- | --- | --- |
| `restore_<ts>.json` | `npm run bench:restore` | cold-boot vs snapshot-restore latency: mean/p50/p90/min/max + raw samples, `speedup_p50` |
| `fanout_<ts>.json` | `npm run bench:fanout` | `rows[]` of `{n, wallMs, perCloneMs, ok}` + a cold-boot `baseline` |
| `fleet_<mode>_<n>_<ts>.json` | `benchmarks/fleet-bench.ts` | N sandboxes via the gateway: create/workload latency percentiles per config |
| `burst_*.json` | `benchmarks/burst-bench.ts --output` | churn/held-burst results: ok/err counts, retries, create percentiles |
| `<mode>_<ts>.json` | `npm run bench` (`run-bench.ts`) | in-guest SQLite+fs workload (upstream `sandbox-sqlite-bench` shape) |
| `extensive/` | `scripts/bench-extensive.sh` | one full sweep: `latency.json`, `fanout.json`, `fleet_*.json`, `mem.json` (PSS/RSS density) |

## Headline results (July 2026, GCP n2-standard-8, guests 2 vCPU / 1 GB)

- **Restore vs cold boot:** p50 212 ms vs 3463 ms — 16.3× (`extensive/latency.json`)
- **Fan-out:** 32 clones in 2.68 s (84 ms/clone), 64/64 usable at N=64 (`extensive/fanout.json`)
- **Memory density @ N=64:** 925 MB PSS fan-out vs 10.1 GB cold-boot (`extensive/mem.json`)
- **Hot create:** 199–271 ms server-side (measured live, not a suite output)

Full context, comparison against hosted providers, and caveats:
[`docs/benchmarks.md`](../../../../docs/benchmarks.md).

## Regenerate

```bash
cd sdk/typescript
export SANDBOX_API_URL=http://<host>:8080 SANDBOX_API_KEY=<token>
npm run bench:restore
npm run bench:fanout
node benchmarks/burst-bench.ts --count 500 --concurrency 96 --retry-ms 250 --output results/burst.json
HOST_IP=<ip> SSH_HOST=<ip> HOST_TOKEN=... GATEWAY_TOKEN=... bash ../../scripts/bench-extensive.sh
```
