/**
 * Fleet benchmark — bring up N sandboxes concurrently across the whole fleet
 * (via the gateway), run the `benchmark.ts` workload in every one of them at
 * once, then aggregate and tear down.
 *
 * Where run-bench.ts drives a single sandbox, this is a load test of the
 * multi-host gateway: SANDBOX_API_URL points at the gateway, which places each
 * create on the least-loaded host, so N sandboxes spread across the fleet.
 *
 * Usage:
 *   SANDBOX_API_URL=http://<gateway>:9090 SANDBOX_API_KEY=<tok> \
 *     tsx benchmarks/fleet-bench.ts [--count 64] [--mode default|fsync|large]
 *       [--iterations 1] [--create-concurrency 8] [--run-concurrency 64] [--output file.json]
 */
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { readFileSync, mkdirSync, writeFileSync } from 'node:fs'
import {
  NotFoundError,
  SandboxClient,
  type ClientSandbox,
} from '../src/index.js'
import { benchmarkMetadata, benchmarkResourceMetadata } from './metadata.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const WORKLOAD_SCRIPT = join(HERE, 'benchmark.ts')
const RESULTS_DIR = join(HERE, 'results')
const GUEST_SCRIPT_PATH = '/tmp/benchmark.ts'

interface Args {
  count: number
  mode: 'default' | 'fsync' | 'large'
  iterations: number
  createConcurrency: number
  runConcurrency: number
  cleanupTimeoutMs: number
  output?: string
}

function parseArgs(argv: string[]): Args {
  const a: Args = {
    count: 64,
    mode: 'default',
    iterations: 1,
    createConcurrency: 8,
    runConcurrency: 64,
    cleanupTimeoutMs: 10_000,
  }
  for (let i = 0; i < argv.length; i++) {
    const k = argv[i]
    if (k === '--count') a.count = Number(argv[++i])
    else if (k === '--mode') {
      const mode = argv[++i]
      if (mode !== 'default' && mode !== 'fsync' && mode !== 'large') {
        throw new Error(`--mode must be one of default|fsync|large, got: ${mode}`)
      }
      a.mode = mode
    }
    else if (k === '--iterations') a.iterations = Number(argv[++i])
    else if (k === '--create-concurrency') a.createConcurrency = Number(argv[++i])
    else if (k === '--run-concurrency') a.runConcurrency = Number(argv[++i])
    else if (k === '--cleanup-timeout-ms') a.cleanupTimeoutMs = Number(argv[++i])
    else if (k === '--output') a.output = argv[++i]
    else if (k === '--help' || k === '-h') {
      console.log(
        'Usage: tsx benchmarks/fleet-bench.ts [--count 64] ' +
        '[--mode default|fsync|large] [--iterations 1] ' +
        '[--create-concurrency 8] [--run-concurrency 64] ' +
        '[--cleanup-timeout-ms 10000] [--output file.json]',
      )
      process.exit(0)
    }
    else throw new Error(`unknown arg: ${k}`)
  }
  for (const [name, value] of [
    ['--count', a.count],
    ['--iterations', a.iterations],
    ['--create-concurrency', a.createConcurrency],
    ['--run-concurrency', a.runConcurrency],
    ['--cleanup-timeout-ms', a.cleanupTimeoutMs],
  ] as const) {
    if (!Number.isInteger(value) || value < 1) {
      throw new Error(`${name} must be a positive integer`)
    }
  }
  a.createConcurrency = Math.min(a.createConcurrency, a.count)
  if (a.runConcurrency > a.count) a.runConcurrency = a.count
  return a
}

/** Run `tasks` with at most `limit` in flight; preserves result order. */
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

function parseBenchmarkJson(output: string): Record<string, unknown> {
  const idx = output.indexOf('--- JSON ---')
  if (idx === -1) return { error: 'no JSON marker' }
  try {
    return JSON.parse(output.slice(idx + '--- JSON ---'.length).trim()) as Record<string, unknown>
  } catch (e) {
    return { error: `parse failed: ${e instanceof Error ? e.message : String(e)}` }
  }
}

