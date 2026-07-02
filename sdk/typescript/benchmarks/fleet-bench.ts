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
import { Sandbox } from '../src/index.js'

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
  output?: string
}

function parseArgs(argv: string[]): Args {
  const a: Args = { count: 64, mode: 'default', iterations: 1, createConcurrency: 8, runConcurrency: 64 }
  for (let i = 0; i < argv.length; i++) {
    const k = argv[i]
    if (k === '--count') a.count = Number(argv[++i])
    else if (k === '--mode') a.mode = argv[++i] as Args['mode']
    else if (k === '--iterations') a.iterations = Number(argv[++i])
    else if (k === '--create-concurrency') a.createConcurrency = Number(argv[++i])
    else if (k === '--run-concurrency') a.runConcurrency = Number(argv[++i])
    else if (k === '--output') a.output = argv[++i]
    else throw new Error(`unknown arg: ${k}`)
  }
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
  const url = (process.env.SANDBOX_API_URL ?? '').replace(/\/+$/, '') + '/hosts'
  const res = await fetch(url, { headers: { Authorization: `Bearer ${process.env.SANDBOX_API_KEY ?? ''}` } })
  return res.json()
}

interface Run {
  idx: number
  id?: string
  createMs?: number
  runMs?: number
  totalTime?: number
  error?: string
  sbx?: Sandbox
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2))
  console.log(`\nFleet benchmark: count=${args.count} mode=${args.mode} iters=${args.iterations} ` +
    `create-concurrency=${args.createConcurrency} run-concurrency=${args.runConcurrency}`)
  console.log(`Gateway: ${process.env.SANDBOX_API_URL}`)

  const workload = readFileSync(WORKLOAD_SCRIPT, 'utf8')
  const fleetStart = Date.now()

  // --- Phase 1: create N sandboxes (bounded concurrency), all kept alive ---
  console.log(`\n[1/4] Creating ${args.count} sandboxes...`)
  const runs: Run[] = Array.from({ length: args.count }, (_, idx) => ({ idx }))
  await mapLimit(runs, args.createConcurrency, async (r) => {
    const t = Date.now()
    try {
      const sbx = await Sandbox.create({ timeoutMs: 30 * 60_000 })
      r.sbx = sbx
      r.id = sbx.sandboxId
      r.createMs = Date.now() - t
      process.stdout.write('.')
    } catch (e) {
      r.error = `create: ${e instanceof Error ? e.message : String(e)}`
      process.stdout.write('x')
    }
  })
  const live = runs.filter((r) => r.sbx)
  console.log(`\n  created ${live.length}/${args.count}`)

  // Distribution snapshot at peak occupancy (before any teardown).
  console.log('  host distribution at peak:')
  console.log('   ', JSON.stringify(await gatewayHosts()))

  // --- Phase 2: run the workload in every live sandbox concurrently ---
  console.log(`\n[2/4] Running workload in ${live.length} sandboxes (concurrency ${args.runConcurrency})...`)
  const cmd = `node --no-warnings ${GUEST_SCRIPT_PATH} --mode ${args.mode} --iterations ${args.iterations}`
  await mapLimit(live, args.runConcurrency, async (r) => {
    const t = Date.now()
    try {
      await r.sbx!.files.write(GUEST_SCRIPT_PATH, workload)
      const res = await r.sbx!.commands.run(cmd, { timeoutMs: 600_000 })
      r.runMs = Date.now() - t
      r.totalTime = metric(parseBenchmarkJson(res.stdout), 'total_time')
      process.stdout.write(r.totalTime !== undefined ? '.' : '?')
    } catch (e) {
      r.error = `run: ${e instanceof Error ? e.message : String(e)}`
      process.stdout.write('x')
    }
  })
  const ok = live.filter((r) => r.totalTime !== undefined)
  console.log(`\n  workload ok: ${ok.length}/${live.length}`)

  // --- Phase 3: aggregate ---
  const fleetWall = (Date.now() - fleetStart) / 1000
  const createS = stats(runs.filter((r) => r.createMs).map((r) => r.createMs! / 1000))
  const runWallS = stats(ok.map((r) => r.runMs! / 1000))
  const benchS = stats(ok.map((r) => r.totalTime!))

  console.log(`\n${'='.repeat(64)}\n  FLEET RESULTS\n${'='.repeat(64)}`)
  console.log(`  requested:        ${args.count}`)
  console.log(`  created:          ${live.length}`)
  console.log(`  workload success: ${ok.length}`)
  console.log(`  failures:         ${runs.filter((r) => r.error).length}`)
  console.log(`  fleet wall time:  ${fmt(fleetWall)}s`)
  console.log(`  create time  (s): mean ${fmt(createS.mean)}  p50 ${fmt(createS.p50)}  p95 ${fmt(createS.p95)}  max ${fmt(createS.max)}`)
  console.log(`  workload wall(s): mean ${fmt(runWallS.mean)}  p50 ${fmt(runWallS.p50)}  p95 ${fmt(runWallS.p95)}  max ${fmt(runWallS.max)}`)
  console.log(`  bench total  (s): mean ${fmt(benchS.mean)}  p50 ${fmt(benchS.p50)}  p95 ${fmt(benchS.p95)}  min ${fmt(benchS.min)}  max ${fmt(benchS.max)}`)
  const errs = runs.filter((r) => r.error)
  if (errs.length) {
    console.log('\n  first few errors:')
    for (const e of errs.slice(0, 5)) console.log(`   #${e.idx}: ${e.error}`)
  }

  // --- Phase 4: teardown ---
  console.log(`\n[3/4] (results above)\n[4/4] Tearing down ${live.length} sandboxes...`)
  await mapLimit(live, 16, async (r) => {
    try { await r.sbx!.kill() } catch { /* best effort */ }
  })
  console.log('  done; host state after teardown:')
  console.log('   ', JSON.stringify(await gatewayHosts()))

  mkdirSync(RESULTS_DIR, { recursive: true })
  const ts = new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '').replace('T', '_')
  const outPath = args.output ?? join(RESULTS_DIR, `fleet_${args.mode}_${args.count}_${ts}.json`)
  writeFileSync(outPath, JSON.stringify({
    args, fleetWall, created: live.length, workloadOk: ok.length,
    createStats: createS, runWallStats: runWallS, benchStats: benchS,
    runs: runs.map((r) => ({ idx: r.idx, id: r.id, createMs: r.createMs, runMs: r.runMs, totalTime: r.totalTime, error: r.error })),
  }, null, 2))
  console.log(`\nSaved ${outPath}`)
}

main().catch((e) => { console.error(e instanceof Error ? (e.stack ?? e.message) : e); process.exit(1) })
