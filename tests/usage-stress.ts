/**
 * Billing stress and edge-case campaign, driven through the TypeScript SDK
 * against a live fleet.
 *
 * The unit tests prove the ledger's arithmetic and its behaviour under
 * synthetic concurrency. This asks whether the meter is still right when real
 * VMs are involved: when a burst of creates lands at once, when a sandbox is
 * paused and resumed repeatedly, when the TTL reaper and the idle reaper close
 * intervals on their own schedule, when teardown paths race each other for the
 * same sandbox, and when the read paths are hammered while all of that is
 * happening.
 *
 * Every assertion is a property an invoice depends on, and every expectation is
 * MEASURED (wall clock observed by this client, states observed through the
 * API) rather than asserted from a constant — an expectation that cannot
 * disagree with the meter proves nothing.
 *
 * Run it from the CONTROL VM. A laptop tunnel adds hundreds of ms of RTT per
 * call, which lands in the wall-clock column and reads as metering drift.
 *
 *   SANDBOX_API_URL=http://10.160.0.100:9090 SANDBOX_API_KEY=<token> \
 *     tsx usage-stress.ts
 *
 * Optional:
 *   USAGE_STRESS_BURST=24     concurrent creates in the burst scenario
 *   USAGE_STRESS_SCENARIOS=a,b,c   run a subset (default: all)
 *   USAGE_STRESS_OUT=<path>   write the JSON artifact somewhere specific
 */
import { writeFileSync } from 'node:fs'
import {
  SandboxClient,
  type ClientSandbox,
  type UsageReport,
  type UsageIntervalResource,
} from '../sdk/typescript/src/index.js'

const baseUrl = process.env.SANDBOX_API_URL
const apiKey = process.env.SANDBOX_API_KEY
if (!baseUrl || !apiKey) throw new Error('SANDBOX_API_URL and SANDBOX_API_KEY are required')

const RUN = `usagestress-${Date.now().toString(36)}`
const BURST = Number(process.env.USAGE_STRESS_BURST ?? 24)
const ONLY = (process.env.USAGE_STRESS_SCENARIOS ?? '').split(',').map((s) => s.trim()).filter(Boolean)
const OUT = process.env.USAGE_STRESS_OUT ?? `usage-stress-${RUN}.json`

const client = new SandboxClient({ baseUrl, apiKey, maxRetries: 2 })
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

const runStartedAt = new Date()
const created = new Set<string>()
/** Wall-clock windows this client actually observed, per sandbox. */
const life = new Map<string, { bornAt: number; diedAt?: number; frozenMs: number }>()

let failures = 0
let checks = 0
const findings: string[] = []

function check(ok: boolean, msg: string): boolean {
  checks++
  if (ok) console.log(`    \x1b[32mok\x1b[0m ${msg}`)
  else {
    console.log(`    \x1b[31mFAIL\x1b[0m ${msg}`)
    failures++
    findings.push(msg)
  }
  return ok
}
function head(name: string): void {
  console.log(`\n\x1b[1m== ${name}\x1b[0m`)
}
const secs = (ms: number) => ms / 1000

async function make(scenario: string, opts: Parameters<typeof client.sandboxes.create>[0] = {}): Promise<ClientSandbox> {
  const sb = await client.sandboxes.create({
    ...opts,
    metadata: { ...(opts.metadata ?? {}), run: RUN, scenario },
  })
  created.add(sb.id)
  life.set(sb.id, { bornAt: Date.now(), frozenMs: 0 })
  return sb
}

async function kill(sb: ClientSandbox): Promise<void> {
  try {
    await sb.terminate()
  } catch {
    /* already gone */
  }
  const l = life.get(sb.id)
  if (l && l.diedAt === undefined) l.diedAt = Date.now()
}

/** Reads by sandbox id through the fleet-wide route, which survives deletion. */
async function ledger(id: string): Promise<UsageReport> {
  return client.usage.report({ sandboxId: id, pageSize: 100 })
}

/** Every interval the run produced for one sandbox, oldest first. */
async function intervals(id: string): Promise<UsageIntervalResource[]> {
  const report = await ledger(id)
  return [...report.intervals].sort((a, b) => a.sequence - b.sequence)
}

/**
 * The invariants that must hold for any sandbox, whatever happened to it:
 * contiguous sequence numbers, no two intervals overlapping in time, nothing
 * still accruing after teardown, and billed quantities that agree with the
 * resources and duration on the same row.
 */
