/**
 * Snapshot fan-out benchmark with a deliberately large active working set.
 *
 * Unlike a readiness-only benchmark, this reports two boundaries:
 *   - command ready: every clone can execute a control command
 *   - hydrated: every clone completed another full anonymous-memory sweep,
 *     hashed the large file, enumerated small files, and checked SQLite
 *
 * Usage:
 *   npm run bench:snapshot-working-set -- \
 *     --counts 1,4,8,16,24,32 --rounds 2 --max-parallelism 32
 */
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import {
  SandboxClient,
  type ClientSandbox,
  type ProblemDetails,
} from '../src/index.js'
import { benchmarkMetadata, benchmarkResourceMetadata } from './metadata.js'
import {
  cleanupRunSandboxes,
  deleteSnapshotWithRetry,
  terminateTracked,
} from './snapshot-batch-bench.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const RESULTS_DIR = join(HERE, 'results')
const GUEST_SOURCE = join(HERE, 'snapshot-working-set-guest.ts')
const GUEST_PATH = '/tmp/snapshot-working-set-guest.ts'
const ROOT = '/tmp/snapshot-working-set'
const PID_PATH = '/tmp/snapshot-working-set.pid'
const LOG_PATH = '/tmp/snapshot-working-set.log'

export interface Args {
  counts: number[]
  rounds: number
  maxParallelism: number
  guestMemoryMiB?: number
  memoryMiB: number
  diskMiB: number
  smallFiles: number
  sqliteMiB: number
  readinessTimeoutMs: number
  hydrationTimeoutMs: number
  settleMs: number
  output?: string
}

function integer(raw: string | undefined, flag: string, min: number, max = Number.MAX_SAFE_INTEGER): number {
  const value = Number(raw)
  if (!Number.isInteger(value) || value < min || value > max) {
    throw new Error(`${flag} must be an integer from ${min} to ${max}`)
  }
  return value
}

export function parseArgs(argv: string[]): Args {
  const args: Args = {
    counts: [1, 4, 8, 16, 24, 32],
    rounds: 2,
    maxParallelism: 32,
    memoryMiB: 256,
    diskMiB: 384,
    smallFiles: 5_000,
    sqliteMiB: 32,
    readinessTimeoutMs: 30_000,
    hydrationTimeoutMs: 180_000,
    settleMs: 1_000,
  }
  for (let index = 0; index < argv.length; index++) {
    const flag = argv[index]
    if (flag === '--counts') {
      args.counts = argv[++index]!.split(',').map((value) => integer(value.trim(), '--counts', 1, 100))
    } else if (flag === '--rounds') args.rounds = integer(argv[++index], flag, 1)
    else if (flag === '--max-parallelism') args.maxParallelism = integer(argv[++index], flag, 1, 32)
    else if (flag === '--guest-memory-mib') args.guestMemoryMiB = integer(argv[++index], flag, 128)
    else if (flag === '--memory-mib') args.memoryMiB = integer(argv[++index], flag, 1)
    else if (flag === '--disk-mib') args.diskMiB = integer(argv[++index], flag, 1)
    else if (flag === '--small-files') args.smallFiles = integer(argv[++index], flag, 1)
    else if (flag === '--sqlite-mib') args.sqliteMiB = integer(argv[++index], flag, 1)
    else if (flag === '--readiness-timeout-ms') args.readinessTimeoutMs = integer(argv[++index], flag, 1)
    else if (flag === '--hydration-timeout-ms') args.hydrationTimeoutMs = integer(argv[++index], flag, 1)
    else if (flag === '--settle-ms') args.settleMs = integer(argv[++index], flag, 0)
    else if (flag === '--output') args.output = argv[++index]
    else if (flag === '--help' || flag === '-h') {
      console.log(
        'Usage: tsx benchmarks/snapshot-working-set-bench.ts ' +
        '[--counts 1,4,8,16,24,32] [--rounds 2] [--max-parallelism 32] ' +
        '[--guest-memory-mib 2048] ' +
        '[--memory-mib 256] [--disk-mib 384] [--small-files 5000] ' +
        '[--sqlite-mib 32] [--readiness-timeout-ms 30000] ' +
        '[--hydration-timeout-ms 180000] [--settle-ms 1000] [--output file.json]',
      )
      process.exit(0)
    } else throw new Error(`unknown argument: ${flag}`)
  }
  if (args.counts.length === 0) throw new Error('--counts must not be empty')
  return args
}

