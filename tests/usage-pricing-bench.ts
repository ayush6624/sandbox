/**
 * Real-world usage/pricing benchmark, driven through the TypeScript SDK.
 *
 * The ledger's unit tests prove the arithmetic. This asks the different
 * question a pricing decision needs answered: for the shapes customers
 * actually run, what does the meter SAY, what did the platform actually
 * spend, and where do those two diverge enough to matter in money?
 *
 * Every scenario records three independent numbers:
 *   - wall:   measured client-side, what the customer perceives
 *   - billed: what the ledger will invoice (allocated resources x duration)
 *   - spent:  host CPU actually consumed (recorded, never billed)
 *
 * Run it from the CONTROL VM. A laptop tunnel adds hundreds of ms of RTT per
 * call, which lands directly in the wall-clock column and would be read as
 * metering drift.
 *
 *   SANDBOX_API_URL=http://10.160.0.100:9090 SANDBOX_API_KEY=<token> \
 *     tsx usage-pricing-bench.ts
 *
 * Rates are ILLUSTRATIVE — there is no pricing model in the repo yet (phase 4).
 * Override with PRICE_VCPU_HOUR / PRICE_GIB_HOUR. They exist so the edge cases
 * show up in dollars instead of seconds, since "is this worth fixing" is a
 * question about money.
 */
import { SandboxClient, type ClientSandbox, type UsageReport } from '../sdk/typescript/src/index.js'

const baseUrl = process.env.SANDBOX_API_URL
const apiKey = process.env.SANDBOX_API_KEY
if (!baseUrl || !apiKey) throw new Error('SANDBOX_API_URL and SANDBOX_API_KEY are required')

const PRICE_VCPU_HOUR = Number(process.env.PRICE_VCPU_HOUR ?? 0.05)
const PRICE_GIB_HOUR = Number(process.env.PRICE_GIB_HOUR ?? 0.01)
const RUN = `pricebench-${Date.now().toString(36)}`

const client = new SandboxClient({ baseUrl, apiKey, maxRetries: 2 })
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))
const created: string[] = []
const findings: string[] = []
let failures = 0

interface Row {
  scenario: string
  shape: string
  wallRunningS: number
  billedS: number
  intervals: number
  vcpuS: number
  mibS: number
  cpuS: number
  usd: number
}
const rows: Row[] = []

function usd(vcpuS: number, mibS: number): number {
  return (vcpuS / 3600) * PRICE_VCPU_HOUR + (mibS / 1024 / 3600) * PRICE_GIB_HOUR
}
function money(n: number): string {
  return n >= 0.01 ? `$${n.toFixed(4)}` : `$${n.toFixed(6)}`
}
function check(ok: boolean, msg: string): void {
  if (ok) console.log(`    \x1b[32mok\x1b[0m ${msg}`)
  else { console.log(`    \x1b[31mFAIL\x1b[0m ${msg}`); failures++ }
}

async function make(name: string, opts: Parameters<typeof client.sandboxes.create>[0] = {}): Promise<ClientSandbox> {
  const sb = await client.sandboxes.create({
    ...opts,
    metadata: { ...(opts.metadata ?? {}), bench: RUN, scenario: name },
  })
  created.push(sb.id)
  return sb
}

/** Reads the ledger by sandbox id, which keeps working after termination. */
async function ledger(id: string): Promise<UsageReport> {
  return client.usage.report({ sandboxId: id })
}

function record(scenario: string, shape: string, wallRunningS: number, report: UsageReport): Row {
  const t = report.totals
  const row: Row = {
    scenario, shape, wallRunningS,
    billedS: t.durationSeconds, intervals: Number(t.intervals),
    vcpuS: t.vcpuSeconds, mibS: t.memoryMibSeconds, cpuS: t.cpuSeconds,
    usd: usd(t.vcpuSeconds, t.memoryMibSeconds),
  }
  rows.push(row)
  console.log(`    billed ${row.billedS}s over ${row.intervals} interval(s) | ` +
    `${row.vcpuS} vCPU-s, ${row.mibS} MiB-s, ${row.cpuS.toFixed(3)} CPU-s | ${money(row.usd)}`)
  return row
}

// ---------------------------------------------------------------- scenarios

/**
 * The shortest thing a customer can do. A ready-pool create is ~12ms and the
 * ledger stores whole seconds, so this is where per-second billing with no
 * minimum is most visible.
 */