function assertSandboxInvariants(id: string, ivs: UsageIntervalResource[], opts: { terminated: boolean }): void {
  const label = id.slice(0, 8)
  const seqs = ivs.map((iv) => iv.sequence)
  check(
    seqs.every((s, i) => s === i + 1),
    `${label}: sequences contiguous from 1 (got ${seqs.join(',')}) — a gap loses an interval, a repeat bills twice`,
  )
  check(new Set(ivs.map((iv) => iv.id)).size === ivs.length, `${label}: interval ids unique`)
  if (opts.terminated) {
    check(ivs.every((iv) => iv.state === 'closed'), `${label}: nothing still accruing after termination`)
  }
  for (let i = 1; i < ivs.length; i++) {
    const prev = ivs[i - 1]
    const cur = ivs[i]
    if (!prev.endedAt) continue
    check(
      cur.startedAt.getTime() >= prev.endedAt.getTime(),
      `${label}: interval ${cur.sequence} starts at ${cur.startedAt.toISOString()}, before interval ${prev.sequence} ended at ${prev.endedAt.toISOString()} — two VMs cannot bill the same second`,
    )
  }
  for (const iv of ivs) {
    check(iv.durationSeconds >= 0, `${label}:${iv.sequence} duration is non-negative (${iv.durationSeconds}s)`)
    check(
      Math.abs(iv.vcpuSeconds - iv.resources.vcpus * iv.durationSeconds) < 0.001,
      `${label}:${iv.sequence} vcpu_seconds ${iv.vcpuSeconds} = ${iv.resources.vcpus} vCPU x ${iv.durationSeconds}s`,
    )
    check(
      Math.abs(iv.memoryMibSeconds - iv.resources.memoryMib * iv.durationSeconds) < 0.001,
      `${label}:${iv.sequence} memory_mib_seconds ${iv.memoryMibSeconds} = ${iv.resources.memoryMib} MiB x ${iv.durationSeconds}s`,
    )
    check(iv.resources.vcpus > 0 && iv.resources.memoryMib > 0, `${label}:${iv.sequence} reports effective resources, not "0 = template default"`)
    check(
      iv.cpuSeconds >= 0 && iv.cpuSeconds <= (iv.durationSeconds + 2) * iv.resources.vcpus + 5,
      `${label}:${iv.sequence} consumed CPU ${iv.cpuSeconds.toFixed(3)}s is within the interval's physical ceiling (${iv.durationSeconds}s x ${iv.resources.vcpus} vCPU)`,
    )
  }
}

/** Billed time must never exceed the wall clock this client observed. */
function assertBilledWithinWall(id: string, ivs: UsageIntervalResource[]): void {
  const l = life.get(id)!
  const wall = secs((l.diedAt ?? Date.now()) - l.bornAt)
  const billed = ivs.reduce((n, iv) => n + iv.durationSeconds, 0)
  // +2s of slack: the ledger stores whole seconds, so a create at t=0.9 and a
  // destroy at t=1.1 can legitimately round to 2.
  check(
    billed <= wall + 2,
    `${id.slice(0, 8)}: billed ${billed}s against ${wall.toFixed(1)}s of observed wall clock`,
  )
}

// ------------------------------------------------------------------ scenarios

/**
 * A burst of concurrent creates, each doing a little work, then all torn down
 * at once. This is the shape of ordinary production traffic and the one where a
 * shared ready pool, per-host placement and the create semaphore all interact.
 */
