/**
 * Snapshot-source batch-create benchmark.
 *
 * Measures how fast the v1 batch operation creates N sandboxes from one
 * immutable snapshot source, versus N default-source creates.
 *
 * For each N it reports operation wall-clock and amortized per-sandbox time,
 * then — for the largest N — a default-source baseline
 * (N concurrent default-source creates) so the speedup is explicit.
 *
 * Usage:
 *   SANDBOX_API_URL=http://<host>:8080 SANDBOX_API_KEY=<key> \
 *     tsx benchmarks/snapshot-batch-bench.ts [--counts 1,4,16,32] [--baseline] [--output file.json]
 *
 * Point SANDBOX_API_URL at a single host's API: snapshots are host-local.
 */
import { fileURLToPath } from 'node:url'
import { dirname, join, resolve } from 'node:path'
import { mkdirSync, writeFileSync } from 'node:fs'
import {
  SandboxClient,
  type ClientSandbox,
  type ProblemDetails,
} from '../src/index.js'
import { benchmarkMetadata, benchmarkResourceMetadata } from './metadata.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const RESULTS_DIR = join(HERE, 'results')

interface Args {
  counts: number[]
  baseline: boolean
  baselineParallelism: number
  maxParallelism: number
  readinessTimeoutMs: number
  readinessPollMs: number
  output?: string
}

function parseArgs(argv: string[]): Args {
  const a: Args = {
    counts: [1, 4, 16, 32],
    baseline: false,
    baselineParallelism: 32,
    maxParallelism: 32,
    readinessTimeoutMs: 30_000,
    readinessPollMs: 250,
  }
  for (let i = 0; i < argv.length; i++) {
    const k = argv[i]
    if (k === '--counts') a.counts = argv[++i]!.split(',').map((x) => Number(x.trim()))
    else if (k === '--baseline') a.baseline = true
    else if (k === '--baseline-parallelism') a.baselineParallelism = Number(argv[++i])
    else if (k === '--max-parallelism') a.maxParallelism = Number(argv[++i])
    else if (k === '--readiness-timeout-ms') a.readinessTimeoutMs = Number(argv[++i])
    else if (k === '--readiness-poll-ms') a.readinessPollMs = Number(argv[++i])
    else if (k === '--output') a.output = argv[++i]
    else if (k === '--help' || k === '-h') {
      console.log(
        'Usage: tsx benchmarks/snapshot-batch-bench.ts [--counts 1,4,16,32] ' +
        '[--baseline] [--baseline-parallelism 32] [--max-parallelism 32] ' +
        '[--readiness-timeout-ms 30000] [--readiness-poll-ms 250] ' +
        '[--output file.json]',
      )
      process.exit(0)
    }
    else throw new Error(`unknown arg: ${k}`)
  }
  if (a.counts.some((n) => !Number.isInteger(n) || n < 1)) throw new Error('--counts must be positive integers')
  if (!Number.isInteger(a.baselineParallelism) || a.baselineParallelism < 1 || a.baselineParallelism > 32) {
    throw new Error('--baseline-parallelism must be an integer from 1 to 32')
  }
  if (!Number.isInteger(a.maxParallelism) || a.maxParallelism < 1 || a.maxParallelism > 32) {
    throw new Error('--max-parallelism must be an integer from 1 to 32')
  }
  if (!Number.isInteger(a.readinessTimeoutMs) || a.readinessTimeoutMs < 1) {
    throw new Error('--readiness-timeout-ms must be a positive integer')
  }
  if (!Number.isInteger(a.readinessPollMs) || a.readinessPollMs < 1) {
    throw new Error('--readiness-poll-ms must be a positive integer')
  }
  return a
}

async function mapLimit<T, R>(items: T[], limit: number, fn: (item: T, idx: number) => Promise<R>): Promise<R[]> {
  const results = new Array<R>(items.length)
  let next = 0
  const workers = Array.from({ length: Math.min(limit, items.length) }, async () => {
    while (true) {
      const i = next++
      if (i >= items.length) return
      results[i] = await fn(items[i]!, i)
    }
  })
  await Promise.all(workers)
  return results
}

