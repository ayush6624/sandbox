/**
 * Compare N concurrent snapshot-sourced creates with one createMany operation.
 *
 * Both measurements end when the slowest create has completed. Every returned
 * sandbox is then command-probed before cleanup. Alternating the order between
 * rounds limits bias from host caches and background fleet activity.
 */
import { fileURLToPath } from 'node:url'
import { dirname, join, resolve } from 'node:path'
import { mkdirSync, writeFileSync } from 'node:fs'
import { SandboxClient, type ClientSandbox } from '../src/index.js'
import { benchmarkMetadata, benchmarkResourceMetadata } from './metadata.js'
import {
  cleanupRunSandboxes,
  deleteSnapshotWithRetry,
  terminateTracked,
} from './snapshot-batch-bench.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

interface Args {
  count: number
  rounds: number
  settleMs: number
  output?: string
}

function parseArgs(argv: string[]): Args {
  const args: Args = { count: 32, rounds: 4, settleMs: 2_000 }
  for (let i = 0; i < argv.length; i++) {
    const key = argv[i]
    if (key === '--count') args.count = Number(argv[++i])
    else if (key === '--rounds') args.rounds = Number(argv[++i])
    else if (key === '--settle-ms') args.settleMs = Number(argv[++i])
    else if (key === '--output') args.output = argv[++i]
    else throw new Error(`unknown argument: ${key}`)
  }
  if (!Number.isInteger(args.count) || args.count < 1 || args.count > 100) {
    throw new Error('--count must be an integer from 1 to 100')
  }
  if (!Number.isInteger(args.rounds) || args.rounds < 1) {
    throw new Error('--rounds must be a positive integer')
  }
  if (!Number.isInteger(args.settleMs) || args.settleMs < 0) {
    throw new Error('--settle-ms must be a non-negative integer')
  }
  return args
}

function percentile(values: number[], fraction: number): number {
  const sorted = [...values].sort((a, b) => a - b)
  return sorted[Math.min(sorted.length - 1, Math.ceil(sorted.length * fraction) - 1)]!
}

function summary(values: number[]) {
  return {
    min_ms: Math.min(...values),
    median_ms: percentile(values, 0.5),
    p95_ms: percentile(values, 0.95),
    max_ms: Math.max(...values),
  }
}

async function probeAll(sandboxes: ClientSandbox[]): Promise<number> {
  const started = Date.now()
  const results = await Promise.all(sandboxes.map(async (sandbox) => {
    const result = await sandbox.commands.run('echo comparison-ready', { timeoutMs: 10_000 })
    return result.stdout.trim() === 'comparison-ready'
  }))
  if (results.some((ok) => !ok)) throw new Error('a sandbox returned unexpected probe output')
  return Date.now() - started
}

interface ModeResult {
  mode: 'individual' | 'batch'
  round: number
  makespan_ms: number
  probe_wall_ms: number
  created: number
  completion_ms?: { p50: number; p95: number; max: number }
}

async function runIndividual(
  client: SandboxClient,
  snapshotId: string,
  count: number,
  round: number,
  metadata: Record<string, string>,
  tracked: Map<string, ClientSandbox>,
): Promise<ModeResult> {
  const started = Date.now()
  const completions: number[] = []
  const sandboxes = await Promise.all(Array.from({ length: count }, async () => {
    const sandbox = await client.sandboxes.create({
      source: { snapshotId },
      metadata,
      requestTimeoutMs: 10 * 60_000,
    })
    completions.push(Date.now() - started)
    tracked.set(sandbox.id, sandbox)
    return sandbox
  }))
  const makespanMs = Date.now() - started
  const probeWallMs = await probeAll(sandboxes)
  const cleanupErrors = await terminateTracked(sandboxes)
  if (cleanupErrors.length) throw new Error(`individual cleanup failed: ${cleanupErrors.join('; ')}`)
  for (const sandbox of sandboxes) tracked.delete(sandbox.id)
  return {
    mode: 'individual', round, makespan_ms: makespanMs, probe_wall_ms: probeWallMs,
    created: sandboxes.length,
    completion_ms: {
      p50: percentile(completions, 0.5),
      p95: percentile(completions, 0.95),
      max: Math.max(...completions),
    },
  }
}