function metric(results: Record<string, unknown>, key: string): number | undefined {
  const v = results[key]
  if (typeof v === 'number') return v
  if (v && typeof v === 'object' && 'mean' in v) {
    const m = (v as { mean: unknown }).mean
    return typeof m === 'number' ? m : undefined
  }
  return undefined
}

function pct(sorted: number[], p: number): number {
  if (!sorted.length) return NaN
  const i = Math.min(sorted.length - 1, Math.floor((p / 100) * sorted.length))
  return sorted[i]!
}
const stats = (xs: number[]) => {
  const s = [...xs].sort((a, b) => a - b)
  const sum = s.reduce((x, y) => x + y, 0)
  return { n: s.length, mean: sum / (s.length || 1), p50: pct(s, 50), p95: pct(s, 95), min: s[0] ?? NaN, max: s.at(-1) ?? NaN }
}
const fmt = (x: number) => (Number.isFinite(x) ? x.toFixed(3) : 'n/a')

async function gatewayHosts(): Promise<unknown> {
  const url = (process.env.SANDBOX_API_URL ?? '').replace(/\/+$/, '') + '/internal/v1/hosts'
  const res = await fetch(url, {
    headers: { Authorization: `Bearer ${process.env.SANDBOX_API_KEY ?? ''}` },
    signal: AbortSignal.timeout(10_000),
  })
  if (!res.ok) throw new Error(`host inventory returned HTTP ${res.status}`)
  return res.json()
}

interface Run {
  idx: number
  id?: string
  createMs?: number
  runMs?: number
  totalTime?: number
  error?: string
  createError?: string
  workloadError?: string
  cleanupMs?: number
  cleanupAttempts?: number
  cleanupVerified?: boolean
  cleanupError?: string
  cleanupAttemptErrors?: string[]
  sbx?: ClientSandbox
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))
const errorMessage = (error: unknown) =>
  String((error as Error)?.message ?? error).slice(0, 500)

async function cleanupRun(
  client: SandboxClient,
  run: Run,
  requestTimeoutMs: number,
): Promise<void> {
  if (!run.sbx) return
  const started = Date.now()
  let lastError = ''
  run.cleanupAttemptErrors = []
  for (let attempt = 1; attempt <= 3; attempt++) {
    run.cleanupAttempts = attempt
    try {
      await run.sbx.terminate({ timeoutMs: requestTimeoutMs })
    } catch (error) {
      if (!(error instanceof NotFoundError)) {
        lastError = `terminate: ${errorMessage(error)}`
        run.cleanupAttemptErrors.push(`attempt ${attempt} ${lastError}`)
      }
    }

    try {
      await client.sandboxes.get(run.sbx.id, AbortSignal.timeout(requestTimeoutMs))
      lastError = 'verification: sandbox still exists'
      run.cleanupAttemptErrors.push(`attempt ${attempt} ${lastError}`)
    } catch (error) {
      if (error instanceof NotFoundError) {
        run.cleanupVerified = true
        run.cleanupMs = Date.now() - started
        run.cleanupError = undefined
        return
      }
      lastError = `verification: ${errorMessage(error)}`
      run.cleanupAttemptErrors.push(`attempt ${attempt} ${lastError}`)
    }
    if (attempt < 3) await sleep(250 * attempt)
  }
  run.cleanupVerified = false
  run.cleanupMs = Date.now() - started
  run.cleanupError = lastError || 'cleanup could not be proven'
}

