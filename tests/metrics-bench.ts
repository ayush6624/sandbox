/**
 * Per-sandbox utilization benchmark, driven through the TypeScript SDK against
 * a live fleet.
 *
 * The unit tests prove the sampler's arithmetic against synthetic counters.
 * This asks the only question they cannot: do the numbers describe the sandbox
 * a user is actually running? Every scenario therefore CAUSES a known amount of
 * work in the guest and checks that the series moves by the amount that work
 * implies — a metric that agrees with a constant proves nothing, so nothing
 * here is asserted against a hard-coded expectation the sampler could not
 * contradict.
 *
 * It also measures what the feature costs: read latency for the series, and the
 * time between a sandbox existing and its first sample being available.
 *
 * Run it from the CONTROL VM. A laptop tunnel adds hundreds of ms of RTT per
 * call, which lands in the read-latency column and reads as sampler cost.
 *
 *   SANDBOX_API_URL=http://10.160.0.100:9090 SANDBOX_API_KEY=<token> \
 *     tsx metrics-bench.ts
 *
 * Optional:
 *   METRICS_BENCH_FANOUT=8         sandboxes in the concurrent read benchmark
 *   METRICS_BENCH_SCENARIOS=a,b    run a subset (default: all)
 *   METRICS_BENCH_OUT=<path>       where to write the JSON artifact
 */
import { writeFileSync } from 'node:fs'
import { Sandbox, type MetricSample, type SandboxMetrics } from '../sdk/typescript/src/index.js'

const apiUrl = process.env.SANDBOX_API_URL
const apiKey = process.env.SANDBOX_API_KEY
if (!apiUrl || !apiKey) throw new Error('SANDBOX_API_URL and SANDBOX_API_KEY are required')
const opts = { apiUrl, apiKey }

const RUN = `metricsbench-${Date.now().toString(36)}`
const FANOUT = Number(process.env.METRICS_BENCH_FANOUT ?? 8)
const ONLY = (process.env.METRICS_BENCH_SCENARIOS ?? '').split(',').map((s) => s.trim()).filter(Boolean)
const OUT = process.env.METRICS_BENCH_OUT ?? `metrics-bench-${RUN}.json`

const MIB = 1024 * 1024
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

type Finding = { scenario: string; ok: boolean; detail: string }
const findings: Finding[] = []
const measurements: Record<string, unknown> = {}

function check(scenario: string, ok: boolean, detail: string): void {
  findings.push({ scenario, ok, detail })
  console.log(`   ${ok ? '✓' : '✗'} ${detail}`)
}

function pct(values: number[], p: number): number {
  if (values.length === 0) return 0
  const sorted = [...values].sort((a, b) => a - b)
  return sorted[Math.min(sorted.length - 1, Math.floor((p / 100) * sorted.length))]
}

/**
 * Waits until the sandbox has produced at least `n` samples of its CURRENT VM.
 *
 * Filtering by generation matters after a resume: the pre-pause samples are
 * still in the window, and a scenario that differenced across that boundary
 * would be differencing two different VMs' counters.
 *
 * `newerThan` is what makes the wait mean anything after a resume. The window
 * still holds the pre-freeze samples, so without it this returns instantly with
 * stale data and every assertion downstream is made about the OLD VM — which is
 * exactly how the first run of this benchmark reported a generation that had in
 * fact already advanced.
 */
async function samplesAfter(
  sbx: Sandbox,
  n: number,
  timeoutMs = 60_000,
  newerThan = 0,
): Promise<MetricSample[]> {
  const deadline = Date.now() + timeoutMs
  let last: SandboxMetrics | undefined
  while (Date.now() < deadline) {
    last = await sbx.metrics()
    const fresh = last.samples.filter((s) => s.timestamp.getTime() > newerThan)
    const generation = fresh.at(-1)?.vmmGeneration
    const current = fresh.filter((s) => s.vmmGeneration === generation)
    if (current.length >= n) return current
    await sleep(1_000)
  }
  throw new Error(`only ${last?.samples.length ?? 0} sample(s) after ${timeoutMs}ms (state=${last?.state})`)
}

/** Highest cpuUsedPct seen while `work` runs, sampled through the API. */
async function peakDuring(sbx: Sandbox, work: Promise<unknown>): Promise<number> {
  let peak = 0
  let running = true
  void work.finally(() => {
    running = false
  })
  while (running) {
    await sleep(2_000)
    const latest = (await sbx.metrics({ limit: 3 })).samples
    for (const s of latest) peak = Math.max(peak, s.cpuUsedPct)
  }
  await work
  return peak
}