async function scenarioBurst(): Promise<void> {
  head(`a. burst: ${BURST} concurrent create -> exec -> delete`)
  const t0 = Date.now()
  const boxes = await Promise.all(
    Array.from({ length: BURST }, (_, i) => make('burst', { metadata: { index: String(i) } })),
  )
  check(boxes.length === BURST, `${boxes.length}/${BURST} sandboxes created`)
  console.log(`    created in ${secs(Date.now() - t0).toFixed(1)}s`)

  await Promise.all(
    boxes.map(async (sb) => {
      try {
        await sb.commands.run('sh -c "for i in 1 2 3; do echo $i > /dev/null; done"')
      } catch (err) {
        check(false, `${sb.id.slice(0, 8)}: exec failed: ${(err as Error).message}`)
      }
    }),
  )

  // Every sandbox must be billing exactly once WHILE it runs.
  const open = await Promise.all(boxes.map((sb) => ledger(sb.id)))
  const accruing = open.filter((r) => r.totals.openIntervals === 1).length
  check(accruing === BURST, `${accruing}/${BURST} running sandboxes have exactly one open interval`)
  const labelled = open.filter((r) => r.intervals.some((iv) => iv.metadata.run === RUN)).length
  check(
    labelled === BURST,
    `${labelled}/${BURST} intervals carry the sandbox's labels — metadata is the ledger's only attribution, and the v1 API applies it just after the interval opens`,
  )

  await Promise.all(boxes.map(kill))
  await sleep(1500)

  let closed = 0
  for (const sb of boxes) {
    const ivs = await intervals(sb.id)
    if (ivs.length === 1 && ivs[0].endReason === 'destroy' && ivs[0].state === 'closed') closed++
    assertSandboxInvariants(sb.id, ivs, { terminated: true })
    assertBilledWithinWall(sb.id, ivs)
  }
  check(closed === BURST, `${closed}/${BURST} sandboxes billed exactly one closed interval, ended as "destroy"`)
}

/**
 * Pause/resume. Each cycle runs a new VM, so it must bill a new interval — and
 * the frozen span in between must bill nothing at all, which is the property
 * that makes pausing worth anything to a customer.
 */
async function scenarioPauseResume(): Promise<void> {
  head('b. pause/resume: a frozen sandbox bills nothing')
  const CYCLES = 3
  const boxes = await Promise.all(Array.from({ length: 6 }, () => make('pause-resume')))

  const frozen = new Map<string, number>()
  await Promise.all(
    boxes.map(async (sb) => {
      let frozenMs = 0
      for (let c = 0; c < CYCLES; c++) {
        await sleep(1200) // accrue a measurable running span
        const pausedAt = Date.now()
        await sb.pause()
        await sleep(2500) // a span that must NOT be billed
        await sb.resume()
        frozenMs += Date.now() - pausedAt
      }
      frozen.set(sb.id, frozenMs)
      life.get(sb.id)!.frozenMs = frozenMs
    }),
  )
  await Promise.all(boxes.map(kill))
  await sleep(1500)

  for (const sb of boxes) {
    const ivs = await intervals(sb.id)
    const label = sb.id.slice(0, 8)
    check(ivs.length === CYCLES + 1, `${label}: ${ivs.length} intervals for ${CYCLES} pause/resume cycles (want ${CYCLES + 1}: one VM each)`)
    assertSandboxInvariants(sb.id, ivs, { terminated: true })
    assertBilledWithinWall(sb.id, ivs)

    const l = life.get(sb.id)!
    const wall = secs((l.diedAt ?? Date.now()) - l.bornAt)
    const billed = ivs.reduce((n, iv) => n + iv.durationSeconds, 0)
    const frozenS = secs(frozen.get(sb.id) ?? 0)
    check(
      billed <= wall - frozenS * 0.5,
      `${label}: billed ${billed}s of ${wall.toFixed(1)}s wall with ${frozenS.toFixed(1)}s frozen — the frozen span must not be billed`,
    )
    const pauses = ivs.filter((iv) => iv.endReason === 'hibernate').length
    check(pauses === CYCLES, `${label}: ${pauses}/${CYCLES} intervals closed as "hibernate"`)
  }
}

/**
 * The TTL reaper closes an interval on its own schedule, with nobody holding
 * the request. The end reason must say so: "expire" is the answer to a customer
 * asking why a sandbox vanished, and "destroy" would imply someone deleted it.
 */
async function scenarioTTLExpiry(): Promise<void> {
  head('c. TTL expiry: the reaper closes the interval and says why')
  const boxes = await Promise.all(Array.from({ length: 4 }, () => make('ttl', { ttlMs: 20_000 })))
  const deadline = Date.now() + 150_000
  const gone = new Set<string>()
  while (gone.size < boxes.length && Date.now() < deadline) {
    await sleep(5000)
    for (const sb of boxes) {
      if (gone.has(sb.id)) continue
      try {
        await client.sandboxes.get(sb.id)
      } catch {
        gone.add(sb.id)
        life.get(sb.id)!.diedAt = Date.now()
      }
    }
  }
  check(gone.size === boxes.length, `${gone.size}/${boxes.length} sandboxes reaped by their TTL`)

  await sleep(1500)
  for (const sb of boxes) {
    const ivs = await intervals(sb.id)
    const label = sb.id.slice(0, 8)
    check(ivs.length >= 1, `${label}: TTL-reaped sandbox still has a ledger row`)
    if (ivs.length === 0) continue
    check(
      ivs[ivs.length - 1].endReason === 'expire',
      `${label}: last interval ended as "${ivs[ivs.length - 1].endReason}", want "expire"`,
    )
    assertSandboxInvariants(sb.id, ivs, { terminated: true })
    assertBilledWithinWall(sb.id, ivs)
  }
}