async function ephemeral(): Promise<void> {
  console.log('\n\x1b[1m1. ephemeral — create then terminate immediately\x1b[0m')
  const t0 = Date.now()
  const sb = await make('ephemeral')
  await sb.terminate()
  const wall = (Date.now() - t0) / 1000
  const row = record('ephemeral', '2 vCPU / 1024 MiB', wall, await ledger(sb.id))
  check(row.intervals === 1, `one interval for one VM (got ${row.intervals})`)
  if (row.billedS === 0) {
    findings.push(
      `A create+terminate that held real capacity for ${wall.toFixed(2)}s bills ${money(row.usd)} — ` +
      `whole-second storage floors a sub-second sandbox to zero. Free to the customer, ` +
      `and a burst of them is unbounded free capacity. This is the "rounding and minimums" ` +
      `open question in docs/usage-metering-plan.md, and it is a PRICING decision: the ` +
      `ledger keeps raw microseconds, so a per-create minimum can be applied at invoice time.`)
  }
}

/** A CI-shaped job: seconds, not minutes. Quantization is a big % of it. */
async function shortJob(): Promise<void> {
  console.log('\n\x1b[1m2. short job — 5s of work\x1b[0m')
  const sb = await make('short')
  const t0 = Date.now()
  await sb.commands.run('sleep 5')
  await sb.terminate()
  const wall = (Date.now() - t0) / 1000
  const row = record('short-job', '2 vCPU / 1024 MiB', wall, await ledger(sb.id))
  const driftPct = wall > 0 ? ((row.billedS - wall) / wall) * 100 : 0
  console.log(`    wall ${wall.toFixed(2)}s vs billed ${row.billedS}s (${driftPct.toFixed(1)}%)`)
  check(row.billedS <= Math.ceil(wall) + 2, `billed does not exceed wall clock (${row.billedS} vs ${wall.toFixed(2)})`)
  if (Math.abs(driftPct) > 10) {
    findings.push(
      `A ${wall.toFixed(1)}s sandbox bills ${row.billedS}s (${driftPct.toFixed(0)}%). Second-granularity ` +
      `timestamps are worth ±1s per interval, which is noise on an hour and a fifth of a 5s job.`)
  }
}

/** The honest baseline: a sandbox someone is actually sitting in. */
async function steadyIdle(): Promise<void> {
  console.log('\n\x1b[1m3. steady — 60s running but idle\x1b[0m')
  const sb = await make('steady')
  const t0 = Date.now()
  await sleep(60_000)
  await sb.terminate()
  const wall = (Date.now() - t0) / 1000
  const row = record('steady-idle', '2 vCPU / 1024 MiB', wall, await ledger(sb.id))
  const hourly = usd(row.vcpuS, row.mibS) * (3600 / Math.max(row.billedS, 1))
  console.log(`    implied ${money(hourly)}/hour for this shape`)
  check(Math.abs(row.billedS - wall) <= 3, `billed ${row.billedS}s tracks wall ${wall.toFixed(1)}s within 3s`)
  check(row.cpuS < row.vcpuS * 0.5,
    `an idle guest consumes far less CPU than it is allocated (${row.cpuS.toFixed(2)} of ${row.vcpuS})`)
}

/** A parked devbox: the product story is that pausing is free. Prove it. */
async function pauseGap(): Promise<void> {
  console.log('\n\x1b[1m4. pause/resume — is the parked span really free?\x1b[0m')
  const sb = await make('paused')
  await sleep(5_000)
  const beforePause = Date.now()
  await sb.pause()
  const pausedAt = Date.now()
  await sleep(20_000)
  await sb.resume()
  const resumedAt = Date.now()
  await sleep(5_000)
  await sb.terminate()
  const parkedS = (resumedAt - pausedAt) / 1000
  const runningS = (beforePause - (beforePause - 5_000)) / 1000 + 5
  const row = record('pause-resume', '2 vCPU / 1024 MiB', runningS, await ledger(sb.id))
  console.log(`    parked ${parkedS.toFixed(1)}s between two VMs`)
  check(row.intervals === 2, `two VMs produced two intervals (got ${row.intervals})`)
  check(row.billedS < parkedS + runningS,
    `billed ${row.billedS}s excludes the ${parkedS.toFixed(0)}s parked span`)
  const savedUsd = usd(2 * parkedS, 1024 * parkedS)
  console.log(`    parking saved ${money(savedUsd)} (${money(savedUsd * (3600 / parkedS))}/hour parked)`)
}

