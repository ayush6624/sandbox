/**
 * Resource lifecycle latency benchmark.
 *
 * Measures typed v1 create, pause, resume-to-usable, and terminate operations.
 * Each iteration uses a fresh sandbox so results are independent and cleanup is
 * deterministic.
 */
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { mkdirSync, writeFileSync } from 'node:fs'

import { SandboxClient, type ClientSandbox } from '../src/index.js'
import { benchmarkMetadata, benchmarkResourceMetadata, observeBuilds } from './metadata.js'
import { cleanupRunSandboxes } from './snapshot-batch-bench.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const RESULTS_DIR = join(HERE, 'results')

interface Args {
  iterations: number
  requestTimeoutMs: number
  output?: string
}

interface Stats {
  mean: number
  p50: number
  p90: number
  p95: number
  min: number
  max: number
  samples: number[]
}

function parseArgs(argv: string[]): Args {
  const args: Args = { iterations: 10, requestTimeoutMs: 120_000 }
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i]
    if (arg === '--iterations') args.iterations = Number(argv[++i])
    else if (arg === '--request-timeout-ms') args.requestTimeoutMs = Number(argv[++i])
    else if (arg === '--output') args.output = argv[++i]
    else if (arg === '--help' || arg === '-h') {
      console.log(
        'Usage: tsx benchmarks/lifecycle-bench.ts [--iterations N] ' +
        '[--request-timeout-ms N] [--output file.json]',
      )
      process.exit(0)
    } else throw new Error(`unknown argument: ${arg}`)
  }
  if (!Number.isInteger(args.iterations) || args.iterations < 1) {
    throw new Error('--iterations must be a positive integer')
  }
  if (!Number.isInteger(args.requestTimeoutMs) || args.requestTimeoutMs < 1) {
    throw new Error('--request-timeout-ms must be a positive integer')
  }
  return args
}

function stats(samples: number[]): Stats | null {
  if (samples.length === 0) return null
  const sorted = [...samples].sort((a, b) => a - b)
  const percentile = (value: number) =>
    sorted[Math.min(sorted.length - 1, Math.floor((value / 100) * sorted.length))]!
  return {
    mean: samples.reduce((sum, value) => sum + value, 0) / samples.length,
    p50: percentile(50),
    p90: percentile(90),
    p95: percentile(95),
    min: sorted[0]!,
    max: sorted.at(-1)!,
    samples,
  }
}

async function verify(sandbox: ClientSandbox): Promise<void> {
  const result = await sandbox.commands.run('echo ready', { timeoutMs: 15_000 })
  if (result.stdout.trim() !== 'ready') {
    throw new Error(`readiness verification failed: ${JSON.stringify(result.stdout)}`)
  }
}

const elapsed = (started: number): number => performance.now() - started
const format = (value: number): string => `${Math.round(value)}ms`

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2))
  const client = new SandboxClient({ requestTimeoutMs: args.requestTimeoutMs })
  const metadata = benchmarkMetadata('sandbox-lifecycle', {
    iterations: args.iterations,
    request_timeout_ms: args.requestTimeoutMs,
    operations: ['create', 'pause', 'resume', 'terminate'],
    create_readiness_probe: 'echo ready',
    resume_readiness_probe: 'echo ready',
  })
  if (process.env.SANDBOX_API_URL && process.env.SANDBOX_API_KEY) {
    await observeBuilds(metadata, process.env.SANDBOX_API_URL, process.env.SANDBOX_API_KEY)
  }
  const createSamples: number[] = []
  const createReadySamples: number[] = []
  const pauseSamples: number[] = []
  const resumeSamples: number[] = []
  const terminateSamples: number[] = []

  const tracked = new Map<string, ClientSandbox>()
  const failures: Array<{ iteration: number; stage: string; error: string }> = []
  const cleanupErrors: string[] = []
  let completed = 0

  console.log(`Lifecycle benchmark: ${args.iterations} iterations`)
  for (let i = 0; i < args.iterations; i++) {
    let sandbox: ClientSandbox | undefined
    let stage = 'create'
    try {
      let started = performance.now()
      sandbox = await client.sandboxes.create({
        requestTimeoutMs: args.requestTimeoutMs,
        metadata: benchmarkResourceMetadata(metadata),
      })
      tracked.set(sandbox.id, sandbox)
      createSamples.push(elapsed(started))
      stage = 'create_probe'
      await verify(sandbox)
      createReadySamples.push(elapsed(started))

      stage = 'pause'
      started = performance.now()
      await sandbox.pause({ timeoutMs: args.requestTimeoutMs })
      pauseSamples.push(elapsed(started))

      stage = 'resume'
      started = performance.now()
      await sandbox.resume({ timeoutMs: args.requestTimeoutMs })
      await verify(sandbox)
      resumeSamples.push(elapsed(started))

      stage = 'terminate'
      started = performance.now()
      await sandbox.terminate({ timeoutMs: args.requestTimeoutMs })
      terminateSamples.push(elapsed(started))
      tracked.delete(sandbox.id)
      completed++
    } catch (error) {
      failures.push({ iteration: i + 1, stage, error: error instanceof Error ? error.message : String(error) })
    } finally {
      if (sandbox && tracked.has(sandbox.id)) {
        try {
          await sandbox.terminate({ timeoutMs: args.requestTimeoutMs })
          tracked.delete(sandbox.id)
        } catch (error) {
          cleanupErrors.push(`${sandbox.id}: ${error instanceof Error ? error.message : String(error)}`)
        }
      }
    }
    if (failures.length || cleanupErrors.length) break
  }
  cleanupErrors.push(...await cleanupRunSandboxes(client, tracked, benchmarkResourceMetadata(metadata)))

  if (process.env.SANDBOX_API_URL && process.env.SANDBOX_API_KEY) {
    await observeBuilds(metadata, process.env.SANDBOX_API_URL, process.env.SANDBOX_API_KEY)
  }

  const result = {
    metadata,
    passed: completed === args.iterations && failures.length === 0 && cleanupErrors.length === 0,
    requested: args.iterations,
    completed,
    failures,
    cleanupErrors,
    create_ready: stats(createReadySamples),
    create: stats(createSamples),
    pause: stats(pauseSamples),
    resume: stats(resumeSamples),
    terminate: stats(terminateSamples),
    // Deprecated compatibility aliases for old lifecycle dashboards.
    hibernate: stats(pauseSamples),
    wake: stats(resumeSamples),
    kill: stats(terminateSamples),
  }

  console.log('\nLifecycle latency (p50 / p95):')
  for (const name of ['create', 'create_ready', 'pause', 'resume', 'terminate'] as const) {
    const value = result[name]
    console.log(`  ${name.padEnd(12)} ${value ? `${format(value.p50)} / ${format(value.p95)}` : 'no successful samples'}`)
  }

  mkdirSync(RESULTS_DIR, { recursive: true })
  const timestamp = new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '').replace('T', '_')
  const output = args.output ?? join(RESULTS_DIR, `lifecycle_${timestamp}.json`)
  mkdirSync(dirname(output), { recursive: true })
  writeFileSync(output, JSON.stringify(result, null, 2))
  if (!result.passed) process.exitCode = 1
  console.log(`\nSaved ${output}`)
}

main().catch((error) => {
  console.error(error instanceof Error ? (error.stack ?? error.message) : error)
  process.exit(1)
})
