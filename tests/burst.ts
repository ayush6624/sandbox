/**
 * Burst / autoscale driver.
 *
 * Fires BURST_N concurrent creates at the gateway — deliberately MORE than the
 * fleet's current free-slot capacity — so the excess queues (bounded wait),
 * the autoscaler sees queue-depth + create-rate and scales the MIG up, and new
 * workers adopt the baked golden data disk and start draining the queue.
 *
 * It reports the create-latency distribution, the success/failure/queue-timeout
 * breakdown, spot-checks a sample of survivors with a real exec, and then kills
 * everything it created (TTL is a backstop in case it dies mid-run).
 *
 *   BURST_N=160 BURST_TTL_MS=900000 \
 *   LIVE_AUTOSCALE_BENCHMARK=I_UNDERSTAND_THIS_CREATES_REAL_VMS \
 *   SANDBOX_API_URL=http://localhost:9090 SANDBOX_API_KEY=... \
 *   npx tsx burst.ts
 */
import { Sandbox } from '../sdk/typescript/src/index.js'

const LIVE_ACK = 'I_UNDERSTAND_THIS_CREATES_REAL_VMS'
const URL = requireEnv('SANDBOX_API_URL')
const KEY = requireEnv('SANDBOX_API_KEY')
const N = Number(process.env.BURST_N ?? 160)
const MAX_N = Number(process.env.BURST_MAX_N ?? 512)
const TTL_MS = Number(process.env.BURST_TTL_MS ?? 900_000)
const SAMPLE = Number(process.env.BURST_SAMPLE ?? 24)
const RUN_ID = (process.env.BENCH_RUN_ID ?? `standalone-${process.pid}-${Date.now()}`)
  .replace(/[^a-zA-Z0-9-]/g, '-')
  .slice(0, 24)
const created: Sandbox[] = []
let cleanupPromise: Promise<number> | undefined
let cleanupArmed = false

function requireEnv(k: string): string {
  const v = process.env[k]
  if (!v) {
    console.error(`missing env ${k}`)
    process.exit(1)
  }
  return v
}

interface Rec {
  i: number
  ok: boolean
  ms: number
  id?: string
  host?: string
  err?: string
  status?: number
}

function iso(ms: number): string {
  return new Date(ms).toISOString().slice(11, 23)
}

async function main(): Promise<void> {
  if (process.env.LIVE_AUTOSCALE_BENCHMARK !== LIVE_ACK) {
    throw new Error(`refusing live run: set LIVE_AUTOSCALE_BENCHMARK=${LIVE_ACK}`)
  }
  for (const [name, value, minimum] of [
    ['BURST_N', N, 1],
    ['BURST_MAX_N', MAX_N, 1],
    ['BURST_TTL_MS', TTL_MS, 60_000],
    ['BURST_SAMPLE', SAMPLE, 1],
  ] as const) {
    if (!Number.isInteger(value) || value < minimum) {
      throw new Error(`${name} must be an integer >= ${minimum}, got ${value}`)
    }
  }
  if (N > MAX_N) throw new Error(`BURST_N=${N} exceeds BURST_MAX_N=${MAX_N}`)
  cleanupArmed = true

  const t0 = Date.now()
  console.log(`BURST start run=${RUN_ID} N=${N} ttl=${TTL_MS}ms target=${URL} t0=${iso(t0)}`)

  const recs: Rec[] = await Promise.all(
    Array.from({ length: N }, async (_, i): Promise<Rec> => {
      const s = Date.now()
      try {
        const sbx = await Sandbox.create({
          apiUrl: URL,
          apiKey: KEY,
          timeoutMs: TTL_MS,
          hibernateAfterMs: -1, // stay running so occupancy pressure is real
          name: `burst-${RUN_ID}-${i}`.slice(0, 63),
        })
        created.push(sbx)
        return { i, ok: true, ms: Date.now() - s, id: sbx.sandboxId, host: sbx.info.hostAddr ?? 'local' }
      } catch (e: unknown) {
        const err = e as { message?: string; status?: number }
        return { i, ok: false, ms: Date.now() - s, err: String(err?.message ?? e), status: err?.status }
      }
    })
  )

  const wall = Date.now() - t0
  const ok = recs.filter((r) => r.ok)
  const fail = recs.filter((r) => !r.ok)
  const ms = ok.map((r) => r.ms).sort((a, b) => a - b)
  const pct = (p: number) => (ms.length ? ms[Math.min(ms.length - 1, Math.floor((p / 100) * ms.length))] : 0)

  console.log(`\nBURST done wall=${wall}ms  ok=${ok.length}  fail=${fail.length}  (t_end=${iso(Date.now())})`)
  if (ms.length) {
    console.log(
      `create latency ms: min=${ms[0]} p50=${pct(50)} p90=${pct(90)} p99=${pct(99)} max=${ms[ms.length - 1]}`
    )
  }

  // Placement spread across hosts.
  const byHost = new Map<string, number>()
  for (const r of ok) byHost.set(r.host!, (byHost.get(r.host!) ?? 0) + 1)
  console.log('placement: ' + [...byHost.entries()].map(([h, n]) => `${h.split(':')[0]}=${n}`).join(' '))

  // Failure breakdown.
  if (fail.length) {
    const byErr = new Map<string, number>()
    for (const f of fail) {
      const k = (f.status ? `[${f.status}] ` : '') + (f.err || '').split('\n')[0].slice(0, 90)
      byErr.set(k, (byErr.get(k) ?? 0) + 1)
    }
    console.log('failures:')
    for (const [k, n] of byErr) console.log(`  x${n}: ${k}`)
  }

  // Spot-check a random sample actually executes.
  const sampleSize = Math.min(SAMPLE, created.length)
  const step = sampleSize ? created.length / sampleSize : 1
  const sample = Array.from({ length: sampleSize }, (_, i) => created[Math.floor(i * step)])
  let usable = 0
  await Promise.all(
    sample.map(async (sbx) => {
      try {
        const r = await sbx.commands.run('echo ok', { timeoutMs: 15_000 })
        if (r.stdout.trim() === 'ok') usable++
      } catch {
        /* counts as unusable */
      }
    })
  )
  console.log(`usable sample: ${usable}/${sample.length} executed`)

  // Cleanup.
  const killFail = await cleanup()
  const residual = await sweepRunOwned()
  console.log(`cleanup done (${killFail} kill failures, ${residual} run-owned residuals)`)
  const exitCode = fail.length || usable !== sample.length || killFail || residual ? 1 : 0
  console.log(`EXIT_CODE=${exitCode}`)
  process.exitCode = exitCode
}