async function safeGatewayHosts(): Promise<{ value?: unknown; error?: string }> {
  try {
    return { value: await gatewayHosts() }
  } catch (error) {
    return { error: errorMessage(error) }
  }
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2))
  const client = new SandboxClient()
  const metadata = benchmarkMetadata('fleet-sqlite-filesystem', {
    count: args.count,
    mode: args.mode,
    iterations: args.iterations,
    create_concurrency: args.createConcurrency,
    run_concurrency: args.runConcurrency,
    cleanup_timeout_ms: args.cleanupTimeoutMs,
  })
  const workload = readFileSync(WORKLOAD_SCRIPT, 'utf8')
  const runs: Run[] = Array.from({ length: args.count }, (_, idx) => ({ idx }))
  const fleetStart = Date.now()
  let fleetWall = 0
  let peakHosts: { value?: unknown; error?: string } = {}
  let finalHosts: { value?: unknown; error?: string } = {}
  let fatalError: string | undefined

  const ts = new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '').replace('T', '_')
  mkdirSync(RESULTS_DIR, { recursive: true })
  const outPath = args.output ?? join(RESULTS_DIR, `fleet_${args.mode}_${args.count}_${ts}.json`)

  console.log(`\nFleet benchmark: count=${args.count} mode=${args.mode} iters=${args.iterations} ` +
    `create-concurrency=${args.createConcurrency} run-concurrency=${args.runConcurrency}`)
  console.log(`Gateway: ${process.env.SANDBOX_API_URL}`)

  try {
    console.log(`\n[1/4] Creating ${args.count} sandboxes...`)
    await mapLimit(runs, args.createConcurrency, async (run) => {
      const started = Date.now()
      try {
        const sandbox = await client.sandboxes.create({
          requestTimeoutMs: 30 * 60_000,
          metadata: benchmarkResourceMetadata(metadata),
        })
        run.sbx = sandbox
        run.id = sandbox.id
        run.createMs = Date.now() - started
        process.stdout.write('.')
      } catch (error) {
        run.createError = errorMessage(error)
        run.error = `create: ${run.createError}`
        process.stdout.write('x')
      }
    })
    const live = runs.filter((run) => run.sbx)
    console.log(`\n  created ${live.length}/${args.count}`)

    console.log('  host distribution at peak:')
    peakHosts = await safeGatewayHosts()
    console.log('   ', JSON.stringify(peakHosts.value ?? { error: peakHosts.error }))

    console.log(`\n[2/4] Running workload in ${live.length} sandboxes (concurrency ${args.runConcurrency})...`)
    const cmd = `node --no-warnings ${GUEST_SCRIPT_PATH} --mode ${args.mode} --iterations ${args.iterations}`
    await mapLimit(live, args.runConcurrency, async (run) => {
      const started = Date.now()
      try {
        await run.sbx!.files.write(GUEST_SCRIPT_PATH, workload)
        const response = await run.sbx!.commands.run(cmd, { timeoutMs: 600_000 })
        run.runMs = Date.now() - started
        const parsed = parseBenchmarkJson(response.stdout)
        run.totalTime = metric(parsed, 'total_time')
        if (run.totalTime === undefined) {
          run.workloadError = errorMessage(parsed.error ?? 'benchmark output has no total_time')
          run.error = `run: ${run.workloadError}`
        }
        process.stdout.write(run.totalTime !== undefined ? '.' : '?')
      } catch (error) {
        run.workloadError = errorMessage(error)
        run.error = `run: ${run.workloadError}`
        process.stdout.write('x')
      }
    })
    fleetWall = (Date.now() - fleetStart) / 1000
    console.log(`\n  workload ok: ${runs.filter((run) => run.totalTime !== undefined).length}/${live.length}`)
  } catch (error) {
    fatalError = errorMessage(error)
    fleetWall = (Date.now() - fleetStart) / 1000
    console.error(`\n  fatal benchmark error: ${fatalError}`)
  } finally {
    const createdRuns = runs.filter((run) => run.sbx)
    console.log(`\n[3/4] Aggregating results\n[4/4] Tearing down ${createdRuns.length} sandboxes...`)
    await mapLimit(createdRuns, 16, async (run) => {
      await cleanupRun(client, run, args.cleanupTimeoutMs)
      process.stdout.write(run.cleanupVerified ? '.' : 'x')
    })
    console.log('\n  host state after teardown:')
    finalHosts = await safeGatewayHosts()
    console.log('   ', JSON.stringify(finalHosts.value ?? { error: finalHosts.error }))

    const created = createdRuns.length
    const workloadOk = runs.filter((run) => run.totalTime !== undefined).length
    const cleanupVerified = createdRuns.filter((run) => run.cleanupVerified).length
    const createS = stats(runs.filter((run) => run.createMs !== undefined).map((run) => run.createMs! / 1000))
    const runWallS = stats(runs.filter((run) => run.totalTime !== undefined).map((run) => run.runMs! / 1000))
    const benchS = stats(runs.filter((run) => run.totalTime !== undefined).map((run) => run.totalTime!))
    const passed =
      fatalError === undefined &&
      created === args.count &&
      workloadOk === args.count &&
      cleanupVerified === created &&
      peakHosts.error === undefined &&
      finalHosts.error === undefined

    console.log(`\n${'='.repeat(64)}\n  FLEET RESULTS\n${'='.repeat(64)}`)
    console.log(`  requested:        ${args.count}`)
    console.log(`  created:          ${created}`)
    console.log(`  workload success: ${workloadOk}`)
    console.log(`  cleanup verified: ${cleanupVerified}/${created}`)
    console.log(`  failures:         ${runs.filter((run) => run.error || run.cleanupError).length}`)
    console.log(`  fleet wall time:  ${fmt(fleetWall)}s`)
    console.log(`  create time  (s): mean ${fmt(createS.mean)}  p50 ${fmt(createS.p50)}  p95 ${fmt(createS.p95)}  max ${fmt(createS.max)}`)
    console.log(`  workload wall(s): mean ${fmt(runWallS.mean)}  p50 ${fmt(runWallS.p50)}  p95 ${fmt(runWallS.p95)}  max ${fmt(runWallS.max)}`)
    console.log(`  bench total  (s): mean ${fmt(benchS.mean)}  p50 ${fmt(benchS.p50)}  p95 ${fmt(benchS.p95)}  min ${fmt(benchS.min)}  max ${fmt(benchS.max)}`)

    const errors = runs.filter((run) => run.error || run.cleanupError)
    if (errors.length) {
      console.log('\n  first few errors:')
      for (const run of errors.slice(0, 10)) {
        console.log(`   #${run.idx}: ${[run.error, run.cleanupError].filter(Boolean).join('; ')}`)
      }
    }

    writeFileSync(outPath, JSON.stringify({
      metadata,
      passed,
      args,
      fleetWall,
      created,
      workloadOk,
      createStats: createS,
      runWallStats: runWallS,
      benchStats: benchS,
      peakHosts,
      finalHosts,
      ...(fatalError === undefined ? {} : { fatalError }),
      cleanup: {
        requested: created,
        verified: cleanupVerified,
        failed: created - cleanupVerified,
      },
      runs: runs.map((run) => ({
        idx: run.idx,
        id: run.id,
        createMs: run.createMs,
        runMs: run.runMs,
        totalTime: run.totalTime,
        error: run.error,
        createError: run.createError,
        workloadError: run.workloadError,
        cleanupMs: run.cleanupMs,
        cleanupAttempts: run.cleanupAttempts,
        cleanupVerified: run.cleanupVerified,
        cleanupError: run.cleanupError,
        cleanupAttemptErrors: run.cleanupAttemptErrors,
      })),
    }, null, 2))
    console.log(`\nSaved ${outPath}`)
    if (!passed) process.exitCode = 1
  }
}

main().catch((e) => { console.error(e instanceof Error ? (e.stack ?? e.message) : e); process.exit(1) })
