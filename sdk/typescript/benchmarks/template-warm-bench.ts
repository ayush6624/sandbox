/** Compare batch create-to-command-ready latency for an existing template. */
import { mkdirSync, writeFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { SandboxClient, type ClientSandbox } from '../src/index.js'

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

async function terminateAll(sandboxes: ClientSandbox[]): Promise<string[]> {
  const results = await Promise.all(sandboxes.map(async (sandbox) => {
    try {
      await sandbox.terminate({ timeoutMs: 30_000 })
      return undefined
    } catch (error) {
      return `${sandbox.id}: ${String((error as Error)?.message ?? error)}`
    }
  }))
  return results.filter((error): error is string => error !== undefined)
}

async function main() {
  const args = parseArgs(process.argv.slice(2))
  const client = new SandboxClient({ requestTimeoutMs: 10 * 60_000 })
  const rows: Array<Record<string, unknown>> = []
  let failed = false

  console.log(`Template warm benchmark: template=${args.templateId} count=${args.count} rounds=${args.rounds}`)
  for (let round = 1; round <= args.rounds; round++) {
    const started = Date.now()
    const operation = await client.sandboxes.createMany({
      count: args.count,
      maxParallelism: Math.min(args.count, 32),
      source: { templateId: args.templateId },
      requestTimeoutMs: 10 * 60_000,
      metadata: { benchmark: 'template-warm', benchmark_round: String(round) },
    })
    const state = await operation.wait({ timeoutMs: 10 * 60_000, pollIntervalMs: 100 })
    const operationMs = Date.now() - started
    const sandboxes = state.results.flatMap((result) => result.value ? [result.value] : [])

    const readyStarted = Date.now()
    const probes = await Promise.all(sandboxes.map(async (sandbox) => {
      try {
        const result = await sandbox.commands.run('echo benchmark-ready', { timeoutMs: 30_000 })
        return result.stdout.trim() === 'benchmark-ready' ? undefined : `unexpected output from ${sandbox.id}`
      } catch (error) {
        return `${sandbox.id}: ${String((error as Error)?.message ?? error)}`
      }
    }))
    const readyMs = Date.now() - started
    const probeErrors = probes.filter((error): error is string => error !== undefined)
    const cleanupErrors = await terminateAll(sandboxes)
    const passed = state.status === 'succeeded' && state.succeeded === args.count && probeErrors.length === 0 && cleanupErrors.length === 0
    failed ||= !passed
    rows.push({
      round, operation_ms: operationMs, command_ready_ms: readyMs,
      per_sandbox_ms: readyMs / args.count, succeeded: state.succeeded,
      failed: state.failed, probe_errors: probeErrors, cleanup_errors: cleanupErrors,
      command_probe_ms: Date.now() - readyStarted,
    })
    console.log(`  round=${round} operation=${operationMs}ms command-ready=${readyMs}ms per-sandbox=${(readyMs / args.count).toFixed(1)}ms ok=${state.succeeded}/${args.count}`)
    if (round < args.rounds && args.roundDelayMs > 0) await sleep(args.roundDelayMs)
  }

  const samples = rows.map((row) => Number(row.command_ready_ms))
  const report = {
    template_id: args.templateId,
    count: args.count,
    rounds: args.rounds,
    release: process.env.SANDBOX_RELEASE ?? 'unknown',
    command_ready_ms: {
      mean: samples.reduce((sum, value) => sum + value, 0) / samples.length,
      p50: percentile(samples, 0.50), p95: percentile(samples, 0.95), samples,
    },
    rows,
  }
  console.log(JSON.stringify(report.command_ready_ms))
  if (args.output) {
    const path = resolve(args.output)
    mkdirSync(dirname(path), { recursive: true })
    writeFileSync(path, JSON.stringify(report, null, 2) + '\n')
    console.log(`Wrote ${path}`)
  }
  if (failed) process.exitCode = 1
}

await main()