/**
 * The idle reaper freezes a sandbox nobody is using. Billing must stop at the
 * freeze — an idle sandbox that keeps billing is the failure mode a customer
 * discovers on an invoice rather than in an API.
 */
async function scenarioIdleHibernate(): Promise<void> {
  head('d. idle hibernation: billing stops when the reaper freezes an idle sandbox')
  const boxes = await Promise.all(Array.from({ length: 3 }, () => make('idle', { idleTimeoutMs: 20_000 })))
  const deadline = Date.now() + 150_000
  const paused = new Set<string>()
  while (paused.size < boxes.length && Date.now() < deadline) {
    await sleep(5000)
    for (const sb of boxes) {
      if (paused.has(sb.id)) continue
      const fresh = await client.sandboxes.get(sb.id)
      if (fresh.status === 'paused') paused.add(sb.id)
    }
  }
  check(paused.size === boxes.length, `${paused.size}/${boxes.length} idle sandboxes frozen by the reaper`)

  const frozenAt = Date.now()
  for (const sb of boxes) {
    const ivs = await intervals(sb.id)
    const label = sb.id.slice(0, 8)
    check(ivs.length === 1 && ivs[0].state === 'closed', `${label}: the freeze closed the interval (${ivs.length} interval(s), state ${ivs[0]?.state})`)
    check(ivs[0]?.endReason === 'hibernate', `${label}: closed as "${ivs[0]?.endReason}", want "hibernate"`)
  }

  // Frozen time must not accrue: re-read after a wait and confirm the numbers
  // did not move.
  const before = await Promise.all(boxes.map((sb) => ledger(sb.id)))
  await sleep(12_000)
  const after = await Promise.all(boxes.map((sb) => ledger(sb.id)))
  for (let i = 0; i < boxes.length; i++) {
    check(
      before[i].totals.durationSeconds === after[i].totals.durationSeconds,
      `${boxes[i].id.slice(0, 8)}: a frozen sandbox billed ${after[i].totals.durationSeconds - before[i].totals.durationSeconds}s more over ${secs(Date.now() - frozenAt).toFixed(0)}s of being paused`,
    )
  }

  // Waking bills again, as a new VM.
  for (const sb of boxes) {
    await sb.resume()
    await sb.commands.run('true')
    const ivs = await intervals(sb.id)
    check(ivs.length === 2 && ivs[1].state === 'open', `${sb.id.slice(0, 8)}: waking opened interval 2 (got ${ivs.length}, last state ${ivs[ivs.length - 1]?.state})`)
  }
  await Promise.all(boxes.map(kill))
}

/**
 * A sandbox created with explicit resources takes the cold path, and the ledger
 * must bill what was ALLOCATED — not the template default it would have got
 * from the ready pool.
 */
async function scenarioResourceOverrides(): Promise<void> {
  head('e. resource overrides are what gets billed')
  const shapes = [
    { vcpus: 1, memoryMib: 512 },
    { vcpus: 2, memoryMib: 2048 },
    { vcpus: 4, memoryMib: 4096 },
  ]
  const boxes = await Promise.all(shapes.map((resources) => make('overrides', { resources })))
  await sleep(2000)
  for (let i = 0; i < boxes.length; i++) {
    const ivs = await intervals(boxes[i].id)
    const want = shapes[i]
    check(
      ivs.length === 1 && ivs[0].resources.vcpus === want.vcpus && ivs[0].resources.memoryMib === want.memoryMib,
      `${boxes[i].id.slice(0, 8)}: billed ${ivs[0]?.resources.vcpus} vCPU / ${ivs[0]?.resources.memoryMib} MiB, allocated ${want.vcpus} / ${want.memoryMib}`,
    )
  }
  await Promise.all(boxes.map(kill))
  await sleep(1200)
  for (let i = 0; i < boxes.length; i++) {
    const ivs = await intervals(boxes[i].id)
    assertSandboxInvariants(boxes[i].id, ivs, { terminated: true })
  }
}