const fmt = (x: number) => (Number.isFinite(x) ? x.toFixed(1) : 'n/a')
const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

interface ReadinessResult {
  usable: boolean
  readinessMs: number
  attempts: number
  error?: string
}

interface ItemResult extends ReadinessResult {
  index: number
  sandboxId?: string
  operationError?: ProblemDetails
}

interface BatchRow {
  n: number
  maxParallelism: number
  operationWallMs: number
  readinessWallMs: number
  wallMs: number
  perSandboxMs: number
  ok: number
  failed: number
  operationId: string
  operationStatus: string
  operationError?: string
  items: ItemResult[]
  cleanupErrors: string[]
  /** @deprecated Historic alias for perSandboxMs. */
  perCloneMs: number
}

async function waitUntilUsable(
  sandbox: ClientSandbox,
  timeoutMs: number,
  pollMs: number,
): Promise<ReadinessResult> {
  const started = Date.now()
  const deadline = started + timeoutMs
  let attempts = 0
  let lastError = 'sandbox did not become command-ready'

  while (Date.now() < deadline) {
    attempts++
    const remaining = Math.max(1, deadline - Date.now())
    try {
      const result = await sandbox.commands.run('echo benchmark-ready', {
        timeoutMs: Math.min(5_000, remaining),
      })
      if (result.stdout.trim() === 'benchmark-ready') {
        return { usable: true, readinessMs: Date.now() - started, attempts }
      }
      lastError = `unexpected readiness output: ${JSON.stringify(result.stdout.trim())}`
    } catch (error) {
      lastError = errorMessage(error)
    }
    const remainingAfterAttempt = deadline - Date.now()
    if (remainingAfterAttempt > 0) await sleep(Math.min(pollMs, remainingAfterAttempt))
  }

  return {
    usable: false,
    readinessMs: Date.now() - started,
    attempts,
    error: lastError,
  }
}

export async function terminateTracked(
  sandboxes: Iterable<ClientSandbox>,
): Promise<string[]> {
  const unique = [...new Map([...sandboxes].map((sandbox) => [sandbox.id, sandbox])).values()]
  const errors = await mapLimit(unique, 16, async (sandbox) => {
    let lastError = ''
    for (let attempt = 1; attempt <= 3; attempt++) {
      try {
        await sandbox.terminate()
        return undefined
      } catch (error) {
        const status = (error as { status?: unknown })?.status
        if (status === 404) return undefined
        lastError = errorMessage(error)
        if (attempt < 3) await sleep(250 * attempt)
      }
    }
    return `${sandbox.id}: ${lastError}`
  })
  return errors.filter((error): error is string => error !== undefined)
}

function errorMessage(error: unknown): string {
  return String((error as Error)?.message ?? error).slice(0, 500)
}

interface SandboxDiscoveryClient {
  sandboxes: {
    list(options: { metadata: Record<string, string> }): AsyncIterable<ClientSandbox>
  }
}

interface CleanupOptions {
  attempts?: number
  delayMs?: number
}

/**
 * Discover and terminate resources attributed to exactly this benchmark run.
 *
 * Operation polling can fail before its result list reaches the client, so the
 * tracked map alone is insufficient. Server-side metadata filters are backed by
 * an exact client-side check before termination as a defense against a broken or
 * unexpectedly broad filter implementation.
 */