/** Pricing must scale with the shape the customer chose, not the host's. */
async function proportionality(): Promise<void> {
  console.log('\n\x1b[1m5. proportionality — 2x vCPU and 4x RAM for the same duration\x1b[0m')
  const small = await make('prop-small')
  const big = await make('prop-big', { resources: { vcpus: 4, memoryMib: 4096 } })
  await Promise.all([small.commands.run('sleep 10'), big.commands.run('sleep 10')])
  await Promise.all([small.terminate(), big.terminate()])
  const [rs, rb] = [await ledger(small.id), await ledger(big.id)]
  const a = record('proportional-small', '2 vCPU / 1024 MiB', 10, rs)
  const b = record('proportional-big', '4 vCPU / 4096 MiB', 10, rb)
  const perSecondSmall = usd(a.vcpuS, a.mibS) / Math.max(a.billedS, 1)
  const perSecondBig = usd(b.vcpuS, b.mibS) / Math.max(b.billedS, 1)
  console.log(`    per running second: small ${money(perSecondSmall)}, big ${money(perSecondBig)} ` +
    `(${(perSecondBig / perSecondSmall).toFixed(2)}x)`)
  check(b.vcpuS / Math.max(b.billedS, 1) === 4, 'big sandbox bills 4 vCPU-seconds per second')
  check(b.mibS / Math.max(b.billedS, 1) === 4096, 'big sandbox bills 4096 MiB-seconds per second')
  // A cold-booted override takes the slow path; the bill must not include it.
  check(b.billedS <= 20, `bring-up is not billed (${b.billedS}s for a ~10s job on the cold path)`)
}

/** The oversubscription margin: identical bills, very different real cost. */
async function marginSpread(): Promise<void> {
  console.log('\n\x1b[1m6. margin — a busy guest and an idle guest bill the same\x1b[0m')
  const busy = await make('margin-busy')
  const idle = await make('margin-idle')
  await Promise.all([
    busy.commands.run('timeout 15 sh -c "while :; do :; done" || true'),
    idle.commands.run('sleep 15'),
  ])
  await Promise.all([busy.terminate(), idle.terminate()])
  const rb = record('margin-busy', '2 vCPU / 1024 MiB', 15, await ledger(busy.id))
  const ri = record('margin-idle', '2 vCPU / 1024 MiB', 15, await ledger(idle.id))
  check(Math.abs(rb.usd - ri.usd) < rb.usd * 0.35, 'both pay for allocation, not consumption')
  const ratio = ri.cpuS > 0.001 ? rb.cpuS / ri.cpuS : Infinity
  console.log(`    consumed CPU differs ${ratio === Infinity ? '>1000' : ratio.toFixed(1)}x for the same bill ` +
    `(${rb.cpuS.toFixed(2)}s vs ${ri.cpuS.toFixed(2)}s)`)
  findings.push(
    `Allocation billing means a busy and an idle sandbox of the same shape pay the same ` +
    `(${money(rb.usd)} vs ${money(ri.usd)}) while consuming ${rb.cpuS.toFixed(1)}s and ` +
    `${ri.cpuS.toFixed(1)}s of host CPU. That spread is the margin cpu_seconds exists to measure, ` +
    `and it is the input a burst SKU would price against.`)
}

/** A TTL reap has to stop the meter where the sandbox stopped. */
async function ttlExpiry(): Promise<void> {
  console.log('\n\x1b[1m7. TTL expiry — the meter stops when the reaper does\x1b[0m')
  const sb = await make('ttl', { ttlMs: 20_000 })
  const t0 = Date.now()
  await sleep(45_000) // TTL + the ~10s reaper tick, with room to spare
  const report = await ledger(sb.id)
  const wall = (Date.now() - t0) / 1000
  const row = record('ttl-expiry', '2 vCPU / 1024 MiB', 20, report)
  const reason = report.intervals[0]?.endReason
  check(reason === 'expire', `end_reason is expire (got ${reason})`)
  check(row.billedS > 0 && row.billedS < wall - 10,
    `billed ${row.billedS}s stops at the reap, not at read time (${wall.toFixed(0)}s later)`)
  check(Number(report.totals.openIntervals) === 0, 'no interval left open by the reaper')
}

/** Idle auto-hibernation is the "park it and forget it" story, unattended. */
async function idleHibernation(): Promise<void> {
  console.log('\n\x1b[1m8. idle auto-hibernation — unattended parking\x1b[0m')
  const sb = await make('idle-hib', { idleTimeoutMs: 15_000 })
  await sleep(50_000) // idle window + reaper tick
  const parked = await ledger(sb.id)
  const first = parked.intervals[0]
  check(first?.state === 'closed', `the idle reaper closed the interval (state=${first?.state})`)
  check(first?.endReason === 'hibernate', `end_reason is hibernate (got ${first?.endReason})`)
  const billedWhileParked = parked.totals.durationSeconds
  await sleep(15_000) // stay parked: this span must cost nothing
  const stillParked = await ledger(sb.id)
  check(stillParked.totals.durationSeconds === billedWhileParked,
    `15s more parked added nothing to the bill (${billedWhileParked}s -> ${stillParked.totals.durationSeconds}s)`)
  await sb.commands.run('echo woke') // wake on access
  await sb.terminate()
  const row = record('idle-hibernation', '2 vCPU / 1024 MiB', 15, await ledger(sb.id))
  check(row.intervals === 2, `wake opened a second interval (got ${row.intervals})`)
}

