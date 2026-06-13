/**
 * websandbox SQLite + filesystem benchmark — runs *inside* the guest.
 *
 * A TypeScript reimagining of tensorlakeai/sandbox-sqlite-bench's `benchmark.py`,
 * built for what websandbox sandboxes actually ship: Node 22 with the built-in
 * `node:sqlite` module (SQLite 3.51.x) and `node:worker_threads`. Zero npm
 * dependencies, no Python, no native addons — Node runs this `.ts` file directly
 * via type-stripping (`node --no-warnings benchmark.ts ...`).
 *
 * It keeps the upstream suite's 11 SQLite operations and three modes
 * (default / fsync / large) so the shape stays familiar, then adds our own
 * dimension the SQLite-only suite lacks: real filesystem I/O (many small files
 * and a large blob, each fsync'd), which is the thing that actually
 * distinguishes a sandbox's per-VM disk.
 *
 * Concurrency is real: the concurrent-read test spawns N `worker_threads`, each
 * with its own `DatabaseSync` connection, so it exercises multiple cores the
 * same way the Python version's threads do (SQLite releases the lock during
 * query execution).
 *
 * NOTE: numbers are NOT 1:1 comparable to the Python suite — different language
 * binding and a newer bundled SQLite (3.51.x vs 3.45.x). Comparisons are most
 * meaningful websandbox-vs-websandbox across configs.
 *
 * Usage (inside the guest):
 *   node --no-warnings benchmark.ts [--mode default|fsync|large] [--iterations N]
 *
 * Must stay erasable-syntax-only TypeScript (no enums / namespaces / parameter
 * properties) so Node's type-stripping can run it without a compile step.
 */
import { DatabaseSync } from 'node:sqlite'
import { Worker, isMainThread, parentPort, workerData } from 'node:worker_threads'
import { fileURLToPath } from 'node:url'
import {
  mkdirSync,
  rmSync,
  existsSync,
  statSync,
  openSync,
  writeSync,
  fsyncSync,
  closeSync,
  readFileSync,
} from 'node:fs'
import { join } from 'node:path'

const DB_PATH = '/tmp/bench.db'
const FS_DIR = '/tmp/benchfs'
const ALPHANUM = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'

/** A row from the concurrent-read worker; also the worker's input. */
interface ReaderJob {
  threadId: number
  queries: number
  dbPath: string
}

/** Mode configuration — scale factors mirroring the upstream `MODES`, plus our FS knobs. */
interface ModeConfig {
  label: string
  journalMode: string
  synchronous: string
  cacheSizeKb: number
  seqInserts: number
  batchInserts: number
  rangeQueries: number
  likeQueries: number
  updates: number
  deletes: number
  txInserts: number
  /** concurrent-read worker count + queries each (the multi-core test). */
  readThreads: number
  queriesPerThread: number
  /** our filesystem dimension: how many ~4 KB files, and the large-blob size. */
  fsSmallFiles: number
  fsLargeMb: number
}

const MODES: Record<string, ModeConfig> = {
  default: {
    label: 'Default (WAL, small dataset)',
    journalMode: 'WAL',
    synchronous: 'NORMAL',
    cacheSizeKb: 64000,
    seqInserts: 10000,
    batchInserts: 50000,
    rangeQueries: 1000,
    likeQueries: 500,
    updates: 5000,
    deletes: 2000,
    txInserts: 5000,
    readThreads: 4,
    queriesPerThread: 500,
    fsSmallFiles: 1000,
    fsLargeMb: 64,
  },
  fsync: {
    label: 'Fsync stress (synchronous=FULL, no WAL)',
    journalMode: 'DELETE',
    synchronous: 'FULL',
    cacheSizeKb: 64000,
    seqInserts: 5000,
    batchInserts: 20000,
    rangeQueries: 500,
    likeQueries: 200,
    updates: 2000,
    deletes: 1000,
    txInserts: 5000,
    readThreads: 4,
    queriesPerThread: 500,
    fsSmallFiles: 500,
    fsLargeMb: 32,
  },
  large: {
    label: 'Large dataset (WAL, exceeds cache)',
    journalMode: 'WAL',
    synchronous: 'NORMAL',
    cacheSizeKb: 8000, // 8MB cache to force spills
    seqInserts: 50000,
    batchInserts: 200000,
    rangeQueries: 2000,
    likeQueries: 1000,
    updates: 10000,
    deletes: 5000,
    txInserts: 10000,
    readThreads: 4,
    queriesPerThread: 500,
    fsSmallFiles: 2000,
    fsLargeMb: 128,
  },
}

