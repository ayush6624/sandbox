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
 *   SANDBOX_API_URL=http://localhost:9090 SANDBOX_API_KEY=... \
 *   npx tsx burst.ts
 */
import { Sandbox } from '../sdk/typescript/src/index.js'

const URL = requireEnv('SANDBOX_API_URL')
const KEY = requireEnv('SANDBOX_API_KEY')
const N = Number(process.env.BURST_N ?? 160)
const TTL_MS = Number(process.env.BURST_TTL_MS ?? 900_000)
const SAMPLE = Number(process.env.BURST_SAMPLE ?? 24)

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
  const t0 = Date.now()
  console.log(`BURST start N=${N} ttl=${TTL_MS}ms target=${URL} t0=${iso(t0)}`)
  const created: Sandbox[] = []

  const recs: Rec[] = await Promise.all(
    Array.from({ length: N }, async (_, i): Promise<Rec> => {
      const s = Date.now()
      try {
        const sbx = await Sandbox.create({
          apiUrl: URL,
          apiKey: KEY,
          timeoutMs: TTL_MS,
          hibernateAfterMs: -1, // stay running so occupancy pressure is real
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
  const shuffled = [...created].sort(() => 0.5) // stable-ish; enough for a spread
  const sample = shuffled.slice(0, Math.min(SAMPLE, shuffled.length))
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
  console.log(`cleanup: killing ${created.length} sandbox(es)...`)
  const killed = await Promise.allSettled(created.map((s) => s.kill()))
  const killFail = killed.filter((k) => k.status === 'rejected').length
  console.log(`cleanup done (${killFail} kill failures — TTL backstop will reap those)`)
  console.log(`EXIT_CODE=${fail.length && ok.length === 0 ? 1 : 0}`)
}

main().catch((err) => {
  console.error('burst crashed:', err)
  console.log('EXIT_CODE=2')
  process.exit(2)
})
