# Benchmark results

Raw per-run JSON from the suites in [`../`](../). Ad-hoc results are
**gitignored** because they are machine- and date-specific. Files or directories
prefixed `production_` are deliberately versioned release evidence; they must
identify the deployed release and run ID in metadata and contain no
credentials. Curated numbers live in
[`docs/benchmarks.md`](../../../../docs/benchmarks.md).

## File naming

| Pattern | Producer | Shape |
| --- | --- | --- |
| `production_*.json` | Any suite | Curated single-run production evidence |
| `production_*/` | `scripts/bench-extensive.sh` | Curated multi-file production campaign |
| `snapshot_source_<ts>.json` | `npm run bench:snapshot-source` | Default-source versus snapshot-source create latency |
| `snapshot_batch_<ts>.json` | `npm run bench:snapshot-batch` | Snapshot-source batch operation/readiness scaling |
| `lifecycle_<ts>.json` | `npm run bench:lifecycle` | Create, pause, resume-to-usable, and terminate latency |
| `fleet_<mode>_<n>_<ts>.json` | `benchmarks/fleet-bench.ts` | Gateway create/workload timing, host observations, diagnostics, and cleanup proof |
| `burst_*.json` | `npm run bench:burst` | Create/exec/terminate churn or held-burst results |
| `<mode>_<ts>.json` | `npm run bench` | In-guest SQLite and filesystem workload |

## Current production result

The latest campaign is
[`production_extensive_9b6a9fc_20260729/`](./production_extensive_9b6a9fc_20260729/)
for release `9b6a9fc`:

- Default source: 22/25 ready-pool hits in 8–15 ms; three refill-bound
  creates in 1.756–2.132 s.
- Snapshot batch N=32: 32/32 usable in 14.898 s, versus 6.486 s for the
  default-source baseline.
- Fleet default: 32/32 and 64/64 passed; the 128-way run created 80/128 because
  one worker's jailer `io.max` referenced an absent device.
- Fsync: 64/64 created, 63/64 workloads passed; large: 64/64 passed.
- Snapshot-source density used 452 MiB (15.5%) less PSS than default source
  across 32 additional VMs.
- Every created resource was verified deleted; the final fleet count was zero.

Environment, methodology, security gates, and failure attribution:
[`docs/benchmarks.md`](../../../../docs/benchmarks.md).

## Regenerate

```bash
export HOST_URL=http://<direct-worker>:8080
export GATEWAY_URL=http://<gateway>:9090
export SANDBOX_HOST_KEY=<worker-client-token>
export SANDBOX_API_KEY=<gateway-client-token>
export SSH_HOST=<direct-worker>
export SANDBOX_RELEASE=<release>
export BENCH_RUN_ID=<unique-run-id>
export SINGLE_HOST_COUNT=32

bash scripts/bench-extensive.sh
```

Only promote results after verifying all benchmark resources were cleaned up
and no credentials are present. Deprecated aliases remain for existing
automation: `restore`, `fanout`, `perCloneMs`, `killMs`,
`bench:restore`, and `bench:fanout`.