/**
 * Small deterministic PRNG (mulberry32). The upstream uses `random.seed(42)`;
 * we can't reproduce CPython's Mersenne Twister stream, but this gives the same
 * property that matters: identical data across runs of this script.
 */
function mulberry32(seed: number): () => number {
  let a = seed >>> 0
  return function () {
    a = (a + 0x6d2b79f5) | 0
    let t = Math.imul(a ^ (a >>> 15), 1 | a)
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}

function randomString(rng: () => number, length = 50): string {
  let s = ''
  for (let i = 0; i < length; i++) s += ALPHANUM[Math.floor(rng() * ALPHANUM.length)]
  return s
}

const now = (): number => Number(process.hrtime.bigint()) / 1e9
const round4 = (x: number): number => Math.round(x * 1e4) / 1e4

// ---------------------------------------------------------------------------
// Worker entry: when not the main thread, run the concurrent-read loop and exit.
// ---------------------------------------------------------------------------

if (!isMainThread) {
  const job = workerData as ReaderJob
  const db = new DatabaseSync(job.dbPath)
  // busy_timeout MUST come first: in fsync mode the db is in DELETE journal mode,
  // so the workers race to flip it to WAL and need a grace period on the lock
  // (mirrors Python sqlite3.connect's default 5s timeout).
  db.exec('PRAGMA busy_timeout=5000')
  db.exec('PRAGMA journal_mode=WAL')
  const stmt = db.prepare('SELECT * FROM bench WHERE value BETWEEN ? AND ? LIMIT 100')
  const rng = mulberry32(job.threadId + 1)
  const t0 = now()
  for (let i = 0; i < job.queries; i++) {
    const low = rng() * 500
    stmt.all(low, low + 100)
  }
  const elapsed = now() - t0
  db.close()
  parentPort!.postMessage({ elapsed })
}

// ---------------------------------------------------------------------------
// SQLite operations — each returns elapsed seconds (main thread only).
// ---------------------------------------------------------------------------

function createSchema(db: DatabaseSync): void {
  db.exec('DROP TABLE IF EXISTS bench')
  db.exec('DROP TABLE IF EXISTS bench2')
  db.exec(`
    CREATE TABLE bench (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      name TEXT NOT NULL,
      value REAL,
      data TEXT,
      created_at TEXT DEFAULT CURRENT_TIMESTAMP
    )
  `)
  db.exec('CREATE INDEX idx_name ON bench(name)')
  db.exec('CREATE INDEX idx_value ON bench(value)')
}

function benchSequentialInserts(db: DatabaseSync, rng: () => number, n: number): number {
  const stmt = db.prepare('INSERT INTO bench (name, value, data) VALUES (?, ?, ?)')
  const start = now()
  db.exec('BEGIN')
  for (let i = 0; i < n; i++) stmt.run(`item_${i}`, rng() * 1000, randomString(rng))
  db.exec('COMMIT')
  return now() - start
}

function benchBatchInserts(db: DatabaseSync, rng: () => number, n: number): number {
  const stmt = db.prepare('INSERT INTO bench (name, value, data) VALUES (?, ?, ?)')
  const start = now()
  const data: Array<[string, number, string]> = []
  for (let i = 0; i < n; i++) data.push([`batch_${i}`, rng() * 1000, randomString(rng)])
  db.exec('BEGIN')
  for (const [name, value, blob] of data) stmt.run(name, value, blob)
  db.exec('COMMIT')
  return now() - start
}

function benchSelectCount(db: DatabaseSync): { elapsed: number; count: number } {
  const stmt = db.prepare('SELECT COUNT(*) AS c FROM bench')
  const start = now()
  const row = stmt.get() as { c: number }
  return { elapsed: now() - start, count: Number(row.c) }
}

function benchSelectRange(db: DatabaseSync, rng: () => number, iterations: number): number {
  const stmt = db.prepare('SELECT * FROM bench WHERE value BETWEEN ? AND ? LIMIT 100')
  const start = now()
  for (let i = 0; i < iterations; i++) {
    const low = rng() * 500
    stmt.all(low, low + 100)
  }
  return now() - start
}

function benchSelectLike(db: DatabaseSync, iterations: number): number {
  const stmt = db.prepare('SELECT * FROM bench WHERE name LIKE ? LIMIT 50')
  const start = now()
  for (let i = 0; i < iterations; i++) stmt.all(`item_${i}%`)
  return now() - start
}

function benchUpdate(db: DatabaseSync, rng: () => number, n: number): number {
  const stmt = db.prepare('UPDATE bench SET value = ? WHERE name = ?')
  const start = now()
  db.exec('BEGIN')
  for (let i = 0; i < n; i++) stmt.run(rng() * 1000, `item_${i}`)
  db.exec('COMMIT')
  return now() - start
}

function benchDelete(db: DatabaseSync, n: number): number {
  const stmt = db.prepare('DELETE FROM bench WHERE name = ?')
  const start = now()
  db.exec('BEGIN')
  for (let i = 0; i < n; i++) stmt.run(`batch_${i}`)
  db.exec('COMMIT')
  return now() - start
}

function benchTransaction(db: DatabaseSync, rng: () => number, n: number): number {
  const stmt = db.prepare('INSERT INTO bench (name, value, data) VALUES (?, ?, ?)')
  const start = now()
  db.exec('BEGIN')
  for (let i = 0; i < n; i++) stmt.run(`tx_${i}`, rng() * 1000, randomString(rng, 30))
  db.exec('COMMIT')
  return now() - start
}

function benchAggregate(db: DatabaseSync): number {
  const agg = db.prepare('SELECT AVG(value), MIN(value), MAX(value), SUM(value) FROM bench')
  const grp = db.prepare('SELECT name, COUNT(*) FROM bench GROUP BY substr(name, 1, 4)')
  const start = now()
  agg.get()
  grp.all()
  return now() - start
}

function benchJoin(db: DatabaseSync, rng: () => number): number {
  db.exec('DROP TABLE IF EXISTS bench2')
  db.exec('CREATE TABLE bench2 (id INTEGER PRIMARY KEY, bench_id INTEGER, extra TEXT)')
  const ins = db.prepare('INSERT INTO bench2 (bench_id, extra) VALUES (?, ?)')
  db.exec('BEGIN')
  for (let i = 0; i < 10000; i++) ins.run(Math.floor(rng() * 60000) + 1, randomString(rng, 20))
  db.exec('COMMIT')
  const join = db.prepare(`
    SELECT b.name, b.value, b2.extra
    FROM bench b
    JOIN bench2 b2 ON b.id = b2.bench_id
    WHERE b.value > 500
    LIMIT 1000
  `)
  const start = now()
  join.all()
  return now() - start
}

/** Concurrent reads via worker_threads — the multi-core test. Returns wall seconds. */
async function benchConcurrentReads(
  dbPath: string,
  numThreads: number,
  queriesPerThread: number
): Promise<{ wall: number; totalQueries: number }> {
  const self = fileURLToPath(import.meta.url)
  const wallStart = now()
  const workers = Array.from({ length: numThreads }, (_unused, threadId) => {
    const job: ReaderJob = { threadId, queries: queriesPerThread, dbPath }
    return new Promise<void>((resolve, reject) => {
      const w = new Worker(self, { workerData: job })
      w.on('message', () => resolve())
      w.on('error', reject)
      w.on('exit', (code) => {
        if (code !== 0) reject(new Error(`reader worker exited with code ${code}`))
      })
    })
  })
  await Promise.all(workers)
  return { wall: now() - wallStart, totalQueries: numThreads * queriesPerThread }
}

// ---------------------------------------------------------------------------
// Filesystem operations — our own dimension (real disk, fsync'd).
// ---------------------------------------------------------------------------

/** Write `n` ~4 KB files, fsync'ing each so we measure the guest's real disk. */
function benchFsWriteMany(rng: () => number, n: number): number {
  rmSync(FS_DIR, { recursive: true, force: true })
  mkdirSync(FS_DIR, { recursive: true })
  const payload = Buffer.from(randomString(rng, 4096))
  const start = now()
  for (let i = 0; i < n; i++) {
    const fd = openSync(join(FS_DIR, `f_${i}.dat`), 'w')
    writeSync(fd, payload)
    fsyncSync(fd)
    closeSync(fd)
  }
  return now() - start
}

function benchFsReadMany(n: number): number {
  const start = now()
  let bytes = 0
  for (let i = 0; i < n; i++) bytes += readFileSync(join(FS_DIR, `f_${i}.dat`)).length
  if (bytes < 0) throw new Error('unreachable')
  return now() - start
}

/** Write one large blob in 1 MB chunks then a single fsync. */
function benchFsLargeWrite(rng: () => number, mb: number): number {
  const chunk = Buffer.from(randomString(rng, 1024 * 1024))
  const path = join(FS_DIR, 'large.bin')
  const start = now()
  const fd = openSync(path, 'w')
  for (let i = 0; i < mb; i++) writeSync(fd, chunk)
  fsyncSync(fd)
  closeSync(fd)
  return now() - start
}

function benchFsLargeRead(mb: number): number {
  const path = join(FS_DIR, 'large.bin')
  const start = now()
  const buf = readFileSync(path)
  if (buf.length !== mb * 1024 * 1024) throw new Error(`large blob size mismatch: ${buf.length}`)
  return now() - start
}

function getDbSizeMb(): number {
  let size = 0
  if (existsSync(DB_PATH)) size += statSync(DB_PATH).size
  const wal = DB_PATH + '-wal'
  if (existsSync(wal)) size += statSync(wal).size
  return size / (1024 * 1024)
}

// ---------------------------------------------------------------------------
// One full pass.
// ---------------------------------------------------------------------------

function cleanupDb(): void {
  for (const suffix of ['', '-wal', '-shm']) {
    const p = DB_PATH + suffix
    if (existsSync(p)) rmSync(p, { force: true })
  }
}

type Results = Record<string, number>

/** Keys that are metadata, not timed operations — excluded from total_time. */
const NOT_TIMED = new Set([
  'row_count',
  'db_size_mb',
  'concurrent_reads_total_queries',
  'fs_small_files',
  'fs_large_file_mb',
])

async function runSingle(cfg: ModeConfig): Promise<Results> {
  const rng = mulberry32(42)
  cleanupDb()

  const db = new DatabaseSync(DB_PATH)
  db.exec(`PRAGMA journal_mode=${cfg.journalMode}`)
  db.exec(`PRAGMA synchronous=${cfg.synchronous}`)
  db.exec(`PRAGMA cache_size=-${cfg.cacheSizeKb}`)

  const r: Results = {}
  createSchema(db)

  r.sequential_inserts = round4(benchSequentialInserts(db, rng, cfg.seqInserts))
  r.batch_inserts = round4(benchBatchInserts(db, rng, cfg.batchInserts))

  const count = benchSelectCount(db)
  r.select_count = round4(count.elapsed)
  r.row_count = count.count

  r.range_queries = round4(benchSelectRange(db, rng, cfg.rangeQueries))
  r.like_queries = round4(benchSelectLike(db, cfg.likeQueries))
  r.updates = round4(benchUpdate(db, rng, cfg.updates))
  r.deletes = round4(benchDelete(db, cfg.deletes))
  r.transaction_inserts = round4(benchTransaction(db, rng, cfg.txInserts))
  r.aggregates = round4(benchAggregate(db))
  r.join_query = round4(benchJoin(db, rng))

  db.close()

  const conc = await benchConcurrentReads(DB_PATH, cfg.readThreads, cfg.queriesPerThread)
  r.concurrent_reads_wall = round4(conc.wall)
  r.concurrent_reads_total_queries = conc.totalQueries

  r.db_size_mb = Math.round(getDbSizeMb() * 100) / 100

  // Our own filesystem dimension.
  r.fs_write_many = round4(benchFsWriteMany(rng, cfg.fsSmallFiles))
  r.fs_read_many = round4(benchFsReadMany(cfg.fsSmallFiles))
  r.fs_large_write = round4(benchFsLargeWrite(rng, cfg.fsLargeMb))
  r.fs_large_read = round4(benchFsLargeRead(cfg.fsLargeMb))
  r.fs_small_files = cfg.fsSmallFiles
  r.fs_large_file_mb = cfg.fsLargeMb

  let total = 0
  for (const [k, v] of Object.entries(r)) {
    if (k !== 'total_time' && !NOT_TIMED.has(k)) total += v
  }
  r.total_time = round4(total)

  cleanupDb()
  rmSync(FS_DIR, { recursive: true, force: true })
  return r
}

// ---------------------------------------------------------------------------
// CLI / summary (main thread only).
// ---------------------------------------------------------------------------

interface Stat {
  mean: number
  stddev: number
  min: number
  max: number
}

function stat(values: number[]): Stat {
  const mean = values.reduce((s, v) => s + v, 0) / values.length
  const variance =
    values.length > 1
      ? values.reduce((s, v) => s + (v - mean) ** 2, 0) / (values.length - 1)
      : 0
  return {
    mean: round4(mean),
    stddev: round4(Math.sqrt(variance)),
    min: round4(Math.min(...values)),
    max: round4(Math.max(...values)),
  }
}

function parseArgs(argv: string[]): { mode: string; iterations: number } {
  let mode = 'default'
  let iterations = 1
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--mode') mode = argv[++i] ?? mode
    else if (argv[i] === '--iterations') iterations = Number(argv[++i])
  }
  if (!(mode in MODES)) throw new Error(`--mode must be one of ${Object.keys(MODES).join('|')}`)
  if (!Number.isInteger(iterations) || iterations < 1) throw new Error('--iterations must be >= 1')
  return { mode, iterations }
}