// --- scenarios ---------------------------------------------------------------

/**
 * A sandbox is usable long before its first sample exists, so the API answers
 * with an empty array rather than blocking. That window is a real part of the
 * contract — a client polling for a chart has to tolerate it — so measure it
 * instead of assuming the documented interval.
 */
async function firstSample(): Promise<void> {
  const started = Date.now()
  const sbx = await Sandbox.create({ ...opts, name: `${RUN}-first` })
  const createdAt = Date.now()
  try {
    const immediate = await sbx.metrics()
    check('first-sample', Array.isArray(immediate.samples),
      `a fresh sandbox answers with ${immediate.samples.length} sample(s), not an error`)

    const samples = await samplesAfter(sbx, 1)
    const elapsed = Date.now() - createdAt
    measurements.firstSampleMs = elapsed
    measurements.createMs = createdAt - started
    check('first-sample', elapsed < 30_000,
      `first sample ${elapsed}ms after create (create itself ${createdAt - started}ms)`)
    check('first-sample', samples[0].cpuCount > 0,
      `sample reports allocated vcpus (cpu_count=${samples[0].cpuCount})`)
  } finally {
    await sbx.terminate()
  }
}

/**
 * The headline claim: cpu_used_pct is a percentage of ALLOCATED vCPUs. Burning
 * exactly one core on a 2-vCPU sandbox must therefore read ~50%, not ~100% and
 * not ~25%. Both bounds matter — the wrong denominator fails one of them.
 */
async function cpuTruth(): Promise<void> {
  const sbx = await Sandbox.create({ ...opts, name: `${RUN}-cpu` })
  try {
    const before = await samplesAfter(sbx, 2)
    const idle = before.at(-1)!.cpuUsedPct
    const vcpus = before.at(-1)!.cpuCount
    check('cpu', idle < 15, `idle sandbox reads ${idle.toFixed(1)}% of ${vcpus} vcpus`)

    // One busy core for ~25s, long enough to cover several sampling ticks.
    const burn = sbx.commands.run('timeout 25 sh -c "while :; do :; done" || true', { timeoutMs: 40_000 })
    const oneCore = await peakDuring(sbx, burn)
    const expected = 100 / vcpus
    measurements.cpuOneCorePeak = oneCore
    measurements.cpuOneCoreExpected = expected
    check('cpu', oneCore > expected * 0.7 && oneCore < expected * 1.35,
      `one busy core peaked at ${oneCore.toFixed(1)}%, expected ~${expected.toFixed(0)}% of ${vcpus} allocated vcpus`)

    // Burning every core must approach 100%, which is what pins the ceiling:
    // a sampler dividing by the wrong number passes the test above and fails
    // this one.
    const burnAll = sbx.commands.run(
      `timeout 25 sh -c 'for i in $(seq ${vcpus}); do (while :; do :; done) & done; wait' || true`,
      { timeoutMs: 40_000 },
    )
    const allCores = await peakDuring(sbx, burnAll)
    measurements.cpuAllCoresPeak = allCores
    check('cpu', allCores > 80 && allCores <= 130,
      `${vcpus} busy cores peaked at ${allCores.toFixed(1)}%, expected ~100%`)
    check('cpu', allCores > oneCore * 1.4,
      `saturating all cores (${allCores.toFixed(1)}%) reads meaningfully above one core (${oneCore.toFixed(1)}%)`)

    await sleep(12_000)
    const after = (await sbx.metrics({ limit: 1 })).samples[0]
    check('cpu', after.cpuUsedPct < 25,
      `utilization fell back to ${after.cpuUsedPct.toFixed(1)}% once the load stopped`)

    // cpu_seconds_total is a counter over the VM's life, so it must never go
    // backwards within one generation.
    const series = await samplesAfter(sbx, 3)
    const monotonic = series.every((s, i) => i === 0 || s.cpuSecondsTotal >= series[i - 1].cpuSecondsTotal)
    check('cpu', monotonic, 'cpu_seconds_total is monotonic within a vmm generation')
  } finally {
    await sbx.terminate()
  }
}

