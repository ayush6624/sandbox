/** Compare batch create-to-command-ready latency for an existing template. */
import { mkdirSync, writeFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { SandboxClient, type ClientSandbox } from '../src/index.js'
import { benchmarkMetadata, benchmarkResourceMetadata, observeBuilds } from './metadata.js'
import { observeBatch } from './observe-batch.js'
import { cleanupRunSandboxes } from './snapshot-batch-bench.js'

interface Args {
  templateId: string
  count: number
  rounds: number
  roundDelayMs: number
  output?: string
}

function parseArgs(argv: string[]): Args {
  const out: Args = { templateId: '', count: 4, rounds: 3, roundDelayMs: 3_000 }
  for (let i = 0; i < argv.length; i++) {
    const key = argv[i]
    if (key === '--template-id') out.templateId = argv[++i] ?? ''
    else if (key === '--count') out.count = Number(argv[++i])
    else if (key === '--rounds') out.rounds = Number(argv[++i])
    else if (key === '--round-delay-ms') out.roundDelayMs = Number(argv[++i])
    else if (key === '--output') out.output = argv[++i]
    else throw new Error(`unknown argument: ${key}`)
  }
  if (!out.templateId) throw new Error('--template-id is required')
  for (const [name, value] of [['--count', out.count], ['--rounds', out.rounds], ['--round-delay-ms', out.roundDelayMs]] as const) {
    if (!Number.isInteger(value) || value < (name === '--round-delay-ms' ? 0 : 1)) {
      throw new Error(`${name} must be ${name === '--round-delay-ms' ? 'a non-negative' : 'a positive'} integer`)
    }
  }
  return out
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))
const percentile = (values: number[], p: number) => {
  const sorted = [...values].sort((a, b) => a - b)
  return sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * p))]!
}

async function main() {
  const args = parseArgs(process.argv.slice(2))
  const client = new SandboxClient({ requestTimeoutMs: 10 * 60_000 })
  const metadata = benchmarkMetadata('template-warm', {
    template_id: args.templateId, count: args.count, rounds: args.rounds,
    round_delay_ms: args.roundDelayMs, poll_interval_ms: 100,
    command_probe_ms: 'maximum individual probe duration',
  })
  if (process.env.SANDBOX_API_URL && process.env.SANDBOX_API_KEY) {
    await observeBuilds(metadata, process.env.SANDBOX_API_URL, process.env.SANDBOX_API_KEY)
  }
  const resourceMetadata = benchmarkResourceMetadata(metadata)
  const tracked = new Map<string, ClientSandbox>()
  const rows = []
  const cleanupErrors: string[] = []
  let failure: string | undefined

  try {
    for (let round = 1; round <= args.rounds; round++) {
      const started = performance.now()
      const items: Array<{ index: number; sandbox_id: string; command_ready_ms?: number; probe_ms: number; error?: string }> = []
      const operation = await client.sandboxes.createMany({
        count: args.count,
        maxParallelism: Math.min(args.count, 32),
        source: { templateId: args.templateId },
        requestTimeoutMs: 10 * 60_000,
        metadata: { ...resourceMetadata, benchmark_round: String(round) },
      })
      const acceptedMs = performance.now() - started
      let operationMs: number | undefined
      let operationError: string | undefined
      try {
        operationMs = acceptedMs + await observeBatch({
          operation, timeoutMs: 10 * 60_000,
          onSandbox: async (sandbox, index) => {
            tracked.set(sandbox.id, sandbox)
            const probeStarted = performance.now()
            try {
              const result = await sandbox.commands.run('echo benchmark-ready', { timeoutMs: 30_000 })
              if (result.stdout.trim() !== 'benchmark-ready') throw new Error('unexpected readiness output')
              items.push({ index, sandbox_id: sandbox.id, command_ready_ms: performance.now() - started, probe_ms: performance.now() - probeStarted })
            } catch (error) {
              items.push({ index, sandbox_id: sandbox.id, probe_ms: performance.now() - probeStarted, error: error instanceof Error ? error.message : String(error) })
            }
          },
        })
      } catch (error) {
        operationError = error instanceof Error ? error.message : String(error)
      }
      const ready = items.flatMap((item) => item.command_ready_ms === undefined ? [] : [item.command_ready_ms])
      const readyMs = ready.length === args.count ? Math.max(...ready) : undefined
      const cleanupStarted = performance.now()
      const roundCleanup = await cleanupRunSandboxes(client, tracked, resourceMetadata)
      cleanupErrors.push(...roundCleanup)
      const passed = operationError === undefined && operation.state.status === 'succeeded' && readyMs !== undefined && roundCleanup.length === 0
      rows.push({
        round, passed, operation_id: operation.id, accepted_ms: acceptedMs, operation_ms: operationMs,
        command_ready_ms: readyMs, amortized_ms_per_sandbox: readyMs === undefined ? undefined : readyMs / args.count,
        command_probe_ms: items.length ? Math.max(...items.map((item) => item.probe_ms)) : undefined,
        cleanup_ms: performance.now() - cleanupStarted, cleanup_errors: roundCleanup,
        operation_error: operationError, operation_status: operation.state.status, operation_results: operation.state.results.map((result) => ({ index: result.index, error: result.error })), items: items.sort((a, b) => a.index - b.index),
      })
      if (!passed) throw new Error(`template round ${round} failed; inspect operation and items`)
      if (round < args.rounds) await sleep(args.roundDelayMs)
    }
  } catch (error) {
    failure = error instanceof Error ? error.message : String(error)
  } finally {
    cleanupErrors.push(...await cleanupRunSandboxes(client, tracked, resourceMetadata))
    if (process.env.SANDBOX_API_URL && process.env.SANDBOX_API_KEY) {
      await observeBuilds(metadata, process.env.SANDBOX_API_URL, process.env.SANDBOX_API_KEY)
    }

    const samples = rows.flatMap((row) => row.passed && row.command_ready_ms !== undefined ? [row.command_ready_ms] : [])
    const report = {
      metadata,
      passed: failure === undefined && cleanupErrors.length === 0 && rows.length === args.rounds,
      template_id: args.templateId, count: args.count, rounds: args.rounds,
      command_ready_ms: samples.length ? {
        mean: samples.reduce((sum, value) => sum + value, 0) / samples.length,
        p50: percentile(samples, 0.50), p95: percentile(samples, 0.95), samples,
      } : null,
      rows, cleanupErrors, error: failure,
    }
    const output = resolve(args.output ?? fileURLToPath(new URL(`./results/template_warm_${Date.now()}.json`, import.meta.url)))
    mkdirSync(dirname(output), { recursive: true })
    writeFileSync(output, JSON.stringify(report, null, 2) + '\n')
    console.log(`Saved ${output}`)
    if (!report.passed) process.exitCode = 1
  }
}

await main()