const ORDERED_KEYS = [
  'sequential_inserts',
  'batch_inserts',
  'select_count',
  'range_queries',
  'like_queries',
  'updates',
  'deletes',
  'transaction_inserts',
  'aggregates',
  'join_query',
  'concurrent_reads_wall',
  'fs_write_many',
  'fs_read_many',
  'fs_large_write',
  'fs_large_read',
  'total_time',
]

async function main(): Promise<void> {
  const { mode, iterations } = parseArgs(process.argv.slice(2))
  const cfg = MODES[mode]!
  const sqliteVersion = (() => {
    const db = new DatabaseSync(':memory:')
    const v = (db.prepare('SELECT sqlite_version() AS v').get() as { v: string }).v
    db.close()
    return v
  })()

  console.log(`Engine: node:sqlite (SQLite ${sqliteVersion})`)
  console.log(`Node version: ${process.version}`)
  console.log(`Mode: ${cfg.label}`)
  console.log(`Iterations: ${iterations}`)
  console.log(
    `Journal: ${cfg.journalMode}, Sync: ${cfg.synchronous}, Cache: ${cfg.cacheSizeKb}KB, ` +
      `Readers: ${cfg.readThreads}x${cfg.queriesPerThread}, FS: ${cfg.fsSmallFiles} files + ${cfg.fsLargeMb}MB blob`
  )
  console.log('-'.repeat(60))

  const allRuns: Results[] = []
  for (let i = 0; i < iterations; i++) {
    if (iterations > 1) console.log(`\n--- Iteration ${i + 1}/${iterations} ---`)
    const run = await runSingle(cfg)
    allRuns.push(run)
    for (const k of ORDERED_KEYS) {
      console.log(`  ${k.padEnd(24)} ${run[k]!.toFixed(4)}s`)
    }
    const qps = run.concurrent_reads_total_queries! / run.concurrent_reads_wall!
    console.log(`  concurrent_reads q/s     ${qps.toFixed(0)} q/s`)
    console.log(`  db_size_mb               ${run.db_size_mb!.toFixed(2)} MB`)
  }

  let summary: Record<string, unknown>
  if (iterations === 1) {
    summary = { ...allRuns[0] }
  } else {
    summary = { iterations }
    const statKeys = Object.keys(allRuns[0]!).filter(
      (k) => k !== 'row_count' && k !== 'concurrent_reads_total_queries'
    )
    for (const k of statKeys) summary[k] = stat(allRuns.map((run) => run[k]!))
    summary.row_count = allRuns[0]!.row_count
    summary.all_runs = allRuns
    const tt = summary.total_time as Stat
    console.log(`\n${'='.repeat(60)}`)
    console.log(`  SUMMARY (${iterations} iterations)`)
    console.log('='.repeat(60))
    console.log(`  Total: ${tt.mean.toFixed(4)}s +/- ${tt.stddev.toFixed(4)}s`)
  }

  summary.mode = mode
  summary.mode_label = cfg.label
  summary.sqlite_version = sqliteVersion
  summary.node_version = process.version
  summary.engine = 'node:sqlite'

  console.log('\n--- JSON ---')
  console.log(JSON.stringify(summary, null, 2))
}

if (isMainThread) {
  main().catch((err) => {
    console.error(err instanceof Error ? (err.stack ?? err.message) : err)
    process.exit(1)
  })
}