/**
 * Disk and network are measured from the host — rootfs blocks and tap counters
 * — so they are the fields most likely to be wired to the wrong thing. Cause a
 * known number of bytes and check the series moves by roughly that number.
 *
 * rootfs_alloc_bytes counts blocks the file occupies INCLUDING those still
 * shared with the golden base, so only its growth is attributable to this
 * sandbox. That is what is asserted.
 */
async function diskAndNetwork(): Promise<void> {
  const sbx = await Sandbox.create({ ...opts, name: `${RUN}-io` })
  try {
    const before = (await samplesAfter(sbx, 2)).at(-1)!

    const writeMib = 256
    await sbx.commands.run(
      `dd if=/dev/zero of=/home/sandbox/blob bs=1M count=${writeMib} conv=fsync 2>&1 | tail -1`,
      { timeoutMs: 120_000 },
    )
    await sleep(12_000)
    const afterWrite = (await sbx.metrics({ limit: 1 })).samples[0]
    const grewMib = (afterWrite.rootfsAllocBytes - before.rootfsAllocBytes) / MIB
    measurements.rootfsGrowthMib = grewMib
    check('disk', grewMib > writeMib * 0.8 && grewMib < writeMib * 1.5,
      `rootfs grew ${grewMib.toFixed(0)} MiB for a ${writeMib} MiB write`)
    if (afterWrite.diskUsedBytes !== undefined && before.diskUsedBytes !== undefined) {
      const guestGrewMib = (afterWrite.diskUsedBytes - before.diskUsedBytes) / MIB
      measurements.guestDiskGrowthMib = guestGrewMib
      check('disk', guestGrewMib > writeMib * 0.8,
        `guest-reported disk_used grew ${guestGrewMib.toFixed(0)} MiB`)
    } else {
      check('disk', true, 'guest disk fields absent (guest stats disabled) — host fields still reported')
    }

    // Pull a known payload from inside the guest. rx is the GUEST's receive
    // direction; a sampler that forgot to invert the tap's counters reports
    // this as tx and fails here.
    const netBefore = (await sbx.metrics({ limit: 1 })).samples[0]
    const downloadMib = 64
    const dl = await sbx.commands.run(
      `curl -sS -o /dev/null -w '%{size_download}' https://speed.cloudflare.com/__down?bytes=${downloadMib * MIB} || echo 0`,
      { timeoutMs: 120_000 },
    )
    const downloaded = Number(dl.stdout.trim()) || 0
    await sleep(12_000)
    const netAfter = (await sbx.metrics({ limit: 1 })).samples[0]
    const rxMib = (netAfter.netRxBytes - netBefore.netRxBytes) / MIB
    const txMib = (netAfter.netTxBytes - netBefore.netTxBytes) / MIB
    measurements.netRxGrowthMib = rxMib
    measurements.netTxGrowthMib = txMib
    if (downloaded < downloadMib * MIB * 0.9) {
      check('network', true, `skipped: guest downloaded only ${(downloaded / MIB).toFixed(1)} MiB (no egress?)`)
    } else {
      check('network', rxMib > downloadMib * 0.8,
        `guest rx grew ${rxMib.toFixed(1)} MiB for a ${downloadMib} MiB download`)
      check('network', rxMib > txMib * 3,
        `rx (${rxMib.toFixed(1)} MiB) dominates tx (${txMib.toFixed(1)} MiB) — counters are the guest's way round`)
    }
  } finally {
    await sbx.terminate()
  }
}

/**
 * Memory is the field most easily mislabeled. host_mem_bytes is the
 * hypervisor's charge — guest pages touched — and without a balloon it does
 * NOT fall when the guest frees memory. mem_used_bytes (guest-reported, when
 * enabled) does. Asserting both directions is what keeps someone from later
 * "fixing" one into the other.
 */