/** Concurrency must not lose or duplicate a single interval. */
async function churn(n: number): Promise<void> {
  console.log(`\n\x1b[1m9. churn — ${n} concurrent sandboxes\x1b[0m`)
  const boxes = await Promise.all(Array.from({ length: n }, (_, i) => make(`churn-${i}`)))
  await sleep(5_000)
  await Promise.all(boxes.map((b) => b.terminate()))
  let vcpuS = 0, mibS = 0, intervals = 0
  for (const b of boxes) {
    const r = await ledger(b.id)
    vcpuS += r.totals.vcpuSeconds; mibS += r.totals.memoryMibSeconds; intervals += Number(r.totals.intervals)
  }
  console.log(`    ${intervals} interval(s), ${vcpuS} vCPU-s, ${money(usd(vcpuS, mibS))} total`)
  check(intervals === n, `exactly one interval per sandbox (${intervals} for ${n})`)
  rows.push({
    scenario: `churn-x${n}`, shape: '2 vCPU / 1024 MiB', wallRunningS: 5,
    billedS: 0, intervals, vcpuS, mibS, cpuS: 0, usd: usd(vcpuS, mibS),
  })
}

/**
 * The invoice-level invariant: every sandbox this run created appears exactly
 * once in the fleet-wide ledger, with the same money as its per-sandbox read.
 * A fleet read that dropped or duplicated a host would show up here and
 * nowhere else.
 */
async function reconcile(since: Date): Promise<void> {
  console.log('\n\x1b[1m10. reconciliation — fleet ledger vs per-sandbox reads\x1b[0m')
  const byId = new Map<string, { intervals: number; vcpuS: number }>()
  for await (const iv of client.usage.list({ from: since, pageSize: 100 })) {
    if (!created.includes(iv.sandboxId)) continue
    const cur = byId.get(iv.sandboxId) ?? { intervals: 0, vcpuS: 0 }
    byId.set(iv.sandboxId, { intervals: cur.intervals + 1, vcpuS: cur.vcpuS + iv.vcpuSeconds })
  }
  const missing = created.filter((id) => !byId.has(id))
  check(missing.length === 0, `every sandbox this run created is in the fleet ledger (${created.length - missing.length}/${created.length})`)

  let mismatched = 0
  for (const id of created.slice(0, 8)) {
    const direct = await ledger(id)
    const fleet = byId.get(id)
    if (!fleet) continue
    if (Math.abs(fleet.vcpuS - direct.totals.vcpuSeconds) > 0.001 ||
        fleet.intervals !== Number(direct.totals.intervals)) mismatched++
  }
  check(mismatched === 0, `fleet-wide and per-sandbox reads agree on the money (${mismatched} mismatch)`)

  const total = [...byId.values()].reduce((s, v) => s + v.vcpuS, 0)
  console.log(`    this run billed ${total} vCPU-seconds across ${byId.size} sandboxes`)
}

// -------------------------------------------------------------------- main

async function main(): Promise<void> {
  const since = new Date(Date.now() - 60_000)
  console.log(`\x1b[1mUsage pricing benchmark\x1b[0m  run=${RUN}`)
  console.log(`rates (illustrative): $${PRICE_VCPU_HOUR}/vCPU-hour, $${PRICE_GIB_HOUR}/GiB-hour`)

  await ephemeral()
  await shortJob()
  await steadyIdle()
  await pauseGap()
  await proportionality()
  await marginSpread()
  await ttlExpiry()
  await idleHibernation()
  await churn(8)
  await reconcile(since)

  console.log('\n\x1b[1m== per-scenario bill\x1b[0m')
  console.log('scenario              shape                 billed_s  vCPU-s    MiB-s      CPU-s    price')
  for (const r of rows) {
    console.log(
      r.scenario.padEnd(21) + r.shape.padEnd(22) +
      String(r.billedS).padStart(8) + String(r.vcpuS).padStart(9) +
      String(r.mibS).padStart(11) + r.cpuS.toFixed(2).padStart(9) + '  ' + money(r.usd))
  }

  if (findings.length) {
    console.log('\n\x1b[1m== pricing findings\x1b[0m')
    findings.forEach((f, i) => console.log(`${i + 1}. ${f}\n`))
  }
  console.log(failures === 0 ? '\n\x1b[32mall invariants held\x1b[0m' : `\n\x1b[31m${failures} invariant(s) failed\x1b[0m`)
}

main()
  .catch(async (err) => { console.error('\nbenchmark error:', err); failures++ })
  .finally(async () => {
    // Terminate anything a failed scenario left behind. Terminating an
    // already-gone sandbox is expected to 404 and is not a failure.
    for (const id of created) {
      try { await (await client.sandboxes.get(id)).terminate() } catch { /* already gone */ }
    }
    process.exit(failures === 0 ? 0 : 1)
  })