export async function cleanupRunSandboxes(
  client: SandboxDiscoveryClient,
  tracked: Map<string, ClientSandbox>,
  resourceMetadata: Record<string, string>,
  options: CleanupOptions = {},
): Promise<string[]> {
  const attempts = options.attempts ?? 10
  const delayMs = options.delayMs ?? 1_000
  if (!resourceMetadata.benchmark_run_id) {
    return ['refusing metadata cleanup without benchmark_run_id']
  }

  let lastDiscoveryError = ''
  let lastTerminationErrors: string[] = []
  let emptySweeps = 0
  for (let attempt = 1; attempt <= attempts; attempt++) {
    let discoverySucceeded = false
    try {
      for await (const sandbox of client.sandboxes.list({ metadata: resourceMetadata })) {
        const exactMatch = Object.entries(resourceMetadata)
          .every(([key, value]) => sandbox.metadata[key] === value)
        if (exactMatch) tracked.set(sandbox.id, sandbox)
      }
      discoverySucceeded = true
      lastDiscoveryError = ''
    } catch (error) {
      lastDiscoveryError = `metadata discovery attempt ${attempt}: ${errorMessage(error)}`
    }

    if (discoverySucceeded) {
      lastTerminationErrors = await terminateTracked(tracked.values())
      for (const id of [...tracked.keys()]) {
        if (!lastTerminationErrors.some((error) => error.startsWith(`${id}:`))) {
          tracked.delete(id)
        }
      }
      if (tracked.size === 0 && lastTerminationErrors.length === 0) {
        emptySweeps++
        // A second sweep catches resources published just after an operation
        // polling failure without turning cleanup into an unbounded wait.
        if (emptySweeps >= 2) return []
      } else {
        emptySweeps = 0
      }
    } else {
      emptySweeps = 0
    }

    if (attempt < attempts) await sleep(delayMs)
  }

  return [
    ...lastTerminationErrors,
    ...(lastDiscoveryError ? [lastDiscoveryError] : []),
    ...(tracked.size > 0 && lastTerminationErrors.length === 0
      ? [`metadata cleanup incomplete for ${tracked.size} sandbox(es)`]
      : []),
    ...(tracked.size === 0 && !lastDiscoveryError && lastTerminationErrors.length === 0
      ? ['metadata cleanup could not be verified with two empty sweeps']
      : []),
  ]
}