/**
 * Create and destroy as fast as the API allows. These bill nothing (the ledger
 * has whole-second resolution) but they must still produce a real, closed,
 * auditable row — a sandbox that leaves no trace is indistinguishable from one
 * that never ran, and that is how usage goes missing rather than merely free.
 */
async function scenarioSubSecond(): Promise<void> {
  head('f. sub-second sandboxes still leave a row')
  const boxes = await Promise.all(Array.from({ length: 8 }, () => make('subsecond')))
  await Promise.all(boxes.map(kill))
  await sleep(1500)
  let rows = 0
  for (const sb of boxes) {
    const ivs = await intervals(sb.id)
    if (ivs.length === 1) rows++
    assertSandboxInvariants(sb.id, ivs, { terminated: true })
    assertBilledWithinWall(sb.id, ivs)
  }
  check(rows === boxes.length, `${rows}/${boxes.length} sub-second sandboxes produced exactly one ledger row`)
}

/**
 * Teardown paths racing each other for the same sandbox: a pause and a delete
 * issued concurrently. Whichever wins, the ledger must end with one closed
 * interval and nothing accruing — a double close would credit twice, and a
 * missed close would bill forever.
 */
async function scenarioTeardownRace(): Promise<void> {
  head('g. racing teardowns: pause and delete issued together')
  const boxes = await Promise.all(Array.from({ length: 10 }, () => make('teardown-race')))
  await sleep(1500)
  await Promise.all(
    boxes.map(async (sb) => {
      const results = await Promise.allSettled([sb.pause(), sb.terminate()])
      life.get(sb.id)!.diedAt = Date.now()
      // At least one must have succeeded; both succeeding is fine too.
      check(
        results.some((r) => r.status === 'fulfilled'),
        `${sb.id.slice(0, 8)}: at least one of pause/delete succeeded`,
      )
    }),
  )
  await sleep(2500)
  for (const sb of boxes) {
    // A race the delete lost leaves a paused sandbox; clean it up.
    try {
      await client.sandboxes.get(sb.id)
      await (await client.sandboxes.get(sb.id)).terminate()
    } catch {
      /* already gone */
    }
  }
  await sleep(1500)
  for (const sb of boxes) {
    const ivs = await intervals(sb.id)
    assertSandboxInvariants(sb.id, ivs, { terminated: true })
    assertBilledWithinWall(sb.id, ivs)
    check(
      ivs.every((iv) => iv.state === 'closed'),
      `${sb.id.slice(0, 8)}: no interval left accruing after racing teardowns`,
    )
  }
}

/**
 * The read paths under load, while the fleet is churning. Two properties: they
 * must not fail, and a SETTLED selection must not change — a number that drifts
 * with concurrent traffic cannot be reconciled against an invoice.
 */
async function scenarioReadPathsUnderLoad(): Promise<void> {
  head('h. read paths under concurrent churn')
  const settled = await make('read-load')
  await sleep(1500)
  await kill(settled)
  await sleep(1500)
  const baseline = await ledger(settled.id)
  check(baseline.totals.intervals === 1, `settled sandbox has ${baseline.totals.intervals} interval(s)`)

  let churnErrors = 0
  let readErrors = 0
  let drift = 0
  const stopAt = Date.now() + 20_000

  const churn = (async () => {
    while (Date.now() < stopAt) {
      try {
        const sb = await make('read-load-churn')
        await kill(sb)
      } catch {
        churnErrors++
      }
    }
  })()

  const readers = Array.from({ length: 6 }, () =>
    (async () => {
      while (Date.now() < stopAt) {
        try {
          const now = await ledger(settled.id)
          if (
            now.totals.durationSeconds !== baseline.totals.durationSeconds ||
            now.totals.vcpuSeconds !== baseline.totals.vcpuSeconds ||
            now.totals.intervals !== baseline.totals.intervals
          ) {
            drift++
          }
          await client.usage.report({ pageSize: 20, from: runStartedAt })
        } catch {
          readErrors++
        }
      }
    })(),
  )

  await Promise.all([churn, ...readers])
  check(readErrors === 0, `${readErrors} usage reads failed while the fleet churned`)
  check(drift === 0, `a settled sandbox's totals changed ${drift} time(s) under concurrent traffic`)
  if (churnErrors > 0) console.log(`    (note: ${churnErrors} churn creates failed — capacity, not billing)`)
}

