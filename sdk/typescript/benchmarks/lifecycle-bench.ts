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
import { benchmarkMetadata, benchmarkResourceMetadata } from './metadata.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const RESULTS_DIR = join(HERE, 'results')

interface Args {
  iterations: number
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
  const args: Args = { iterations: 10 }
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i]
    if (arg === '--iterations') args.iterations = Number(argv[++i])
    else if (arg === '--output') args.output = argv[++i]
    else if (arg === '--help' || arg === '-h') {
      console.log('Usage: tsx benchmarks/lifecycle-bench.ts [--iterations N] [--output file.json]')
      process.exit(0)
    } else throw new Error(`unknown argument: ${arg}`)
  }
  if (!Number.isInteger(args.iterations) || args.iterations < 1) {
    throw new Error('--iterations must be a positive integer')
  }
  return args
}

function stats(samples: number[]): Stats {
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
    throw new Error(`resume verification failed: ${JSON.stringify(result.stdout)}`)
  }
}

const elapsed = (started: number): number => Date.now() - started
const format = (value: number): string => `${Math.round(value)}ms`

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2))
  const client = new SandboxClient()
  const metadata = benchmarkMetadata('sandbox-lifecycle', {
    iterations: args.iterations,
    operations: ['create', 'pause', 'resume', 'terminate'],
    resume_readiness_probe: 'echo ready',
  })
  const createSamples: number[] = []
  const pauseSamples: number[] = []
  const resumeSamples: number[] = []
  const terminateSamples: number[] = []

  console.log(`Lifecycle benchmark: ${args.iterations} iterations`)
  for (let i = 0; i < args.iterations; i++) {
    let sandbox: ClientSandbox | undefined
    try {
      let started = Date.now()
      sandbox = await client.sandboxes.create({
        requestTimeoutMs: 10 * 60_000,
        metadata: benchmarkResourceMetadata(metadata),
      })
      const createMs = elapsed(started)
      createSamples.push(createMs)

      started = Date.now()
      await sandbox.pause()
      const pauseMs = elapsed(started)
      pauseSamples.push(pauseMs)

      started = Date.now()
      await sandbox.resume()
      await verify(sandbox)
      const resumeMs = elapsed(started)
      resumeSamples.push(resumeMs)

      started = Date.now()
      await sandbox.terminate()
      const terminateMs = elapsed(started)
      terminateSamples.push(terminateMs)
      sandbox = undefined

      console.log(
        `  ${String(i + 1).padStart(2)}. create=${format(createMs)} ` +
        `pause=${format(pauseMs)} resume=${format(resumeMs)} terminate=${format(terminateMs)}`,
      )
    } finally {
      await sandbox?.terminate().catch(() => {})
    }
  }

  const result = {
    metadata,
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
  for (const [name, value] of Object.entries(result).filter(([name]) =>
    ['create', 'pause', 'resume', 'terminate'].includes(name))) {
    const valueStats = value as Stats
    console.log(`  ${name.padEnd(10)} ${format(valueStats.p50).padStart(8)} / ${format(valueStats.p95).padStart(8)}`)
  }

  mkdirSync(RESULTS_DIR, { recursive: true })
  const timestamp = new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '').replace('T', '_')
  const output = args.output ?? join(RESULTS_DIR, `lifecycle_${timestamp}.json`)
  writeFileSync(output, JSON.stringify(result, null, 2))
  console.log(`\nSaved ${output}`)
}

main().catch((error) => {
  console.error(error instanceof Error ? (error.stack ?? error.message) : error)
  process.exit(1)
})