export async function deleteSnapshotWithRetry(
  client: Pick<SandboxClient, 'snapshots'>,
  snapshotId: string,
  options: CleanupOptions = {},
): Promise<string | undefined> {
  const attempts = options.attempts ?? 3
  const delayMs = options.delayMs ?? 250
  let lastError = ''
  for (let attempt = 1; attempt <= attempts; attempt++) {
    try {
      await client.snapshots.delete(snapshotId)
      return undefined
    } catch (error) {
      const status = (error as { status?: unknown })?.status
      if (status === 404) return undefined
      lastError = errorMessage(error)
      if (attempt < attempts) await sleep(delayMs * attempt)
    }
  }
  return `snapshot ${snapshotId}: ${lastError}`
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2))
  const client = new SandboxClient()
  const metadata = benchmarkMetadata('snapshot-source-batch-create', {
    counts: args.counts,
    baseline: args.baseline,
    baseline_parallelism: args.baselineParallelism,
    max_parallelism: args.maxParallelism,
    readiness_timeout_ms: args.readinessTimeoutMs,
    readiness_poll_ms: args.readinessPollMs,
  })
  const resourceMetadata = benchmarkResourceMetadata(metadata)
  console.log(`\nSnapshot batch-create benchmark: counts=[${args.counts.join(', ')}] baseline=${args.baseline}`)
  console.log(`Host: ${process.env.SANDBOX_API_URL}`)

  let source: ClientSandbox | undefined
  let snapshotId: string | undefined
  const tracked = new Map<string, ClientSandbox>()
  const rows: BatchRow[] = []
  let baseline:
    | {
      n: number
      maxParallelism: number
      wallMs: number
        perSandboxMs: number
        created: number
        failed: number
        errors: string[]
        cleanupErrors: string[]
        /** @deprecated Historic alias for perSandboxMs. */
        perCloneMs: number
      }
    | undefined
  let runFailed = false
  try {
    // --- Setup: one source sandbox -> snapshot -> terminate ---
    console.log('\n[setup] creating source sandbox...')
    source = await client.sandboxes.create({
      requestTimeoutMs: 60 * 60_000,
      metadata: resourceMetadata,
    })
    tracked.set(source.id, source)
    console.log(`[setup] source ${source.id} ready`)
    console.log('[setup] snapshotting...')
    const snapshot = await source.createSnapshot()
    snapshotId = snapshot.id
    console.log(`[setup] snapshot ${snapshotId}`)
    const sourceCleanupErrors = await terminateTracked([source])
    if (sourceCleanupErrors.length) {
      throw new Error(`source cleanup failed: ${sourceCleanupErrors.join('; ')}`)
    }
    tracked.delete(source.id)
    source = undefined
    console.log('[setup] source terminated\n')

    for (const n of args.counts) {
      const batchStarted = Date.now()
      const maxParallelism = Math.min(n, args.maxParallelism)
      const operation = await client.sandboxes.createMany({
        count: n,
        maxParallelism,
        source: { snapshotId },
        requestTimeoutMs: 30 * 60_000,
        metadata: resourceMetadata,
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
      const operationWallMs = Date.now() - batchStarted
      for (const result of state.results) {
        if (result.value) tracked.set(result.value.id, result.value)
      }

      const readinessStarted = Date.now()
      const items = await mapLimit(state.results, 32, async (result, position): Promise<ItemResult> => {
        if (!result.value) {
          return {
            index: result.error ? result.index : position,
            usable: false,
            readinessMs: 0,
            attempts: 0,
            ...(result.error ? { operationError: result.error } : {
              error: 'operation returned neither a sandbox nor an error',
            }),
          }
        }
        return {
          index: result.index,
          sandboxId: result.value.id,
          ...await waitUntilUsable(
            result.value,
            args.readinessTimeoutMs,
            args.readinessPollMs,
          ),
        }
      })
      const readinessWallMs = Date.now() - readinessStarted
      const wallMs = Date.now() - batchStarted
      const ok = items.filter((item) => item.usable).length
      const cleanupErrors = await terminateTracked(
        state.results
          .map((result) => result.value)
          .filter((sandbox): sandbox is ClientSandbox => sandbox !== undefined),
      )
      for (const result of state.results) {
        if (result.value && !cleanupErrors.some((error) => error.startsWith(`${result.value!.id}:`))) {
          tracked.delete(result.value.id)
        }
      }
      const row: BatchRow = {
        n,
        maxParallelism,
        operationWallMs,
        readinessWallMs,
        wallMs,
        perSandboxMs: wallMs / n,
        ok,
        failed: state.failed,
        operationId: operation.id,
        operationStatus: state.status,
        ...(operationError === undefined ? {} : { operationError }),
        items,
        cleanupErrors,
        perCloneMs: wallMs / n,
      }
      rows.push(row)
      const passed =
        operationError === undefined &&
        state.status === 'succeeded' &&
        state.succeeded === n &&
        state.failed === 0 &&
        items.length === n &&
        ok === n &&
        cleanupErrors.length === 0
      runFailed ||= !passed
      console.log(
        `  N=${String(n).padStart(3)} max-parallel=${String(maxParallelism).padStart(2)} ` +
        `operation=${fmt(operationWallMs)}ms ready=${fmt(wallMs)}ms ` +
        `per-sandbox=${fmt(wallMs / n)}ms usable=${ok}/${n} status=${state.status}`,
      )
      for (const item of items.filter((result) => !result.usable).slice(0, 5)) {
        console.error(
          `    item ${item.index} sandbox=${item.sandboxId ?? 'none'}: ` +
          `${item.operationError?.detail ?? item.error ?? 'not usable'}`,
        )
      }
      for (const cleanupError of cleanupErrors) {
        console.error(`    cleanup: ${cleanupError}`)
      }
      if (!passed) break
    }

    // --- Baseline: N concurrent cold boots, for the largest N ---
    if (args.baseline && !runFailed) {
      const n = Math.max(...args.counts)
      console.log(`\n[baseline] ${n} concurrent default-source creates...`)
      const t = Date.now()
      const baselineErrors: string[] = []
      const baselineParallelism = Math.min(n, args.baselineParallelism)
      const boots = await mapLimit(Array.from({ length: n }), baselineParallelism, async () => {
        try {
          const sandbox = await client.sandboxes.create({
            requestTimeoutMs: 30 * 60_000,
            metadata: resourceMetadata,
          })
          tracked.set(sandbox.id, sandbox)
          return sandbox
        } catch (error) {
          baselineErrors.push(errorMessage(error))
          return null
        }
      })
      const wallMs = Date.now() - t
      const created = boots.filter((sandbox): sandbox is ClientSandbox => sandbox !== null)
      const cleanupErrors = await terminateTracked(created)
      for (const sandbox of created) {
        if (!cleanupErrors.some((error) => error.startsWith(`${sandbox.id}:`))) tracked.delete(sandbox.id)
      }
      baseline = {
        n,
        maxParallelism: baselineParallelism,
        wallMs,
        perSandboxMs: wallMs / n,
        created: created.length,
        failed: n - created.length,
        errors: baselineErrors,
        cleanupErrors,
        perCloneMs: wallMs / n,
      }
      runFailed ||= created.length !== n || cleanupErrors.length > 0
      console.log(`  cold boot N=${n}  batch ${fmt(wallMs)}ms  per-boot ${fmt(wallMs / n)}ms`)
    }

    console.log(`\n${'='.repeat(56)}\n  SNAPSHOT BATCH-CREATE RESULTS\n${'='.repeat(56)}`)
    console.log('   N   operation(ms)  ready(ms)  per-sandbox(ms)  usable  operation')
    for (const r of rows) {
      console.log(
        `  ${String(r.n).padStart(3)}   ${fmt(r.operationWallMs).padStart(13)} ` +
        `${fmt(r.wallMs).padStart(10)}   ${fmt(r.perSandboxMs).padStart(12)} ` +
        `${r.ok}/${r.n}  ${r.operationStatus}`,
      )
    }
    if (baseline) {
      console.log(`\n  default-source baseline N=${baseline.n}: ${fmt(baseline.wallMs)}ms batch, ${fmt(baseline.perSandboxMs)}ms/sandbox`)
      const biggest = rows.find((r) => r.n === baseline!.n)
      if (biggest) console.log(`  snapshot-source speedup at N=${baseline.n}: ${fmt(baseline.wallMs / biggest.wallMs)}x`)
    }

    mkdirSync(RESULTS_DIR, { recursive: true })
    const ts = new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '').replace('T', '_')
    const outPath = args.output ?? join(RESULTS_DIR, `snapshot_batch_${ts}.json`)
    writeFileSync(outPath, JSON.stringify({
      metadata,
      passed: !runFailed,
      rows,
      baseline,
      // Deprecated compatibility alias for historic fan-out result readers.
      fanout: rows,
    }, null, 2))
    console.log(`\nSaved ${outPath}`)
    if (runFailed) {
      throw new Error('snapshot batch benchmark failed; inspect per-item and cleanup results')
    }
  } finally {
    const finalCleanupErrors = await cleanupRunSandboxes(
      client,
      tracked,
      resourceMetadata,
    )
    for (const cleanupError of finalCleanupErrors) console.error(`[cleanup] ${cleanupError}`)
    let snapshotCleanupError: string | undefined
    if (snapshotId) {
      snapshotCleanupError = await deleteSnapshotWithRetry(client, snapshotId)
      if (snapshotCleanupError) {
        console.error(`[cleanup] ${snapshotCleanupError}`)
      }
    }
    const cleanupErrors = [
      ...finalCleanupErrors,
      ...(snapshotCleanupError ? [snapshotCleanupError] : []),
    ]
    if (cleanupErrors.length) {
      throw new Error(`snapshot batch cleanup failed: ${cleanupErrors.join('; ')}`)
    }
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((e) => { console.error(e instanceof Error ? (e.stack ?? e.message) : e); process.exit(1) })
}