/**
 * Money must not depend on pagination, and pagination must be able to reach
 * every interval it counts. A report whose totals include rows no page can
 * reach is a report nobody can audit.
 */
async function scenarioPaginationAndWindows(): Promise<void> {
  head('i. pagination and windows')
  const full = await client.usage.report({ from: runStartedAt, pageSize: 100 })
  const totals = full.totals

  for (const pageSize of [1, 7, 100]) {
    const seen = new Set<string>()
    let pageToken: string | undefined
    let pages = 0
    let stableTotals = true
    do {
      const page = await client.usage.report({ from: runStartedAt, pageSize, pageToken })
      if (page.totals.intervals !== totals.intervals || page.totals.vcpuSeconds !== totals.vcpuSeconds) stableTotals = false
      for (const iv of page.intervals) seen.add(iv.id)
      pageToken = page.nextPageToken
      pages++
    } while (pageToken && pages < 500)
    check(stableTotals, `page_size=${pageSize}: totals identical on every page (the amount owed cannot depend on page size)`)
    check(
      full.coverage.truncated || seen.size === Number(totals.intervals),
      `page_size=${pageSize}: paging reached ${seen.size} of ${totals.intervals} counted intervals`,
    )
  }

  // A window that ends before the run started must select none of our work.
  const past = await client.usage.report({
    from: new Date(runStartedAt.getTime() - 3600_000),
    to: runStartedAt,
    pageSize: 100,
  })
  const leaked = past.intervals.filter((iv) => iv.metadata.run === RUN).length
  check(leaked === 0, `${leaked} of this run's intervals leaked into a window that closed before the run began`)

  // And a window covering the run must select them.
  const covering = await client.usage.report({ from: runStartedAt, to: new Date(Date.now() + 60_000), pageSize: 100 })
  check(covering.totals.intervals > 0, `the run's window selects ${covering.totals.intervals} intervals`)
}

/**
 * Usage outlives the sandbox. The id-scoped route routes by id and therefore
 * cannot answer once the sandbox is gone; the fleet-wide route can, and the
 * error from the first must point at the second.
 */
async function scenarioUsageOutlivesTheSandbox(): Promise<void> {
  head('j. usage survives the sandbox it describes')
  const sb = await make('outlives')
  await sleep(1500)
  await kill(sb)
  await sleep(1500)

  const report = await ledger(sb.id)
  check(report.totals.intervals === 1, `a deleted sandbox is still billable through ?sandbox_id= (${report.totals.intervals} interval(s))`)
  check(report.intervals[0]?.state === 'closed', `its interval is closed`)

  let status = 0
  let detail = ''
  try {
    await client.usage.forSandbox(sb.id)
  } catch (err) {
    status = (err as { status?: number }).status ?? 0
    detail = String((err as Error).message ?? '')
  }
  check(status === 404, `the id-routed usage endpoint answers 404 for a deleted sandbox (got ${status})`)
  check(
    detail.includes('sandbox_id') || detail.toLowerCase().includes('usage'),
    `and its error points at the endpoint that can answer (got: ${detail.slice(0, 120)})`,
  )
}

/**
 * Relabelling. The open interval tracks the sandbox's current labels; a closed
 * one is history and must not be rewritten, because it may already be durable
 * in the billing bucket.
 */
async function scenarioLabels(): Promise<void> {
  head('k. labels: the accruing interval tracks them, closed ones are history')
  const sb = await make('labels', { metadata: { tier: 'first' } })
  await sleep(1500)
  const opening = await intervals(sb.id)
  check(opening[0]?.metadata.tier === 'first', `the first interval carries the labels the sandbox was created with (got ${JSON.stringify(opening[0]?.metadata ?? {})})`)

  await sb.pause()
  await sb.resume()
  await sb.update({ metadata: { run: RUN, scenario: 'labels', tier: 'second' } })
  await sleep(1000)

  const after = await intervals(sb.id)
  check(after.length >= 2, `pause/resume produced ${after.length} intervals`)
  check(after[0]?.metadata.tier === 'first', `the closed interval still says "${after[0]?.metadata.tier}" — a relabel must not rewrite billing history`)
  check(after[after.length - 1]?.metadata.tier === 'second', `the accruing interval picked up the new labels (got "${after[after.length - 1]?.metadata.tier}")`)
  await kill(sb)
}

/**
 * Whole-run reconciliation: every interval this run produced, checked as one
 * ledger, and cross-checked against the host counters exported for operators.
 */
