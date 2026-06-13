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
```

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
gitignored.

## Implementation notes

- `benchmark.ts` is **erasable-syntax-only** TypeScript (no enums, namespaces, or
  parameter properties) so Node's type-stripping runs it without a compile step.
- It's a single file that re-spawns *itself* as the reader worker (guarded by
  `isMainThread`), so there's nothing else to copy into the guest.
- In `fsync` mode the DB uses a DELETE journal, so the reader workers race to
  flip it to WAL — they set `PRAGMA busy_timeout` first to serialize on the lock
  (mirroring Python's default 5 s connect timeout).