main().catch((err) => {
  console.error('burst crashed:', err)
  console.log('EXIT_CODE=2')
  const cleanupRun = cleanupArmed
    ? cleanup().then(() => sweepRunOwned()).catch((cleanupError) => {
        console.error(`cleanup failed: ${String((cleanupError as Error)?.message ?? cleanupError)}`)
      })
    : Promise.resolve()
  void cleanupRun.finally(() => {
    process.exitCode = 2
  })
})

async function cleanup(): Promise<number> {
  if (!cleanupPromise) {
    cleanupPromise = Promise.allSettled(created.map((sandbox) => sandbox.terminate()))
      .then((killed) => killed.filter((result) => result.status === 'rejected').length)
  }
  return cleanupPromise
}

async function sweepRunOwned(timeoutMs = 30_000): Promise<number> {
  const deadline = Date.now() + timeoutMs
  let residual = -1
  do {
    const response = await fetch(`${URL.replace(/\/$/, '')}/sandboxes`, {
      headers: { Authorization: `Bearer ${KEY}` },
      signal: AbortSignal.timeout(15_000),
    })
    if (!response.ok) throw new Error(`cleanup list failed: HTTP ${response.status}`)
    const sandboxes = await response.json() as Array<{
      id?: string
      sandbox_id?: string
      name?: string
    }>
    const owned = sandboxes.filter((sandbox) =>
      (sandbox.name ?? '').startsWith(`burst-${RUN_ID}-`)
    )
    residual = owned.length
    if (!residual) return 0
    await Promise.allSettled(owned.map(async (sandbox) => {
      const id = sandbox.id ?? sandbox.sandbox_id
      if (!id) return
      await fetch(`${URL.replace(/\/$/, '')}/sandboxes/${encodeURIComponent(id)}`, {
        method: 'DELETE',
        headers: { Authorization: `Bearer ${KEY}` },
        signal: AbortSignal.timeout(15_000),
      })
    }))
    await new Promise((resolve) => setTimeout(resolve, 500))
  } while (Date.now() < deadline)
  return residual
}

for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  process.on(signal, () => {
    console.error(`\n${signal}: cleaning ${created.length} run-owned sandbox(es)`)
    const cleanupRun = cleanupArmed
      ? cleanup().then(() => sweepRunOwned()).catch((cleanupError) => {
          console.error(`cleanup failed: ${String((cleanupError as Error)?.message ?? cleanupError)}`)
        })
      : Promise.resolve()
    void cleanupRun.finally(() => process.exit(130))
  })
}