async function reconcileRun(): Promise<Record<string, unknown>> {
  head('reconciliation: the whole run, as one ledger')
  const mine: UsageIntervalResource[] = []
  for (const id of created) {
    try {
      mine.push(...(await intervals(id)))
    } catch (err) {
      check(false, `could not read the ledger for ${id.slice(0, 8)}: ${(err as Error).message}`)
    }
  }

  const ids = new Set(mine.map((iv) => iv.id))
  check(ids.size === mine.length, `${mine.length} intervals, ${ids.size} distinct ids`)
  const stillOpen = mine.filter((iv) => iv.state === 'open')
  check(
    stillOpen.length === 0,
    `${stillOpen.length} intervals still accruing after every sandbox was torn down: ${stillOpen.map((iv) => iv.id).slice(0, 5).join(', ')}`,
  )
  const negatives = mine.filter((iv) => iv.durationSeconds < 0 || iv.vcpuSeconds < 0 || iv.cpuSeconds < 0)
  check(negatives.length === 0, `${negatives.length} intervals with negative quantities`)
  const unattributed = mine.filter((iv) => iv.metadata.run !== RUN)
  check(
    unattributed.length === 0,
    `${unattributed.length}/${mine.length} intervals cannot be attributed to this run through their labels`,
  )

  const billedVcpuS = mine.reduce((n, iv) => n + iv.vcpuSeconds, 0)
  const billedMemS = mine.reduce((n, iv) => n + iv.memoryMibSeconds, 0)
  const cpuS = mine.reduce((n, iv) => n + iv.cpuSeconds, 0)
  console.log(
    `    ${mine.length} intervals over ${created.size} sandboxes | ` +
      `${billedVcpuS.toFixed(0)} vCPU-s, ${(billedMemS / 1024).toFixed(0)} GiB-s billed | ${cpuS.toFixed(1)} CPU-s consumed`,
  )

  // The fleet-wide window must contain everything we just measured per sandbox.
  const fleet = await client.usage.report({ from: runStartedAt, pageSize: 100 })
  check(
    fleet.totals.vcpuSeconds >= billedVcpuS - 1,
    `the fleet-wide window reports ${fleet.totals.vcpuSeconds} vCPU-s, at least the ${billedVcpuS.toFixed(0)} this run's sandboxes billed individually`,
  )
  check(fleet.coverage.hostsReporting >= 1, `${fleet.coverage.hostsReporting} host(s) answered the fleet-wide read`)

  return {
    intervals: mine.length,
    sandboxes: created.size,
    billedVcpuSeconds: billedVcpuS,
    billedMemMibSeconds: billedMemS,
    consumedCpuSeconds: cpuS,
    fleetTotals: fleet.totals,
    hostsReporting: fleet.coverage.hostsReporting,
  }
}


/**
 * Scale: push the ledger past the row cap a single response can hold, then
 * check that the report is still auditable.
 *
 * This is the shape that hid a real defect. Totals are aggregated over the whole
 * selection while rows are paginated, so a caller with more intervals than one
 * response holds sees the right amount owed and — before this was fixed — could
 * never page to the rows behind it. Opt-in (USAGE_STRESS_DEEP=1): it creates and
 * destroys hundreds of sandboxes.
 */
