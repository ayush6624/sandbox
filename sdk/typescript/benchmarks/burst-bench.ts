/**
 * Burst benchmark — how the fleet handles a flood of short-lived sandboxes.
 *
 * Each task: create a sandbox → run a small math workload → kill it. Two modes:
 *
 *   churn (default): keep at most --concurrency tasks in flight; as each
 *     finishes its create→math→kill it frees a slot for the next. Measures
 *     sustained throughput for processing N short jobs on a bounded fleet.
 *
 *   --hold: fire all N creates (up to --concurrency at once), keep every
 *     sandbox alive until all creates have settled, then run math in the
 *     survivors, then tear everything down. Surfaces the capacity wall — when
 *     N exceeds fleet slots, excess creates get 503 — and lets the autoscaler
 *     ramp underneath.
 *
 * Usage:
 *   SANDBOX_API_URL=http://<gw>:9090 SANDBOX_API_KEY=<tok> \
 *     tsx benchmarks/burst-bench.ts [--count 500] [--concurrency 96] [--hold]
 *       [--output file.json]
 */
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { mkdirSync, writeFileSync } from 'node:fs'
import { Sandbox } from '../src/index.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const RESULTS_DIR = join(HERE, 'results')

// A small, deterministic math workload (~150-250ms of guest CPU): sum of
// i*i mod p for i in 1..2e6. Verifiable and cheap.
const N_TERMS = 2_000_000
const MOD = 1_000_000_007
const MATH_CMD = `node -e "let s=0;for(let i=1;i<=${N_TERMS};i++)s=(s+i*i)%${MOD};console.log(s)"`
function expectedMathResult(): string {
  let s = 0
  for (let i = 1; i <= N_TERMS; i++) s = (s + i * i) % MOD
  return String(s)
}

