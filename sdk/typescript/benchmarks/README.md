# sandbox benchmark (all-TypeScript)

Our own version of [tensorlakeai/sandbox-sqlite-bench](https://github.com/tensorlakeai/sandbox-sqlite-bench),
rebuilt end-to-end in TypeScript for what sandbox sandboxes actually run:
**Node 22 with the built-in `node:sqlite` module and `node:worker_threads`** —
zero npm dependencies, no Python, no native addons.

- **`benchmark.ts`** — the in-guest workload. It keeps the upstream suite's 11
  SQLite operations and three modes (default / fsync / large), then adds our own
  **filesystem dimension** the SQLite-only suite lacks. Node 22 runs this `.ts`
  file directly via type-stripping (`node --no-warnings benchmark.ts`), so it
  needs no build step or `tsx` inside the guest.
- **`run-bench.ts`** — the host orchestrator (a TS rewrite of upstream's
  `run_benchmarks.py`). It drives a sandbox microVM through this SDK:
  **create → detect specs → copy `benchmark.ts` into the guest → run it → parse
  its JSON → tear down.**
- **`snapshot-source-bench.ts`** — compares default-source creation with
  snapshot-source creation using `SandboxClient`.
- **`snapshot-batch-bench.ts`** — scales the typed v1 batch operation over a
  single immutable snapshot source.
- **`snapshot-working-set-bench.ts`** — repeats snapshot fan-out with 256 MiB
  of continuously dirtied anonymous memory and roughly 436 MiB of mixed
  filesystem state, reporting command readiness separately from full working-
  set hydration.
- **`fleet-bench.ts`** and **`burst-bench.ts`** — exercise fleet-wide workload
  throughput and create/exec/terminate churn through the v1 client.
- **`lifecycle-bench.ts`** — measures typed create, pause, resume-to-usable, and
  terminate latencies.

## What it measures

The 11 SQLite operations (sequential + batch inserts, COUNT, range/LIKE queries,
updates, deletes, a transaction block, aggregates, a join, and concurrent reads),
plus four filesystem operations:

| Op | What it does |
| --- | --- |
| `fs_write_many` | Write N ~4 KB files, `fsync`'ing each (real per-file durability cost) |
| `fs_read_many` | Read them all back |
| `fs_large_write` | Write a large blob (32–128 MB by mode) in 1 MB chunks, then one `fsync` |
| `fs_large_read` | Read the blob back |

These exercise the guest's real per-VM ext4 disk, which is the thing that
actually distinguishes one sandbox from another.

**Concurrency is real.** The concurrent-read test spawns N `worker_threads`,
each with its own `DatabaseSync` connection, so it uses multiple cores the same
way the Python suite's OS threads do. (`node:sqlite` is synchronous and blocks
the event loop, so workers are the only way to get true parallelism.)

## Run it

```bash
cd sdk/typescript
npm install

export SANDBOX_API_URL=http://<host>:8080
export SANDBOX_API_KEY=<key>

npm run bench                              # default mode, 3 iterations
npm run bench -- --mode fsync              # synchronous=FULL, stresses fsync
npm run bench -- --mode large              # ~35 MB DB + 128 MB blob, exceeds cache
npm run bench -- --mode default --iterations 5
npm run bench -- --output results/mine.json
npm run bench:snapshot-source -- --iterations 25
npm run bench:snapshot-batch -- --counts 1,2,4,8,16,32 --baseline
npm run bench:snapshot-working-set -- --counts 1,4,8,16,24,32 --rounds 2
npm run bench:template-warm -- --template-id <id> --count 4 --rounds 3
npm run bench:fleet -- --count 64 --mode default
npm run bench:burst -- --count 500 --concurrency 96 --retry-ms 250
npm run bench:lifecycle -- --iterations 25
```