async function scenarioDeepLedger(): Promise<void> {
  const target = Number(process.env.USAGE_STRESS_DEEP_INTERVALS ?? 900)
  const wave = Number(process.env.USAGE_STRESS_DEEP_WAVE ?? 32)
  head(`l. deep ledger: churn ~${target} intervals, then page the whole thing`)

  const t0 = Date.now()
  let churned = 0
  let churnErrors = 0
  while (churned < target) {
    const size = Math.min(wave, target - churned)
    const boxes = await Promise.all(
      Array.from({ length: size }, async () => {
        try {
          return await make('deep')
        } catch {
          churnErrors++
          return null
        }
      }),
    )
    await Promise.all(boxes.filter((sb): sb is ClientSandbox => sb !== null).map(kill))
    churned += size
  }
  console.log(`    churned ${churned} sandboxes in ${secs(Date.now() - t0).toFixed(0)}s (${churnErrors} create failures)`)
  await sleep(2000)

  // The whole fleet ledger, not just this run: the cap applies per response.
  const first = await client.usage.report({ pageSize: 100 })
  console.log(`    fleet ledger holds ${first.totals.intervals} intervals (truncated=${first.coverage.truncated})`)
  check(
    Number(first.totals.intervals) > 1000,
    `the ledger holds ${first.totals.intervals} intervals, enough to exceed a single response's default rows`,
  )

  const seen = new Set<string>()
  let pageToken: string | undefined
  let pages = 0
  let totalsMoved = 0
  do {
    const page = await client.usage.report({ pageSize: 100, pageToken })
    if (page.totals.intervals !== first.totals.intervals) totalsMoved++
    for (const iv of page.intervals) {
      if (seen.has(iv.id)) check(false, `interval ${iv.id} served on two pages`)
      seen.add(iv.id)
    }
    pageToken = page.nextPageToken
    pages++
  } while (pageToken && pages < 200)

  console.log(`    paged ${seen.size} intervals over ${pages} pages`)
  check(seen.size > 1000, `paging reached ${seen.size} intervals, past the ${1000}-row default a single response returns`)
  check(
    first.coverage.truncated || seen.size === Number(first.totals.intervals),
    `paging reached ${seen.size} of the ${first.totals.intervals} intervals the totals count`,
  )
  // Concurrent churn means the ledger legitimately grows while we page; what
  // must not happen is a page reporting DIFFERENT totals for the same window.
  if (totalsMoved > 0) console.log(`    (note: totals moved on ${totalsMoved} page(s) — the fleet was live; the windowed check in scenario i pins stability)`)

  // The same selection, one page: money must not depend on how it was read.
  const single = await client.usage.report({ pageSize: 1 })
  check(
    single.totals.vcpuSeconds >= first.totals.vcpuSeconds,
    `a one-row page reports ${single.totals.vcpuSeconds} vCPU-s against ${first.totals.vcpuSeconds} for a hundred-row page`,
  )
}

// ---------------------------------------------------------------------- main

const scenarios: Array<[string, () => Promise<void>]> = [
  ['a', scenarioBurst],
  ['b', scenarioPauseResume],
  ['c', scenarioTTLExpiry],
  ['d', scenarioIdleHibernate],
  ['e', scenarioResourceOverrides],
  ['f', scenarioSubSecond],
  ['g', scenarioTeardownRace],
  ['h', scenarioReadPathsUnderLoad],
  ['i', scenarioPaginationAndWindows],
  ['j', scenarioUsageOutlivesTheSandbox],
  ['k', scenarioLabels],
  // Opt-in: hundreds of sandboxes.
  ...(process.env.USAGE_STRESS_DEEP ? ([['l', scenarioDeepLedger]] as Array<[string, () => Promise<void>]>) : []),
]

async function cleanup(): Promise<void> {
  head('cleanup')
  let removed = 0
  for (const id of created) {
    try {
      const sb = await client.sandboxes.get(id)
      await sb.terminate()
      removed++
    } catch {
      /* already gone */
    }
  }
  console.log(`    terminated ${removed} straggler(s)`)
}

async function main(): Promise<void> {
  console.log(`\x1b[1musage stress campaign ${RUN}\x1b[0m against ${baseUrl}`)
  const startedAt = Date.now()
  for (const [key, fn] of scenarios) {
    if (ONLY.length && !ONLY.includes(key)) continue
    try {
      await fn()
    } catch (err) {
      check(false, `scenario ${key} threw: ${(err as Error).message}`)
    }
  }
  let summary: Record<string, unknown> = {}
  try {
    summary = await reconcileRun()
  } catch (err) {
    check(false, `reconciliation threw: ${(err as Error).message}`)
  }
  await cleanup()

  const artifact = {
    run: RUN,
    baseUrl,
    startedAt: runStartedAt.toISOString(),
    durationSeconds: secs(Date.now() - startedAt),
    checks,
    failures,
    findings,
    summary,
  }
  writeFileSync(OUT, JSON.stringify(artifact, null, 2))

  console.log(`\n\x1b[1msummary\x1b[0m ${checks - failures}/${checks} checks passed in ${secs(Date.now() - startedAt).toFixed(0)}s`)
  if (failures) {
    console.log('\x1b[31mfindings:\x1b[0m')
    for (const f of findings) console.log(`  - ${f}`)
  }
  console.log(`artifact: ${OUT}`)
  process.exit(failures ? 1 : 0)
}

process.on('SIGINT', async () => {
  await cleanup()
  process.exit(130)
})

main().catch(async (err) => {
  console.error(err)
  await cleanup()
  process.exit(1)
})