interface Args {
  count: number
  concurrency: number
  hold: boolean
  retryMs: number
  output?: string
}
function parseArgs(argv: string[]): Args {
  const a: Args = { count: 500, concurrency: 96, hold: false, retryMs: 0 }
  for (let i = 0; i < argv.length; i++) {
    const k = argv[i]
    if (k === '--count') a.count = Number(argv[++i])
    else if (k === '--concurrency') a.concurrency = Number(argv[++i])
    else if (k === '--hold') a.hold = true
    else if (k === '--retry-ms') a.retryMs = Number(argv[++i])
    else if (k === '--output') a.output = argv[++i]
  }
  return a
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

// createWithRetry rides out capacity/pool pushback with jittered exponential
// backoff up to retryMs — how a real client absorbs a burst while the
// autoscaler adds hosts and slots recycle. retryMs=0 disables (raw behavior).
async function createWithRetry(retryMs: number): Promise<{ sbx: Sandbox; retries: number }> {
  const deadline = Date.now() + retryMs
  let delay = 200
  let retries = 0
  while (true) {
    try {
      return { sbx: await Sandbox.create({ timeoutMs: 10 * 60_000 }), retries }
    } catch (e) {
      const o = classify(e)
      const retriable = o === 'capacity' || o === 'tap_pool'
      if (!retriable || Date.now() >= deadline) throw e
      retries++
      await sleep(Math.min(delay, 2000) * (0.5 + Math.random()))
      delay *= 2
    }
  }
}

type Outcome = 'ok' | 'capacity' | 'tap_pool' | 'agent_timeout' | 'other'
function classify(err: unknown): Outcome {
  const m = String((err as Error)?.message ?? err)
  if (m.includes('no host with free capacity')) return 'capacity'
  if (m.includes('tap pool exhausted') || m.includes('ip pool') || m.includes('port pool')) return 'tap_pool'
  if (m.includes('agent never became ready') || m.includes('agent not ready')) return 'agent_timeout'
  return 'other'
}

interface Rec {
  createMs?: number
  execMs?: number
  killMs?: number
  retries?: number
  outcome: Outcome
  err?: string
}

function pctl(xs: number[], p: number): number {
  if (xs.length === 0) return NaN
  const s = [...xs].sort((a, b) => a - b)
  return s[Math.min(s.length - 1, Math.floor((p / 100) * s.length))] ?? NaN
}
function stat(label: string, xs: number[]): string {
  if (xs.length === 0) return `${label}: (none)`
  const mean = xs.reduce((a, b) => a + b, 0) / xs.length
  return `${label}: n=${xs.length} mean=${mean.toFixed(0)} p50=${pctl(xs, 50).toFixed(0)} p95=${pctl(xs, 95).toFixed(0)} p99=${pctl(xs, 99).toFixed(0)} max=${Math.max(...xs).toFixed(0)} (ms)`
}

async function mapLimit<T>(n: number, limit: number, fn: (i: number) => Promise<T>): Promise<T[]> {
  const out: T[] = new Array(n)
  let next = 0
  let peak = 0
  let inFlight = 0
  const worker = async () => {
    while (true) {
      const i = next++
      if (i >= n) return
      inFlight++
      peak = Math.max(peak, inFlight)
      try {
        out[i] = await fn(i)
      } finally {
        inFlight--
      }
    }
  }
  await Promise.all(Array.from({ length: Math.min(limit, n) }, worker))
  ;(out as any).peak = peak
  return out
}

async function runChurn(args: Args, expected: string): Promise<Rec[]> {
  return mapLimit(args.count, args.concurrency, async () => {
    const rec: Rec = { outcome: 'ok' }
    const t0 = Date.now()
    let sbx: Sandbox | undefined
    try {
      const c = await createWithRetry(args.retryMs)
      sbx = c.sbx
      rec.retries = c.retries
      rec.createMs = Date.now() - t0
      const t1 = Date.now()
      const res = await sbx.commands.run(MATH_CMD)
      rec.execMs = Date.now() - t1
      if (res.stdout.trim() !== expected) {
        rec.outcome = 'other'
        rec.err = `bad math result: ${res.stdout.trim()}`
      }
    } catch (e) {
      rec.outcome = classify(e)
      rec.err = String((e as Error)?.message ?? e).slice(0, 160)
    } finally {
      if (sbx) {
        const tk = Date.now()
        try {
          await sbx.kill()
          rec.killMs = Date.now() - tk
        } catch { /* best effort */ }
      }
    }
    return rec
  })
}

async function runHold(args: Args, expected: string): Promise<Rec[]> {
  // Phase 1: fire all creates (bounded), keep survivors alive.
  const live: (Sandbox | undefined)[] = new Array(args.count)
  const recs = await mapLimit(args.count, args.concurrency, async (i) => {
    const rec: Rec = { outcome: 'ok' }
    const t0 = Date.now()
    try {
      const c = await createWithRetry(args.retryMs)
      rec.retries = c.retries
      rec.createMs = Date.now() - t0
      live[i] = c.sbx
    } catch (e) {
      rec.outcome = classify(e)
      rec.err = String((e as Error)?.message ?? e).slice(0, 160)
    }
    return rec
  })
  // Phase 2: run math in every survivor concurrently.
  await mapLimit(args.count, args.concurrency, async (i) => {
    const sbx = live[i]
    const rec = recs[i]
    if (!sbx || !rec || rec.outcome !== 'ok') return null
    const t1 = Date.now()
    try {
      const res = await sbx.commands.run(MATH_CMD)
      rec.execMs = Date.now() - t1
      if (res.stdout.trim() !== expected) { rec.outcome = 'other'; rec.err = 'bad math result' }
    } catch (e) {
      rec.outcome = classify(e)
      rec.err = String((e as Error)?.message ?? e).slice(0, 160)
    }
    return null
  })
  // Phase 3: tear everything down.
  await mapLimit(args.count, args.concurrency, async (i) => {
    const sbx = live[i]
    const rec = recs[i]
    if (!sbx) return null
    const tk = Date.now()
    try {
      await sbx.kill()
      if (rec) rec.killMs = Date.now() - tk
    } catch { /* best effort */ }
    return null
  })
  return recs
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2))
  if (!process.env.SANDBOX_API_URL || !process.env.SANDBOX_API_KEY) {
    console.error('set SANDBOX_API_URL and SANDBOX_API_KEY')
    process.exit(1)
  }
  const expected = expectedMathResult()
  console.log(`Burst: count=${args.count} concurrency=${args.concurrency} mode=${args.hold ? 'hold' : 'churn'}`)
  console.log(`Target: ${process.env.SANDBOX_API_URL}  workload: sum(i^2 mod p, i=1..${N_TERMS})=${expected}`)

  const started = Date.now()
  const recs = args.hold ? await runHold(args, expected) : await runChurn(args, expected)
  const wallMs = Date.now() - started

  const by = (o: Outcome) => recs.filter((r) => r.outcome === o)
  const ok = by('ok')
  const counts: Record<Outcome, number> = {
    ok: ok.length,
    capacity: by('capacity').length,
    tap_pool: by('tap_pool').length,
    agent_timeout: by('agent_timeout').length,
    other: by('other').length,
  }
  const createMs = recs.map((r) => r.createMs).filter((x): x is number => x != null)
  const execMs = ok.map((r) => r.execMs).filter((x): x is number => x != null)
  const killMs = recs.map((r) => r.killMs).filter((x): x is number => x != null)

  console.log(`\n${'='.repeat(64)}\n  BURST RESULTS (${args.hold ? 'hold' : 'churn'})\n${'='.repeat(64)}`)
  console.log(`  requested:        ${args.count}`)
  console.log(`  ok:               ${counts.ok}`)
  console.log(`  capacity (503):   ${counts.capacity}`)
  console.log(`  tap/ip/port pool: ${counts.tap_pool}`)
  console.log(`  agent timeout:    ${counts.agent_timeout}`)
  console.log(`  other errors:     ${counts.other}`)
  console.log(`  wall time:        ${(wallMs / 1000).toFixed(1)}s`)
  console.log(`  throughput (ok):  ${(counts.ok / (wallMs / 1000)).toFixed(1)} sandboxes/s`)
  console.log(`  peak in-flight:   ${(recs as any).peak ?? 'n/a'}`)
  const totalRetries = recs.reduce((s, r) => s + (r.retries ?? 0), 0)
  if (args.retryMs > 0) console.log(`  create retries:   ${totalRetries} (retry budget ${args.retryMs}ms)`)
  console.log(`  ${stat('create', createMs)}`)
  console.log(`  ${stat('math exec', execMs)}`)
  console.log(`  ${stat('kill', killMs)}`)
  const errs = recs.filter((r) => r.outcome !== 'ok' && r.err).slice(0, 4)
  if (errs.length) { console.log('  sample errors:'); errs.forEach((r, i) => console.log(`   #${i} [${r.outcome}] ${r.err}`)) }

  mkdirSync(RESULTS_DIR, { recursive: true })
  const out = args.output ?? join(RESULTS_DIR, `burst_${args.hold ? 'hold' : 'churn'}_${args.count}.json`)
  writeFileSync(out, JSON.stringify({ args, wallMs, counts, createMs, execMs, killMs, peak: (recs as any).peak }, null, 2))
  console.log(`\n  Saved ${out}`)
}

main().catch((e) => { console.error(e); process.exit(1) })