Use `--guest-memory-mib 2048` with `bench:snapshot-working-set` to make the
source a cold-booted 2 GiB guest when deliberately separating working-set size
from guest cgroup headroom. Leaving it unset exercises the production template
and warm-clone snapshot path.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--mode` | `default` | `default` (WAL, ~60k rows), `fsync` (DELETE journal + `synchronous=FULL`), `large` (WAL, 8 MB cache, ~250k rows) |
| `--iterations` | `3` | Runs per mode; >1 reports mean/stddev/min/max |
| `--output` | `results/<mode>_<ts>.json` | Where to write the result JSON |

## Comparability caveat

These numbers are **not 1:1 comparable** to the upstream Python results or our
earlier Python run:

- **Different language binding** — `node:sqlite` (a C++ binding) vs CPython's
  `sqlite3`. The tight insert/query loops are dominated by per-call binding
  overhead, which differs between the two.
- **Different SQLite version** — `node:sqlite` bundles **SQLite 3.51.x**; the
  guest's Python `sqlite3` is 3.45.x.
- The filesystem ops have no upstream counterpart at all.

So treat this as a **sandbox-native** benchmark — most meaningful for
comparing sandbox against itself across configs (RAM, vCPU, modes) and over
time. For a cross-provider comparison you'd run the same binding everywhere.

## Output

Prints `benchmark.ts`'s per-iteration breakdown, then a comparison table and
ranking, and writes a one-element array (one provider) to `results/` — the same
per-provider shape as upstream `results/*.json`. The `results/` directory is
gitignored except for curated files and directories prefixed `production_`.

All host-side suites use the resource-oriented v1 `SandboxClient` and attach a
`metadata` object containing schema version, workload parameters, run ID,
API/SDK versions, target, and deployed release. For fleet runs, set:

```bash
export BENCH_RUN_ID=production-9b6a9fc-<date>
export SANDBOX_RELEASE=9b6a9fc
```

For the release-wide matrix, use `scripts/bench-extensive.sh`. It runs direct
source latency, snapshot batch scaling, gateway fleet workloads, and host
memory density sequentially. Set `SINGLE_HOST_COUNT` below the worker's free
slot count when a production warm pool is enabled.

Historic JSON fields such as `restore`, `fanout`, `perCloneMs`, and `killMs`
remain as deprecated aliases so dashboards can compare new runs with old ones.
The `bench:restore` and `bench:fanout` npm commands have been removed; use
`bench:snapshot-source` and `bench:snapshot-batch`.

The snapshot batch suite clamps API `maxParallelism` to 32, the v1 contract
maximum. Operation completion and command readiness are reported separately.
Each returned sandbox is polled until `echo benchmark-ready` succeeds (30 s
default, 250 ms interval), and the JSON retains per-item attempts, latency, and
operation/readiness errors. Override with `--readiness-timeout-ms` and
`--readiness-poll-ms`. Any unusable sandbox, partial operation, or unproven
cleanup makes the process exit nonzero after it writes the diagnostic result.

The fleet suite validates all workload and concurrency arguments before it
allocates capacity. It uses an explicit cleanup request timeout, retries each
termination, and verifies each created resource returns `404` before declaring
cleanup complete. Partial create/workload results, transient cleanup errors,
host observations, and the final cleanup verdict are persisted even when the
command exits nonzero. Tune the per-request bound with
`--cleanup-timeout-ms` (10 s default).

The snapshot working-set suite is the guard against a deceptively clean
snapshot benchmark. Its source process fills random anonymous memory and
touches every 4 KiB page continuously; its disk state contains an incompressible
large file, 5,000 small files, and SQLite WAL data. Every clone must preserve
that live process, complete another full memory sweep, hash the entire large
file, enumerate the small files, and pass `PRAGMA integrity_check`. Counts run
ascending in odd rounds and descending in even rounds so cache warming does not
always benefit the largest fan-out. Use `--memory-mib`, `--disk-mib`,
`--small-files`, and `--sqlite-mib` to scale the working set.

## Implementation notes

- `benchmark.ts` is **erasable-syntax-only** TypeScript (no enums, namespaces, or
  parameter properties) so Node's type-stripping runs it without a compile step.
- It's a single file that re-spawns *itself* as the reader worker (guarded by
  `isMainThread`), so there's nothing else to copy into the guest.
- In `fsync` mode the DB uses a DELETE journal, so the reader workers race to
  flip it to WAL — they set `PRAGMA busy_timeout` first to serialize on the lock
  (mirroring Python's default 5 s connect timeout).