async function memory(): Promise<void> {
  const sbx = await Sandbox.create({ ...opts, name: `${RUN}-mem` })
  try {
    const before = (await samplesAfter(sbx, 2)).at(-1)!
    const allocMib = 384

    // Touch the pages (calloc alone may not fault them in), hold, then free.
    await sbx.commands.run(
      `python3 -c "b=bytearray(${allocMib}*1024*1024); b[::4096]=b'x'*(len(b)//4096); import time; time.sleep(20)"`,
      { timeoutMs: 90_000 },
    )
    const peak = (await sbx.metrics({ limit: 6 })).samples
      .reduce((m, s) => Math.max(m, s.hostMemBytes), 0)
    const grewMib = (peak - before.hostMemBytes) / MIB
    measurements.hostMemGrowthMib = grewMib
    check('memory', grewMib > allocMib * 0.6,
      `host_mem_bytes rose ${grewMib.toFixed(0)} MiB while the guest touched ${allocMib} MiB`)

    await sleep(15_000)
    const after = (await sbx.metrics({ limit: 1 })).samples[0]
    const releasedMib = (peak - after.hostMemBytes) / MIB
    measurements.hostMemReleasedMib = releasedMib
    check('memory', releasedMib < allocMib * 0.5,
      `host_mem_bytes stayed at ${(after.hostMemBytes / MIB).toFixed(0)} MiB after the guest freed ` +
      `(released ${releasedMib.toFixed(0)} MiB) — it is a high-water mark, not live usage`)

    if (after.memUsedBytes !== undefined && after.memTotalBytes !== undefined) {
      measurements.guestMemUsedMib = after.memUsedBytes / MIB
      measurements.guestMemTotalMib = after.memTotalBytes / MIB
      check('memory', after.memUsedBytes < after.memTotalBytes,
        `guest reports ${(after.memUsedBytes / MIB).toFixed(0)}/${(after.memTotalBytes / MIB).toFixed(0)} MiB in use`)
      check('memory', after.memUsedBytes < peak,
        'guest mem_used fell below the host high-water mark once the allocation was freed')
    } else {
      check('memory', true, 'guest memory fields absent (guest stats disabled)')
    }
  } finally {
    await sbx.terminate()
  }
}

/**
 * THE regression this feature could cause. Sampling touches every running
 * sandbox on a timer; if it went through the ordinary agent path it would reset
 * the idle clock and nothing would ever hibernate again — silently, since the
 * sandbox stays perfectly healthy. So: a sandbox with a short idle window and
 * no client traffic must still freeze on schedule, keep its samples, and report
 * a new vmm generation once resumed.
 */
async function hibernationIsUnaffected(): Promise<void> {
  const idleSec = 60
  const sbx = await Sandbox.create({ ...opts, name: `${RUN}-idle`, hibernateAfterMs: idleSec * 1_000 })
  try {
    const before = await samplesAfter(sbx, 2)
    const generation = before.at(-1)!.vmmGeneration

    // Deliberately no traffic of any kind beyond the metrics read itself,
    // which is the thing under test.
    const deadline = Date.now() + (idleSec + 90) * 1_000
    let frozen: SandboxMetrics | undefined
    while (Date.now() < deadline) {
      await sleep(10_000)
      const m = await sbx.metrics()
      if (m.state === 'hibernated') {
        frozen = m
        break
      }
    }
    const froze = frozen !== undefined
    measurements.hibernatedAfterMs = froze ? Date.now() - (deadline - (idleSec + 90) * 1_000) : null
    check('hibernation', froze,
      froze
        ? `sandbox froze on schedule despite being sampled (idle window ${idleSec}s)`
        : `sandbox NEVER froze in ${idleSec + 90}s — sampling is resetting the idle clock`)

    if (froze) {
      check('hibernation', frozen!.samples.length > 0,
        `frozen sandbox kept ${frozen!.samples.length} sample(s) and reads without waking`)

      // Let the series settle before asserting it is quiescent. The status flips
      // to hibernated before the VM is actually gone, so a tick already in
      // flight can still record one truthful sample after the first read that
      // sees "hibernated" — asserting instantaneous equality there measures the
      // freeze boundary, not whether sampling stopped.
      let last = frozen!.samples.at(-1)!.timestamp.getTime()
      let settled = false
      for (let i = 0; i < 6 && !settled; i++) {
        await sleep(8_000)
        const now = (await sbx.metrics()).samples.at(-1)!.timestamp.getTime()
        settled = now === last
        last = now
      }
      check('hibernation', settled, 'the series stopped advancing once the sandbox was frozen')

      // Now it is quiescent: repeated reads over a span longer than the sampling
      // interval must not produce a single new sample, which is what proves the
      // read neither samples nor wakes.
      await sleep(15_000)
      const again = await sbx.metrics()
      check('hibernation', again.samples.at(-1)!.timestamp.getTime() === last,
        `${Math.round(15_000 / 1000)}s of repeated reads on a frozen sandbox added no samples — reads do not wake it`)
      check('hibernation', again.state === 'hibernated',
        'the frozen sandbox is still reported hibernated after being read')
    }

    // Any agent-bound request wakes it; that is a new VM, so the counters
    // restart and the generation must advance. Mark the boundary first — the
    // frozen sandbox's samples are still in the window.
    const wokeAt = Date.now()
    await sbx.commands.run('echo awake', { timeoutMs: 120_000 })
    const after = await samplesAfter(sbx, 1, 90_000, wokeAt)
    measurements.generationBefore = generation
    measurements.generationAfter = after.at(-1)!.vmmGeneration
    check('hibernation', after.at(-1)!.vmmGeneration > generation,
      `vmm_generation advanced ${generation} -> ${after.at(-1)!.vmmGeneration} across the freeze/wake`)
    check('hibernation', after[0].cpuSecondsTotal < before.at(-1)!.cpuSecondsTotal,
      `counters restarted with the new VM (${after[0].cpuSecondsTotal.toFixed(2)}s < ` +
      `${before.at(-1)!.cpuSecondsTotal.toFixed(2)}s before the freeze)`)
  } finally {
    await sbx.terminate()
  }
}