async function runBatch(
  client: SandboxClient,
  snapshotId: string,
  count: number,
  round: number,
  metadata: Record<string, string>,
  tracked: Map<string, ClientSandbox>,
): Promise<ModeResult> {
  const started = Date.now()
  const operation = await client.sandboxes.createMany({
    count,
    maxParallelism: Math.min(count, 32),
    source: { snapshotId },
    metadata,
    requestTimeoutMs: 10 * 60_000,
  })
  const state = await operation.wait({ timeoutMs: 10 * 60_000 })
  const makespanMs = Date.now() - started
  const sandboxes = state.results.flatMap((result) => result.value ? [result.value] : [])
  for (const sandbox of sandboxes) tracked.set(sandbox.id, sandbox)
  if (state.status !== 'succeeded' || sandboxes.length !== count) {
    throw new Error(`batch ${state.status}: created ${sandboxes.length}/${count}`)
  }
  const probeWallMs = await probeAll(sandboxes)
  const cleanupErrors = await terminateTracked(sandboxes)
  if (cleanupErrors.length) throw new Error(`batch cleanup failed: ${cleanupErrors.join('; ')}`)
  for (const sandbox of sandboxes) tracked.delete(sandbox.id)
  return { mode: 'batch', round, makespan_ms: makespanMs, probe_wall_ms: probeWallMs, created: sandboxes.length }
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2))
  const client = new SandboxClient()
  const metadata = benchmarkMetadata('snapshot-create-vs-batch', {
    count: args.count,
    rounds: args.rounds,
    settle_ms: args.settleMs,
  })
  const resourceMetadata = benchmarkResourceMetadata(metadata)
  const tracked = new Map<string, ClientSandbox>()
  const rows: ModeResult[] = []
  let snapshotId: string | undefined

  try {
    console.log(`[setup] create source and snapshot on ${process.env.SANDBOX_API_URL}`)
    const source = await client.sandboxes.create({ metadata: resourceMetadata, requestTimeoutMs: 10 * 60_000 })
    tracked.set(source.id, source)
    snapshotId = (await source.createSnapshot()).id
    const sourceCleanup = await terminateTracked([source])
    if (sourceCleanup.length) throw new Error(sourceCleanup.join('; '))
    tracked.delete(source.id)

    // Stage and cache the snapshot before either measured mode.
    const warmup = await client.sandboxes.create({
      source: { snapshotId }, metadata: resourceMetadata, requestTimeoutMs: 10 * 60_000,
    })
    tracked.set(warmup.id, warmup)
    await probeAll([warmup])
    const warmupCleanup = await terminateTracked([warmup])
    if (warmupCleanup.length) throw new Error(warmupCleanup.join('; '))
    tracked.delete(warmup.id)

    for (let round = 1; round <= args.rounds; round++) {
      const order: Array<'individual' | 'batch'> = round % 2 === 1
        ? ['individual', 'batch']
        : ['batch', 'individual']
      for (const mode of order) {
        await sleep(args.settleMs)
        const row = mode === 'individual'
          ? await runIndividual(client, snapshotId, args.count, round, resourceMetadata, tracked)
          : await runBatch(client, snapshotId, args.count, round, resourceMetadata, tracked)
        rows.push(row)
        console.log(
          `[round ${round}] ${mode.padEnd(10)} all ${row.created} ready in ${row.makespan_ms}ms; ` +
          `probe ${row.probe_wall_ms}ms`,
        )
      }
    }

    const individual = rows.filter((row) => row.mode === 'individual').map((row) => row.makespan_ms)
    const batch = rows.filter((row) => row.mode === 'batch').map((row) => row.makespan_ms)
    const individualSummary = summary(individual)
    const batchSummary = summary(batch)
    const result = {
      metadata,
      passed: rows.every((row) => row.created === args.count),
      count: args.count,
      rounds: args.rounds,
      rows,
      summary: {
        individual: individualSummary,
        batch: batchSummary,
        median_batch_over_individual: batchSummary.median_ms / individualSummary.median_ms,
      },
    }
    const output = args.output ?? join(HERE, 'results', `snapshot_create_vs_batch_${Date.now()}.json`)
    mkdirSync(dirname(output), { recursive: true })
    writeFileSync(output, JSON.stringify(result, null, 2))
    console.log(JSON.stringify(result.summary, null, 2))
    console.log(`saved ${output}`)
  } finally {
    const cleanupErrors = await cleanupRunSandboxes(client, tracked, resourceMetadata)
    if (snapshotId) {
      const snapshotError = await deleteSnapshotWithRetry(client, snapshotId)
      if (snapshotError) cleanupErrors.push(snapshotError)
    }
    if (cleanupErrors.length) throw new Error(`cleanup failed: ${cleanupErrors.join('; ')}`)
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(error instanceof Error ? (error.stack ?? error.message) : error)
    process.exit(1)
  })
}
