# Benchmark results

Raw per-run JSON from the suites in [`../`](../). Ad-hoc results are
**gitignored** because they are machine- and date-specific. Files or directories
prefixed `production_` are deliberately versioned release evidence; they must
identify the deployed release and run ID in metadata and contain no
credentials. Curated numbers live in
[`docs/benchmarks.md`](../../../../docs/benchmarks.md).

`development_*.json` files record curated experiments on the existing fleet
while it is used for development. The
[September 5 canary](../../../../docs/development-canary-2026-09-05.md) records
the environment and interpretation; these runs establish no SLA or SLO.

## File naming

| Pattern | Producer | Shape |
| --- | --- | --- |
| `production_*.json` | Any suite | Curated single-run production evidence |
| `development_*.json` | Any suite | Curated development-fleet measurements or failure diagnostics |
| `production_*.json` | Curated by hand from a raw local campaign | Small, reviewable production summary; raw campaign directories stay local |
| `snapshot_source_<ts>.json` | `npm run bench:snapshot-source` | Default-source versus snapshot-source create latency |
| `snapshot_batch_<ts>.json` | `npm run bench:snapshot-batch` | Snapshot-source batch operation/readiness scaling |
| `snapshot_working_set_<ts>.json` | `npm run bench:snapshot-working-set` | Dirty-memory/filesystem fan-out readiness versus forced hydration |
| `lifecycle_<ts>.json` | `npm run bench:lifecycle` | Create, pause, resume-to-usable, and terminate latency |
| `fleet_<mode>_<n>_<ts>.json` | `benchmarks/fleet-bench.ts` | Gateway create/workload timing, host observations, diagnostics, and cleanup proof |
| `burst_*.json` | `npm run bench:burst` | Create/exec/terminate churn or held-burst results |
| `<mode>_<ts>.json` | `npm run bench` | In-guest SQLite and filesystem workload |

## Recorded production campaign

The August 1 campaign is
[`production_extensive_c0d0c0f_20260801/`](./production_extensive_c0d0c0f_20260801/)
for release `c0d0c0f`, alongside
[`production_lifecycle_c0d0c0f_20260801.json`](./production_lifecycle_c0d0c0f_20260801.json)
and
[`production_burst_c0d0c0f_20260801.json`](./production_burst_c0d0c0f_20260801.json):

These results predate the later placement and peer-transfer changes. They do
not identify the current deployment or measure its performance.

- Lifecycle through the gateway: create p50 12 ms, p95 15 ms over 25 cycles.
- Default source: 22/25 ready-pool hits in 7–16 ms; three refill-bound creates
  at 734 ms, 984 ms, and 1.381 s.
- Snapshot batch: 1/2/4/8/16/32 all fully usable, flat at ~764 ms per sandbox
  from N=4 up, versus 6.464 s for the 32-way default-source baseline.
- Fleet: 32/32, 64/64, **128/128**, fsync 64/64, large 64/64 — no failures in
  any run. The 128-way run was 80/128 on the previous release, and fsync was
  63/64.
- Every created resource was verified deleted (352 across the matrix) and the
  fleet returned to its pre-campaign baseline.
- Memory density was **not** re-measured: the script needs a worker with zero
  Firecracker processes, which the resident 8-VM ready pool prevents. See
  `docs/benchmarks.md`.

Environment, methodology, security gates, and what changed since the previous
release: [`docs/benchmarks.md`](../../../../docs/benchmarks.md).

## Regenerate

```bash
export HOST_URL=http://<direct-worker>:8080
export GATEWAY_URL=http://<gateway>:9090
export SANDBOX_HOST_KEY=<worker-client-token>
export SANDBOX_API_KEY=<gateway-client-token>
export SSH_HOST=<direct-worker>
export SANDBOX_RELEASE=c0d0c0f
export BENCH_RUN_ID=<unique-run-id>
export SINGLE_HOST_COUNT=32

bash scripts/bench-extensive.sh
```

Only promote results after verifying all benchmark resources were cleaned up
and no credentials are present. Deprecated JSON field aliases remain for
existing dashboards: `restore`, `fanout`, `perCloneMs`, and `killMs`.

New lifecycle, template-warm, working-set, and peer-transfer reports record
build identities from metrics before and after the run. Check provenance before
promoting a new report:

```bash
npm run bench:validate -- benchmarks/results/candidate.json
```

Run this from `sdk/typescript`. The validator rejects failing runs, missing or
mismatched observed worker releases, failed build scrapes, and missing declared
image hash, runner region, or cache state. Historical reports lack these fields
and are expected to fail. Do not fill them with guessed values to pass the gate.
Environment declarations still require independent verification, and this check
does not establish adequate sample size, workload equivalence, or cleanup beyond
the report's own verdict.
