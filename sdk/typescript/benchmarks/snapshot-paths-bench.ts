/**
 * End-to-end snapshot path benchmark and correctness validator.
 *
 * Exercises the user journeys that a simple create/restore benchmark misses:
 *
 *   golden create -> command -> snapshot
 *   snapshot -> fork -> command -> snapshot -> fork -> snapshot
 *   snapshot -> 1:1 restore -> command -> snapshot -> command -> snapshot
 *   golden create -> pause -> implicit wake by command -> snapshot
 *
 * Every resumed sandbox must preserve both filesystem state and a process that
 * was live at snapshot time. Snapshots expected to stay on the fast path must
 * remain differential and point directly at one immutable golden base.
 *
 * Usage:
 *   SANDBOX_API_URL=... SANDBOX_API_KEY=... \
 *     tsx benchmarks/snapshot-paths-bench.ts \
 *       [--iterations N] [--diff-budget-ms N] [--full-budget-ms N] \
 *       [--restore-budget-ms N] [--output result.json]
 */
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { mkdirSync, writeFileSync } from 'node:fs'

import { Sandbox, type SnapshotInfo } from '../src/index.js'
import { benchmarkMetadata } from './metadata.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const RESULTS_DIR = join(HERE, 'results')

interface Args {
  iterations: number
  diffBudgetMs: number
  fullBudgetMs: number
  restoreBudgetMs: number
  output?: string
}

interface Measurement {
  iteration: number
  path: string
  operation: string
  elapsed_ms: number
  snapshot_id?: string
  format?: string
  base_id?: string
  expected_format?: string
  budget_ms?: number
  passed: boolean
  detail?: string
}

interface Stats {
  samples: number[]
  mean: number
  p50: number
  p90: number
  p95: number
  min: number
  max: number
}

function parseArgs(argv: string[]): Args {
  const args: Args = {
    iterations: 3,
    diffBudgetMs: 1_500,
    fullBudgetMs: 5_000,
    restoreBudgetMs: 3_000,
  }
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i]
    if (arg === '--iterations') args.iterations = Number(argv[++i])
    else if (arg === '--diff-budget-ms') args.diffBudgetMs = Number(argv[++i])
    else if (arg === '--full-budget-ms') args.fullBudgetMs = Number(argv[++i])
    else if (arg === '--restore-budget-ms') args.restoreBudgetMs = Number(argv[++i])
    else if (arg === '--output') args.output = argv[++i]
    else if (arg === '--help' || arg === '-h') {
      console.log(
        'Usage: tsx benchmarks/snapshot-paths-bench.ts ' +
        '[--iterations N] [--diff-budget-ms N] [--full-budget-ms N] ' +
        '[--restore-budget-ms N] [--output result.json]',
      )
      process.exit(0)
    } else {
      throw new Error(`unknown argument: ${arg}`)
    }
  }
  for (const [name, value] of Object.entries({
    iterations: args.iterations,
    diffBudgetMs: args.diffBudgetMs,
    fullBudgetMs: args.fullBudgetMs,
    restoreBudgetMs: args.restoreBudgetMs,
  })) {
    if (!Number.isInteger(value) || value < 1) {
      throw new Error(`--${name.replace(/[A-Z]/g, (c) => `-${c.toLowerCase()}`)} must be a positive integer`)
    }
  }
  return args
}

function stats(samples: number[]): Stats {
  const sorted = [...samples].sort((a, b) => a - b)
  const percentile = (p: number) =>
    sorted[Math.min(sorted.length - 1, Math.floor((p / 100) * sorted.length))]!
  return {
    samples,
    mean: samples.reduce((sum, value) => sum + value, 0) / samples.length,
    p50: percentile(50),
    p90: percentile(90),
    p95: percentile(95),
    min: sorted[0]!,
    max: sorted.at(-1)!,
  }
}

function elapsed(started: number): number {
  return Date.now() - started
}

function ms(value: number): string {
  return `${Math.round(value)}ms`
}

async function timed<T>(fn: () => Promise<T>): Promise<{ value: T; elapsedMs: number }> {
  const started = Date.now()
  const value = await fn()
  return { value, elapsedMs: elapsed(started) }
}