/**
 * What the feature costs to consume: read latency for the series, across a
 * fleet-sized set of sandboxes rather than one. The read is served from the
 * owning worker's memory, so it should stay well inside ordinary API latency —
 * if it does not, the ring is being copied too eagerly somewhere.
 */
async function readLatency(): Promise<void> {
  const boxes = await Promise.all(
    Array.from({ length: FANOUT }, (_, i) =>
      Sandbox.create({ ...opts, name: `${RUN}-read-${i}` })),
  )
  try {
    await Promise.all(boxes.map((b) => samplesAfter(b, 2)))

    const latencies: number[] = []
    for (let round = 0; round < 10; round++) {
      const timed = boxes.map(async (b) => {
        const t0 = Date.now()
        const m = await b.metrics()
        latencies.push(Date.now() - t0)
        return m
      })
      const results = await Promise.all(timed)
      if (round === 0) {
        check('read-latency', results.every((m) => m.samples.length > 0),
          `all ${FANOUT} concurrent sandboxes have samples`)
      }
    }
    const p50 = pct(latencies, 50)
    const p95 = pct(latencies, 95)
    measurements.readP50Ms = p50
    measurements.readP95Ms = p95
    measurements.readSamples = latencies.length
    check('read-latency', p95 < 1_000,
      `metrics read p50 ${p50}ms / p95 ${p95}ms over ${latencies.length} reads across ${FANOUT} sandboxes`)

    // limit=1 is the "current reading" call a dashboard makes per tile, so it
    // must not cost more than the full window.
    const t0 = Date.now()
    const one = await boxes[0].metrics({ limit: 1 })
    measurements.readLimit1Ms = Date.now() - t0
    check('read-latency', one.samples.length === 1,
      `limit=1 returned exactly one sample in ${Date.now() - t0}ms`)
  } finally {
    await Promise.all(boxes.map((b) => b.terminate().catch(() => {})))
  }
}

const SCENARIOS: Record<string, () => Promise<void>> = {
  'first-sample': firstSample,
  cpu: cpuTruth,
  io: diskAndNetwork,
  memory,
  hibernation: hibernationIsUnaffected,
  'read-latency': readLatency,
}

async function main(): Promise<void> {
  const names = Object.keys(SCENARIOS).filter((n) => ONLY.length === 0 || ONLY.includes(n))
  console.log(`metrics benchmark ${RUN} against ${apiUrl}`)
  console.log(`scenarios: ${names.join(', ')}\n`)

  const started = Date.now()
  for (const name of names) {
    console.log(`== ${name}`)
    try {
      await SCENARIOS[name]()
    } catch (err) {
      check(name, false, `threw: ${(err as Error).message}`)
    }
    console.log('')
  }

  const failed = findings.filter((f) => !f.ok)
  const artifact = {
    run: RUN,
    apiUrl,
    startedAt: new Date(started).toISOString(),
    durationMs: Date.now() - started,
    measurements,
    findings,
    passed: findings.length - failed.length,
    failed: failed.length,
  }
  writeFileSync(OUT, JSON.stringify(artifact, null, 2))

  console.log(`${findings.length - failed.length}/${findings.length} checks passed  (artifact: ${OUT})`)
  if (failed.length > 0) {
    for (const f of failed) console.log(`  FAIL ${f.scenario}: ${f.detail}`)
    process.exitCode = 1
  }
}

await main()