/** Alternating the order prevents a warm snapshot cache from always favoring high N. */
export function countOrder(counts: number[], round: number): number[] {
  return round % 2 === 1 ? [...counts] : [...counts].reverse()
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`
}

function errorMessage(error: unknown): string {
  return String((error as Error)?.message ?? error).slice(0, 500)
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))
const fmt = (value: number | undefined) => value === undefined ? 'n/a' : `${value.toFixed(1)}ms`

async function mapLimit<T, R>(items: T[], limit: number, fn: (item: T, index: number) => Promise<R>): Promise<R[]> {
  const results = new Array<R>(items.length)
  let next = 0
  const workers = Array.from({ length: Math.min(limit, items.length) }, async () => {
    while (true) {
      const index = next++
      if (index >= items.length) return
      results[index] = await fn(items[index]!, index)
    }
  })
  await Promise.all(workers)
  return results
}

function percentile(values: number[], fraction: number): number | undefined {
  if (values.length === 0) return undefined
  const ordered = [...values].sort((a, b) => a - b)
  return ordered[Math.min(ordered.length - 1, Math.ceil(ordered.length * fraction) - 1)]
}

interface Verification {
  runId: string
  memoryMiB: number
  diskMiB: number
  smallFiles: number
  sqliteRows: number
  memoryCycleBefore: number
  memoryCycleAfter: number
}

interface ItemResult {
  index: number
  sandboxId?: string
  operationError?: ProblemDetails
  commandReadyMs?: number
  hydrationMs?: number
  verificationMs?: number
  verification?: Verification
  error?: string
  ok: boolean
}

interface BatchRow {
  round: number
  count: number
  maxParallelism: number
  operationId: string
  operationStatus: string
  operationWallMs: number
  commandReadyMakespanMs?: number
  commandReadyP50Ms?: number
  commandReadyP95Ms?: number
  hydratedMakespanMs?: number
  hydratedP50Ms?: number
  hydratedP95Ms?: number
  ok: number
  failed: number
  operationError?: string
  cleanupErrors: string[]
  items: ItemResult[]
}

interface SourceStats {
  createMs?: number
  setupMs?: number
  snapshotMs?: number
  guest?: { diskUsedMiB: number; memoryAvailableKiB: number; heartbeat: unknown }
}

async function prepareSource(source: ClientSandbox, args: Args, runId: string): Promise<{ setupMs: number; guest: SourceStats['guest'] }> {
  await source.files.write(GUEST_PATH, readFileSync(GUEST_SOURCE, 'utf8'))
  const setupStarted = Date.now()
  const holderArgs = [args.memoryMiB, args.diskMiB, args.smallFiles, args.sqliteMiB, runId]
    .map((value) => shellQuote(String(value)))
    .join(' ')
  await source.commands.run(
    `nohup node --no-warnings ${GUEST_PATH} hold ${holderArgs} >${LOG_PATH} 2>&1 & ` +
    `for i in $(seq 1 480); do test -f ${ROOT}/ready && exit 0; sleep .25; done; ` +
    `cat ${LOG_PATH}; exit 1`,
    { timeoutMs: 150_000 },
  )
  const setupMs = Date.now() - setupStarted
  const stats = await source.commands.run(
    `du -sm ${ROOT} | cut -f1; awk '/MemAvailable/ {print $2}' /proc/meminfo; cat ${ROOT}/heartbeat.json`,
    { timeoutMs: 15_000 },
  )
  const lines = stats.stdout.trim().split('\n')
  return {
    setupMs,
    guest: {
      diskUsedMiB: Number(lines[0]),
      memoryAvailableKiB: Number(lines[1]),
      heartbeat: JSON.parse(lines[2] ?? '{}') as unknown,
    },
  }
}

async function runBatch(
  client: SandboxClient,
  snapshotId: string,
  count: number,
  round: number,
  args: Args,
  runId: string,
  resourceMetadata: Record<string, string>,
  tracked: Map<string, ClientSandbox>,
): Promise<BatchRow> {
  const started = Date.now()
  const maxParallelism = Math.min(count, args.maxParallelism)
  const operation = await client.sandboxes.createMany({
    count,
    maxParallelism,
    source: { snapshotId },
    metadata: resourceMetadata,
    requestTimeoutMs: 30 * 60_000,
  })
  let state = operation.state
  let operationError: string | undefined
  try {
    state = await operation.wait({ timeoutMs: 30 * 60_000 })
  } catch (error) {
    operationError = errorMessage(error)
    await operation.refresh().catch(() => {})
    state = operation.state
  }
  const operationWallMs = Date.now() - started
  for (const result of state.results) {
    if (result.value) tracked.set(result.value.id, result.value)
  }

  const items = await mapLimit(state.results, 32, async (result, position): Promise<ItemResult> => {
    if (!result.value) {
      return {
        index: result.error ? result.index : position,
        ...(result.error ? { operationError: result.error } : { error: 'operation returned no sandbox or error' }),
        ok: false,
      }
    }
    const item: ItemResult = { index: result.index, sandboxId: result.value.id, ok: false }
    try {
      const ready = await result.value.commands.run(
        `test -f ${ROOT}/ready && kill -0 $(cat ${PID_PATH}) && printf working-set-ready`,
        { timeoutMs: args.readinessTimeoutMs },
      )
      if (ready.stdout.trim() !== 'working-set-ready') {
        throw new Error(`unexpected readiness output: ${JSON.stringify(ready.stdout.trim())}`)
      }
      item.commandReadyMs = Date.now() - started
    } catch (error) {
      item.error = errorMessage(error)
    }
    return item
  })

  await mapLimit(items, 32, async (item) => {
    if (item.error || !item.sandboxId) return
    const sandbox = tracked.get(item.sandboxId)
    if (!sandbox) {
      item.error = 'sandbox disappeared from the tracked set'
      return
    }
    const verificationStarted = Date.now()
    try {
      const result = await sandbox.commands.run(
        `node --no-warnings ${GUEST_PATH} verify ${shellQuote(runId)}`,
        { timeoutMs: args.hydrationTimeoutMs },
      )
      item.verification = JSON.parse(result.stdout.trim()) as Verification
      item.verificationMs = Date.now() - verificationStarted
      item.hydrationMs = Date.now() - started
      item.ok = true
    } catch (error) {
      item.error = errorMessage(error)
    }
  })

  const created = state.results.flatMap((result) => result.value ? [result.value] : [])
  const cleanupErrors = await terminateTracked(created)
  for (const sandbox of created) {
    if (!cleanupErrors.some((error) => error.startsWith(`${sandbox.id}:`))) tracked.delete(sandbox.id)
  }

  const ready = items.flatMap((item) => item.commandReadyMs === undefined ? [] : [item.commandReadyMs])
  const hydrated = items.flatMap((item) => item.hydrationMs === undefined ? [] : [item.hydrationMs])
  const ok = items.filter((item) => item.ok).length
  const row: BatchRow = {
    round,
    count,
    maxParallelism,
    operationId: operation.id,
    operationStatus: state.status,
    operationWallMs,
    commandReadyMakespanMs: ready.length ? Math.max(...ready) : undefined,
    commandReadyP50Ms: percentile(ready, 0.5),
    commandReadyP95Ms: percentile(ready, 0.95),
    hydratedMakespanMs: hydrated.length ? Math.max(...hydrated) : undefined,
    hydratedP50Ms: percentile(hydrated, 0.5),
    hydratedP95Ms: percentile(hydrated, 0.95),
    ok,
    failed: count - ok,
    ...(operationError === undefined ? {} : { operationError }),
    cleanupErrors,
    items,
  }
  console.log(
    `r${round} n=${String(count).padStart(2)} parallel=${String(maxParallelism).padStart(2)} ` +
    `operation=${fmt(operationWallMs)} command-ready=${fmt(row.commandReadyMakespanMs)} ` +
    `hydrated=${fmt(row.hydratedMakespanMs)} ok=${ok}/${count}`,
  )
  return row
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2))
  const client = new SandboxClient()
  const metadata = benchmarkMetadata('snapshot-working-set', {
    counts: args.counts,
    rounds: args.rounds,
    max_parallelism: args.maxParallelism,
    guest_memory_mib: args.guestMemoryMiB ?? 'template-default',
    memory_mib: args.memoryMiB,
    disk_mib: args.diskMiB,
    small_files: args.smallFiles,
    sqlite_mib: args.sqliteMiB,
    readiness_timeout_ms: args.readinessTimeoutMs,
    hydration_timeout_ms: args.hydrationTimeoutMs,
    settle_ms: args.settleMs,
    order: 'ascending-then-descending',
  })
  const resourceMetadata = benchmarkResourceMetadata(metadata)
  const tracked = new Map<string, ClientSandbox>()
  const rows: BatchRow[] = []
  const sourceStats: SourceStats = {}
  const cleanupErrors: string[] = []
  let source: ClientSandbox | undefined
  let snapshotId: string | undefined
  let failure: unknown

  const timestamp = new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '').replace('T', '_')
  const output = args.output ?? join(RESULTS_DIR, `snapshot_working_set_${timestamp}.json`)
  try {
    console.log(
      `Snapshot working-set benchmark on ${process.env.SANDBOX_API_URL}: ` +
      `${args.memoryMiB} MiB memory + ${args.diskMiB} MiB file + ${args.smallFiles} small files`,
    )
    const sourceStarted = Date.now()
    source = await client.sandboxes.create({
      metadata: resourceMetadata,
      requestTimeoutMs: 10 * 60_000,
      ...(args.guestMemoryMiB === undefined ? {} : { memMib: args.guestMemoryMiB }),
    })
    sourceStats.createMs = Date.now() - sourceStarted
    tracked.set(source.id, source)

    const prepared = await prepareSource(source, args, metadata.run_id)
    sourceStats.setupMs = prepared.setupMs
    sourceStats.guest = prepared.guest
    const snapshotStarted = Date.now()
    snapshotId = (await source.createSnapshot()).id
    sourceStats.snapshotMs = Date.now() - snapshotStarted

    const sourceCleanup = await terminateTracked([source])
    if (sourceCleanup.length) throw new Error(`source cleanup failed: ${sourceCleanup.join('; ')}`)
    tracked.delete(source.id)
    source = undefined
    await sleep(args.settleMs)

    console.log(
      `source=${fmt(sourceStats.createMs)} setup=${fmt(sourceStats.setupMs)} ` +
      `snapshot=${fmt(sourceStats.snapshotMs)} disk-used=${sourceStats.guest?.diskUsedMiB}MiB`,
    )
    for (let round = 1; round <= args.rounds; round++) {
      for (const count of countOrder(args.counts, round)) {
        rows.push(await runBatch(
          client,
          snapshotId,
          count,
          round,
          args,
          metadata.run_id,
          resourceMetadata,
          tracked,
        ))
        if (rows.at(-1)!.failed > 0 || rows.at(-1)!.cleanupErrors.length > 0) {
          throw new Error(`round ${round} count ${count} failed; inspect result items`)
        }
        await sleep(args.settleMs)
      }
    }
  } catch (error) {
    failure = error
  } finally {
    if (source) {
      const errors = await terminateTracked([source])
      cleanupErrors.push(...errors)
      if (errors.length === 0) tracked.delete(source.id)
    }
    cleanupErrors.push(...await cleanupRunSandboxes(client, tracked, resourceMetadata))
    if (snapshotId) {
      const error = await deleteSnapshotWithRetry(client, snapshotId)
      if (error) cleanupErrors.push(error)
    }

    const passed = failure === undefined && cleanupErrors.length === 0 &&
      rows.length === args.counts.length * args.rounds &&
      rows.every((row) => row.ok === row.count && row.cleanupErrors.length === 0)
    mkdirSync(dirname(output), { recursive: true })
    writeFileSync(output, JSON.stringify({
      metadata,
      passed,
      source: sourceStats,
      snapshotId,
      rows,
      cleanupErrors,
      ...(failure === undefined ? {} : { error: errorMessage(failure) }),
    }, null, 2))
    console.log(`saved ${output}`)
    if (failure !== undefined) throw failure
    if (cleanupErrors.length) throw new Error(`cleanup failed: ${cleanupErrors.join('; ')}`)
    if (!passed) throw new Error('snapshot working-set benchmark did not pass')
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(error instanceof Error ? (error.stack ?? error.message) : error)
    process.exit(1)
  })
}