async function installState(sandbox: Sandbox, run: string, generation: number): Promise<void> {
  const command = [
    'set -eu',
    'mkdir -p /tmp/snapshot-paths',
    `printf '%s' '${run}' > /tmp/snapshot-paths/run`,
    `printf '%s' '${generation}' > /tmp/snapshot-paths/generation`,
    'if [ ! -s /tmp/snapshot-paths/pid ] || ! kill -0 "$(cat /tmp/snapshot-paths/pid)" 2>/dev/null; ' +
      'then nohup sleep 600 >/dev/null 2>&1 & printf \'%s\' "$!" > /tmp/snapshot-paths/pid; fi',
  ].join('; ')
  const result = await sandbox.commands.run(command)
  if (result.exitCode !== 0) {
    throw new Error(`install state failed: ${result.stderr}`)
  }
}

async function verifyState(sandbox: Sandbox, run: string, generation: number): Promise<void> {
  const command = [
    'set -eu',
    `test "$(cat /tmp/snapshot-paths/run)" = '${run}'`,
    `test "$(cat /tmp/snapshot-paths/generation)" = '${generation}'`,
    'test -s /tmp/snapshot-paths/pid',
    'kill -0 "$(cat /tmp/snapshot-paths/pid)"',
    'printf verified',
  ].join('; ')
  const result = await sandbox.commands.run(command)
  if (result.exitCode !== 0 || result.stdout.trim() !== 'verified') {
    throw new Error(
      `state verification failed for generation ${generation}: ` +
      `exit=${result.exitCode} stdout=${JSON.stringify(result.stdout)} stderr=${JSON.stringify(result.stderr)}`,
    )
  }
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2))
  const apiUrl = process.env.SANDBOX_API_URL
  const apiKey = process.env.SANDBOX_API_KEY
  if (!apiUrl || !apiKey) {
    throw new Error('SANDBOX_API_URL and SANDBOX_API_KEY are required')
  }
  const common = { apiUrl, apiKey }
  const metadata = benchmarkMetadata('snapshot-paths', {
    iterations: args.iterations,
    diff_budget_ms: args.diffBudgetMs,
    full_budget_ms: args.fullBudgetMs,
    restore_budget_ms: args.restoreBudgetMs,
  })
  const measurements: Measurement[] = []
  const failures: string[] = []
  const active = new Map<string, Sandbox>()
  const snapshots: string[] = []

  const remember = (sandbox: Sandbox): Sandbox => {
    active.set(sandbox.sandboxId, sandbox)
    return sandbox
  }
  const terminate = async (sandbox: Sandbox): Promise<void> => {
    await sandbox.terminate()
    active.delete(sandbox.sandboxId)
  }
  const record = (measurement: Measurement): void => {
    measurements.push(measurement)
    const icon = measurement.passed ? '✓' : '✗'
    const format = measurement.format ? ` ${measurement.format}` : ''
    console.log(
      `    ${icon} ${measurement.operation.padEnd(28)} ${ms(measurement.elapsed_ms).padStart(7)}${format}` +
      (measurement.detail ? ` — ${measurement.detail}` : ''),
    )
    if (!measurement.passed) {
      failures.push(
        `${measurement.path}/${measurement.operation}: ${measurement.detail ?? 'validation failed'}`,
      )
    }
  }
  const capture = async (
    iteration: number,
    path: string,
    operation: string,
    sandbox: Sandbox,
    expectedFormat: 'diff' | 'full' | 'any',
  ): Promise<SnapshotInfo> => {
    const snapshotName = `sp-${iteration}-${operation.replace(/[^a-z0-9]+/gi, '-').slice(0, 48)}`
    const { value: snapshot, elapsedMs } = await timed(() =>
      sandbox.snapshot({ name: snapshotName }),
    )
    snapshots.push(snapshot.snapshotId)
    const budget = expectedFormat === 'full' || (expectedFormat === 'any' && snapshot.format !== 'diff')
      ? args.fullBudgetMs
      : args.diffBudgetMs
    const formatOK = expectedFormat === 'any' || snapshot.format === expectedFormat
    const latencyOK = elapsedMs <= budget
    const baseOK = expectedFormat !== 'diff' || Boolean(snapshot.baseId)
    record({
      iteration,
      path,
      operation,
      elapsed_ms: elapsedMs,
      snapshot_id: snapshot.snapshotId,
      format: snapshot.format ?? 'full',
      base_id: snapshot.baseId,
      expected_format: expectedFormat,
      budget_ms: budget,
      passed: formatOK && latencyOK && baseOK,
      detail: [
        !formatOK ? `wanted ${expectedFormat}, got ${snapshot.format ?? 'full'}` : '',
        !baseOK ? 'missing immutable base' : '',
        !latencyOK ? `over ${budget}ms budget` : '',
      ].filter(Boolean).join('; ') || `base=${snapshot.baseId?.slice(0, 8) ?? 'none'}`,
    })
    return snapshot
  }
  const fork = async (
    iteration: number,
    path: string,
    operation: string,
    snapshotId: string,
  ): Promise<Sandbox> => {
    const { value: children, elapsedMs } = await timed(() =>
      Sandbox.fanout(snapshotId, 1, {
        ...common,
        timeoutMs: 10 * 60_000,
        hibernateAfterMs: -1,
      }),
    )
    if (children.length !== 1) {
      throw new Error(`${operation}: fanout returned ${children.length} children`)
    }
    const child = remember(children[0]!)
    record({
      iteration,
      path,
      operation,
      elapsed_ms: elapsedMs,
      budget_ms: args.restoreBudgetMs,
      passed: elapsedMs <= args.restoreBudgetMs,
      detail: elapsedMs <= args.restoreBudgetMs
        ? child.sandboxId.slice(0, 8)
        : `over ${args.restoreBudgetMs}ms budget`,
    })
    return child
  }
  const restore = async (
    iteration: number,
    path: string,
    operation: string,
    snapshotId: string,
  ): Promise<Sandbox> => {
    const { value: sandbox, elapsedMs } = await timed(() =>
      Sandbox.restore(snapshotId, {
        ...common,
        timeoutMs: 10 * 60_000,
        hibernateAfterMs: -1,
      }),
    )
    remember(sandbox)
    record({
      iteration,
      path,
      operation,
      elapsed_ms: elapsedMs,
      budget_ms: args.restoreBudgetMs,
      passed: elapsedMs <= args.restoreBudgetMs,
      detail: elapsedMs <= args.restoreBudgetMs
        ? sandbox.sandboxId.slice(0, 8)
        : `over ${args.restoreBudgetMs}ms budget`,
    })
    return sandbox
  }

  console.log('='.repeat(76))
  console.log('  SNAPSHOT USER-PATH BENCHMARK')
  console.log('='.repeat(76))
  console.log(`  target: ${metadata.target}`)
  console.log(`  release: ${metadata.release}`)
  console.log(`  iterations: ${args.iterations}`)
  console.log(
    `  budgets: diff=${args.diffBudgetMs}ms full=${args.fullBudgetMs}ms ` +
    `restore/fork=${args.restoreBudgetMs}ms`,
  )

  try {
    for (let iteration = 1; iteration <= args.iterations; iteration++) {
      const run = `${metadata.run_id}-${iteration}-${Date.now()}`
      console.log(`\n  [${iteration}/${args.iterations}] golden -> fork chain`)

      const { value: source, elapsedMs: createMs } = await timed(() =>
        Sandbox.create({
          ...common,
          timeoutMs: 10 * 60_000,
          hibernateAfterMs: -1,
          name: `snapshot-paths-${metadata.run_id}`,
        }),
      )
      remember(source)
      record({
        iteration,
        path: 'golden-fork-chain',
        operation: 'golden create',
        elapsed_ms: createMs,
        budget_ms: args.restoreBudgetMs,
        passed: createMs <= args.restoreBudgetMs,
        detail: source.sandboxId.slice(0, 8),
      })
      await installState(source, run, 0)
      const first = await capture(iteration, 'golden-fork-chain', 'snapshot golden child', source, 'diff')
      const goldenBase = first.baseId
      await terminate(source)

      const child = await fork(iteration, 'golden-fork-chain', 'fork first snapshot', first.snapshotId)
      await verifyState(child, run, 0)
      await installState(child, run, 1)
      const second = await capture(iteration, 'golden-fork-chain', 'snapshot first fork', child, 'diff')
      if (goldenBase && second.baseId && second.baseId !== goldenBase) {
        failures.push(
          `golden-fork-chain/snapshot first fork: base changed ` +
          `${goldenBase.slice(0, 8)} -> ${second.baseId.slice(0, 8)}`,
        )
      }
      await terminate(child)

      const grandchild = await fork(iteration, 'golden-fork-chain', 'fork second snapshot', second.snapshotId)
      await verifyState(grandchild, run, 1)
      await installState(grandchild, run, 2)
      const third = await capture(iteration, 'golden-fork-chain', 'snapshot second fork', grandchild, 'diff')
      if (goldenBase && third.baseId && third.baseId !== goldenBase) {
        failures.push(
          `golden-fork-chain/snapshot second fork: base changed ` +
          `${goldenBase.slice(0, 8)} -> ${third.baseId.slice(0, 8)}`,
        )
      }
      await terminate(grandchild)

      console.log(`  [${iteration}/${args.iterations}] restore -> repeated snapshot`)
      const restored = await restore(
        iteration,
        'restore-resnapshot',
        'restore chained snapshot',
        third.snapshotId,
      )
      await verifyState(restored, run, 2)
      await installState(restored, run, 3)
      const fourth = await capture(
        iteration,
        'restore-resnapshot',
        'snapshot restored sandbox',
        restored,
        'diff',
      )
      await installState(restored, run, 4)
      const fifth = await capture(
        iteration,
        'restore-resnapshot',
        'repeat snapshot same sandbox',
        restored,
        'diff',
      )
      if (goldenBase && fourth.baseId && fourth.baseId !== goldenBase) {
        failures.push('restore-resnapshot/snapshot restored sandbox: lost golden ancestry')
      }
      if (goldenBase && fifth.baseId && fifth.baseId !== goldenBase) {
        failures.push('restore-resnapshot/repeat snapshot same sandbox: lost golden ancestry')
      }
      await terminate(restored)

      const finalFork = await fork(
        iteration,
        'restore-resnapshot',
        'fork repeated snapshot',
        fifth.snapshotId,
      )
      await verifyState(finalFork, run, 4)
      await terminate(finalFork)

      console.log(`  [${iteration}/${args.iterations}] pause -> wake -> snapshot`)
      const paused = remember(await Sandbox.create({
        ...common,
        timeoutMs: 10 * 60_000,
        hibernateAfterMs: -1,
      }))
      await installState(paused, run, 10)
      const pauseResult = await timed(() => paused.pause())
      const frozen = await paused.refresh()
      record({
        iteration,
        path: 'pause-wake-snapshot',
        operation: 'pause',
        elapsed_ms: pauseResult.elapsedMs,
        budget_ms: args.diffBudgetMs,
        passed: frozen.status === 'hibernated' && pauseResult.elapsedMs <= args.diffBudgetMs,
        detail: `status=${frozen.status}`,
      })
      const wakeResult = await timed(() => verifyState(paused, run, 10))
      record({
        iteration,
        path: 'pause-wake-snapshot',
        operation: 'implicit wake + command',
        elapsed_ms: wakeResult.elapsedMs,
        budget_ms: args.restoreBudgetMs,
        passed: wakeResult.elapsedMs <= args.restoreBudgetMs,
      })
      await installState(paused, run, 11)
      // A hibernation wake currently has no durable user-visible snapshot row
      // to serve as a delta parent, so this is allowed to be a bounded full
      // snapshot. The measurement keeps that limitation visible.
      await capture(
        iteration,
        'pause-wake-snapshot',
        'snapshot after wake',
        paused,
        'any',
      )
      await terminate(paused)
    }
  } finally {
    for (const sandbox of [...active.values()]) {
      await sandbox.terminate().catch((error) => {
        failures.push(`cleanup sandbox ${sandbox.sandboxId}: ${String(error)}`)
      })
    }
    // Children depend on parents, so snapshots must be removed newest first.
    for (const snapshotId of [...snapshots].reverse()) {
      await Sandbox.deleteSnapshot(snapshotId, common).catch((error) => {
        failures.push(`cleanup snapshot ${snapshotId}: ${String(error)}`)
      })
    }
  }

  const grouped: Record<string, Stats> = {}
  for (const operation of new Set(measurements.map((value) => value.operation))) {
    grouped[operation] = stats(
      measurements
        .filter((value) => value.operation === operation)
        .map((value) => value.elapsed_ms),
    )
  }
  const result = {
    metadata,
    passed: failures.length === 0,
    failures,
    budgets: {
      diff_ms: args.diffBudgetMs,
      full_ms: args.fullBudgetMs,
      restore_ms: args.restoreBudgetMs,
    },
    measurements,
    stats: grouped,
  }

  mkdirSync(RESULTS_DIR, { recursive: true })
  const timestamp = new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '').replace('T', '_')
  const output = args.output ?? join(RESULTS_DIR, `snapshot_paths_${timestamp}.json`)
  writeFileSync(output, JSON.stringify(result, null, 2))

  console.log(`\n${'='.repeat(76)}`)
  console.log(failures.length === 0 ? '  PASS — all paths met correctness and latency budgets' : `  FAIL — ${failures.length} issue(s)`)
  for (const failure of failures) console.log(`    - ${failure}`)
  console.log(`  results: ${output}`)
  if (failures.length > 0) process.exitCode = 1
}

main().catch((error) => {
  console.error(error instanceof Error ? (error.stack ?? error.message) : error)
  process.exit(1)
})
