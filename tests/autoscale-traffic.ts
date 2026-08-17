/**
 * Destructive, live-fleet autoscaling correctness benchmark.
 *
 * This is intentionally separate from the normal test runner. It can occupy
 * hundreds of real microVM slots and (for the sawtooth scenario) wait through
 * scale-in. LIVE_AUTOSCALE_BENCHMARK must be set to the exact value below.
 *
 * The important property is that a successful create is not counted as a
 * success by itself. Every held sandbox gets durable in-guest identity state
 * and is repeatedly connected to and executed against while hosts resume,
 * Nomad reconciles allocations, and the gateway changes its routing table.
 *
 * Resource overrides make this a stand-in for a real memory-heavy workload
 * (e.g. a terminal-bench sweep) without running one:
 *
 *   AUTOSCALE_VCPUS=2 AUTOSCALE_MEM_MIB=4096 \
 *   LIVE_AUTOSCALE_BENCHMARK=I_UNDERSTAND_THIS_CREATES_REAL_VMS \
 *   npm run test:autoscale -- scale-in-drain
 *
 * At 4096 MiB a 48-slot host admits ~11 sandboxes, not 48, so scale-out is
 * reached with a fraction of the VMs and the run exercises the memory-admission
 * path that the slot arithmetic mis-sizes. Pair it with a short
 * --scale-in-after-sec on the gateway (and a matching
 * AUTOSCALE_SCALE_IN_AFTER_MS here) to watch a full cordon -> drain -> delete
 * cycle in minutes rather than waiting out the production window.
 */
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { Sandbox } from '../sdk/typescript/src/index.js'

const LIVE_ACK = 'I_UNDERSTAND_THIS_CREATES_REAL_VMS'
const ALLOW_HIBERNATED_BASELINE =
  process.env.AUTOSCALE_ALLOW_HIBERNATED_BASELINE === LIVE_ACK
const API_URL = required('SANDBOX_API_URL')
const API_KEY = required('SANDBOX_API_KEY')
// Worker-control credential (GATEWAY_CONTROL_TOKEN). Only the host-inventory
// probe needs it; every sandbox operation uses the ordinary client key.
const CONTROL_KEY = required('SANDBOX_CONTROL_KEY')
const EXPECTED_RELEASE = required('EXPECTED_WORKER_RELEASE')
const RUN_ID = (process.env.BENCH_RUN_ID ?? `standalone-${process.pid}-${stamp()}`)
  .replace(/[^a-zA-Z0-9-]/g, '-')
  .slice(0, 24)
const MIG_SNAPSHOT = process.env.AUTOSCALE_MIG_SNAPSHOT
const TIMELINE_FILE = process.env.AUTOSCALE_TIMELINE
const TTL_MS = integerEnv('AUTOSCALE_TTL_MS', 45 * 60_000, 60_000)
const REQUEST_TIMEOUT_MS = integerEnv('AUTOSCALE_CREATE_TIMEOUT_MS', 300_000, 10_000)
const PROBE_INTERVAL_MS = integerEnv('AUTOSCALE_PROBE_INTERVAL_MS', 2_000, 100)
const PROBE_CONCURRENCY = integerEnv('AUTOSCALE_PROBE_CONCURRENCY', 48, 1)
const INVARIANT_INTERVAL_MS = integerEnv('AUTOSCALE_INVARIANT_INTERVAL_MS', 500, 100)
const FLOOR_HOSTS = integerEnv('AUTOSCALE_FLOOR_HOSTS', 2, 1)
const SLOTS_PER_HOST = integerEnv('AUTOSCALE_SLOTS_PER_HOST', 48, 1)
const MAX_HOSTS = integerEnv('AUTOSCALE_MAX_HOSTS', 22, FLOOR_HOSTS)
const MAX_LIVE_SANDBOXES = integerEnv('AUTOSCALE_MAX_LIVE_SANDBOXES', 512, 1)
const MAX_CREATE_P95_MS = integerEnv('AUTOSCALE_MAX_CREATE_P95_MS', 30_000, 1)
const MAX_CREATE_MS = integerEnv('AUTOSCALE_MAX_CREATE_MS', 60_000, MAX_CREATE_P95_MS)
// Per-sandbox resource overrides, so a burst can be made MEMORY-bound rather
// than slot-bound. That is the regime real workloads land in, and the one the
// slot arithmetic gets wrong: with MEM_PER_SLOT_MIB=1180 a 48-slot host
// advertises 48 slots but admits only ~11 sandboxes at 4096 MiB (measured
// 2026-08-16 — three hosts at 99.2% of a 56,640 MiB budget while running
// 11/9/11 sandboxes).
//
// It also makes the benchmark much cheaper: forcing scale-out costs ~11 creates
// per host instead of 48, so a full multi-host cycle needs a fraction of the VMs
// and boots. Setting either forces a cold boot (snapshots bake vcpus/mem), which
// is worth exercising in its own right — the ready pool cannot serve these, so
// this is also the path that has no warm fast lane.
const VCPUS = integerEnv('AUTOSCALE_VCPUS', 0, 0)
const MEM_MIB = integerEnv('AUTOSCALE_MEM_MIB', 0, 0)
// How long the gateway must see low demand before it cordons a host, mirroring
// the gateway's --scale-in-after-sec. Drive a run with a short value to exercise
// scale-in without waiting out the production window.
const SCALE_IN_AFTER_MS = integerEnv('AUTOSCALE_SCALE_IN_AFTER_MS', 600_000, 1_000)
const OUTPUT = process.env.AUTOSCALE_OUTPUT ??
  resolve(dirname(fileURLToPath(import.meta.url)), 'results', `autoscale-traffic-${stamp()}.json`)

interface RawHost {
  id: string
  addr: string
  slots_total: number
  slots_used: number
  hibernated: number
  free: number
  warm_ready?: number
  alive: boolean
  last_seen_ms_ago: number
  release?: string
  release_compatible?: boolean
}

interface RawSandbox {
  id: string
  host_addr?: string
  status?: string
}

interface Held {
  sandbox: Sandbox
  marker: string
  scenario: string
  createdAt: number
  host: string
  probes: number
}

interface Failure {
  at: string
  scenario: string
  operation: string
  sandboxId?: string
  message: string
}

interface ScenarioResult {
  name: string
  startedAt: string
  wallMs: number
  creates: number
  createLatencyMs: number[]
  probes: number
  failures: Failure[]
  hostPeak: number
}

interface Scenario {
  name: string
  run(ctx: ScenarioContext): Promise<void>
}

interface TimelineEvent {
  ts_ms: number
  event: string
  data: Record<string, unknown>
}

class ScenarioContext {
  readonly held = new Map<string, Held>()
  readonly failures: Failure[] = []
  readonly createLatencyMs: number[] = []
  probes = 0
  creates = 0
  hostPeak = 0
  private stopping = false
  private monitor?: Promise<void>
  private createsInFlight = 0

  constructor(readonly scenario: string) {}

  startMonitor(): void {
    this.monitor = this.monitorLoop()
  }

  async stopMonitor(): Promise<void> {
    this.stopping = true
    await this.monitor
  }

  async create(label: string): Promise<Held> {
    this.assertHealthy()
    if (this.held.size + this.createsInFlight >= MAX_LIVE_SANDBOXES) {
      throw new Error(
        `${this.scenario}: refusing more than ${MAX_LIVE_SANDBOXES} simultaneously live creates`
      )
    }
    this.createsInFlight++
    try {
      return await this.createTracked(label)
    } finally {
      this.createsInFlight--
    }
  }

  private async createTracked(label: string): Promise<Held> {
    const started = Date.now()
    const sandbox = await Sandbox.create({
      apiUrl: API_URL,
      apiKey: API_KEY,
      timeoutMs: TTL_MS,
      requestTimeoutMs: REQUEST_TIMEOUT_MS,
      hibernateAfterMs: -1,
      name: `autoscale-${RUN_ID}-${this.scenario}-${label}`.slice(0, 63),
      // Omitted entirely when zero: the API treats absent as "template default",
      // and sending an explicit 0 is not the same thing.
      ...(VCPUS > 0 ? { vcpus: VCPUS } : {}),
      ...(MEM_MIB > 0 ? { memMib: MEM_MIB } : {}),
    })
    this.createLatencyMs.push(Date.now() - started)
    this.creates++
    const marker = `${this.scenario}:${label}:${sandbox.sandboxId}:${Date.now()}`
    const held: Held = {
      sandbox,
      marker,
      scenario: this.scenario,
      createdAt: Date.now(),
      host: sandbox.info.hostAddr ?? '',
      probes: 0,
    }
    this.held.set(sandbox.sandboxId, held)
    this.assertHealthy()
    try {
      const result = await sandbox.commands.run(
        `umask 077; printf '%s' '${shellSingle(marker)}' > /tmp/autoscale-marker; echo initialized`
      )
      if (result.stdout.trim() !== 'initialized') throw new Error(`unexpected init output: ${result.stdout}`)
    } catch (error) {
      this.record('initialize', error, held)
      throw error
    }
    return held
  }

  async createMany(count: number, prefix: string, concurrency = count): Promise<Held[]> {
    const settled = await mapLimit(count, concurrency, async (i) => {
      try {
        return await this.create(`${prefix}-${i}`)
      } catch (error) {
        this.record('create', error)
        return undefined
      }
    })
    const made = settled.filter((held): held is Held => held !== undefined)
    if (made.length !== count) {
      throw new Error(`${this.scenario}: ${count - made.length}/${count} creates failed`)
    }
    this.assertHealthy()
    return made
  }

  trackSnapshotCreate(sandbox: Sandbox, marker: string, latencyMs: number): Held {
    if (this.held.size >= MAX_LIVE_SANDBOXES) {
      throw new Error(
        `${this.scenario}: refusing more than ${MAX_LIVE_SANDBOXES} simultaneously live creates`
      )
    }
    this.createLatencyMs.push(latencyMs)
    this.creates++
    const held: Held = {
      sandbox,
      marker,
      scenario: this.scenario,
      createdAt: Date.now(),
      host: sandbox.info.hostAddr ?? '',
      probes: 0,
    }
    this.held.set(sandbox.sandboxId, held)
    return held
  }

  async probe(held: Held): Promise<void> {
    this.probes++
    held.probes++
    try {
      // Connect exercises GET routing independently from the handle retained
      // by create; exec then exercises POST routing and the guest identity.
      const connected = await Sandbox.connect(held.sandbox.sandboxId, {
        apiUrl: API_URL,
        apiKey: API_KEY,
      })
      const result = await connected.commands.run(
        `test "$(cat /tmp/autoscale-marker)" = '${shellSingle(held.marker)}' && echo route-ok`,
        { timeoutMs: 15_000 }
      )
      if (result.stdout.trim() !== 'route-ok') {
        throw new Error(`identity mismatch; stdout=${JSON.stringify(result.stdout)}`)
      }
    } catch (error) {
      this.record('connect+exec', error, held)
    }
  }

  async probeAll(): Promise<void> {
    await mapItems([...this.held.values()], PROBE_CONCURRENCY, (held) => this.probe(held))
    this.assertHealthy()
  }

  async holdAndProbe(ms: number): Promise<void> {
    const deadline = Date.now() + ms
    do {
      await this.probeAll()
      if (Date.now() < deadline) await sleep(Math.min(PROBE_INTERVAL_MS, deadline - Date.now()))
    } while (Date.now() < deadline)
  }

  async kill(held: Held): Promise<void> {
    // Drop from `held` BEFORE issuing the delete, not after. The invariant
    // monitor runs concurrently and asserts that every id still in `held` is
    // present in GET /sandboxes, so a delete that completes while the id is
    // still in the map is a guaranteed false failure. Deleting first made
    // create-exec-kill-churn report
    // "held sandbox <id> disappeared from gateway list" on 2026-07-28 after 229
    // successful creates — the sandbox had been deleted by this very call
    // (DELETE ... 204, one second after two successful execs). At 360 churn
    // iterations and concurrency 96 the window is near-certain to be hit.
    this.held.delete(held.sandbox.sandboxId)
    try {
      await held.sandbox.terminate()
    } catch (error) {
      // Put it back so killAll() still retries it during cleanup.
      this.held.set(held.sandbox.sandboxId, held)
      this.record('kill', error, held)
    }
  }

  async killAll(): Promise<void> {
    for (let attempt = 0; attempt < 5 && this.held.size; attempt++) {
      await mapItems([...this.held.values()], 48, (held) => this.kill(held))
      if (this.held.size) await sleep(500)
    }
  }

  heldCount(): number {
    return this.held.size
  }

  /**
   * Release everything except `keep` sandboxes, which stay live and probed.
   * Scale-in has to be observed with something still running: a fleet that is
   * entirely empty cannot show whether the controller drained a host or simply
   * deleted one out from under a tenant.
   */
  async killAllBut(keep: number): Promise<void> {
    const doomed = [...this.held.values()].slice(keep)
    for (let attempt = 0; attempt < 5 && doomed.some((h) => this.held.has(h.sandbox.sandboxId)); attempt++) {
      await mapItems(
        doomed.filter((h) => this.held.has(h.sandbox.sandboxId)),
        48,
        (held) => this.kill(held)
      )
      if (doomed.some((h) => this.held.has(h.sandbox.sandboxId))) await sleep(500)
    }
  }

  async abortAndCleanup(): Promise<void> {
    this.stopping = true
    const deadline = Date.now() + REQUEST_TIMEOUT_MS + 5_000
    while (this.createsInFlight > 0 && Date.now() < deadline) await sleep(100)
    await this.killAll()
  }

  record(operation: string, error: unknown, held?: Held): void {
    const failure: Failure = {
      at: new Date().toISOString(),
      scenario: this.scenario,
      operation,
      message: String((error as Error)?.message ?? error).slice(0, 500),
    }
    if (held) failure.sandboxId = held.sandbox.sandboxId
    this.failures.push(failure)
    console.error(`  FAIL ${operation}${held ? ` ${held.sandbox.sandboxId}` : ''}: ${failure.message}`)
  }

  assertHealthy(): void {
    if (interrupted) throw new Error(`${this.scenario}: benchmark interrupted`)
    if (this.failures.length) {
      throw new Error(
        `${this.scenario}: stopping after first correctness failure: ` +
        `${this.failures[0].operation}: ${this.failures[0].message}`
      )
    }
  }

  private async monitorLoop(): Promise<void> {
    while (!this.stopping) {
      try {
        const hosts = await getHosts()
        this.hostPeak = Math.max(this.hostPeak, hosts.filter((host) => host.alive).length)
        assertHostInvariants(hosts)
        const listed = await getSandboxes()
        const duplicates = duplicateIds(listed.map((sandbox) => sandbox.id))
        if (duplicates.length) throw new Error(`duplicate sandbox routes: ${duplicates.join(',')}`)
        for (const held of this.held.values()) {
          const routed = listed.find((sandbox) => sandbox.id === held.sandbox.sandboxId)
          if (!routed) throw new Error(`held sandbox ${held.sandbox.sandboxId} disappeared from gateway list`)
          if (held.host && routed.host_addr && routed.host_addr !== held.host) {
            throw new Error(
              `route changed for ${held.sandbox.sandboxId}: ${held.host} -> ${routed.host_addr}`
            )
          }
        }
      } catch (error) {
        this.record('invariant-monitor', error)
        this.stopping = true
      }
      await sleep(INVARIANT_INTERVAL_MS)
    }
  }
}

const heldBurst: Scenario = {
  name: 'held-burst',
  async run(ctx) {
    const count = integerEnv('AUTOSCALE_HELD_BURST', FLOOR_HOSTS * SLOTS_PER_HOST + 64, 1)
    await ctx.createMany(count, 'burst')
    await ctx.holdAndProbe(integerEnv('AUTOSCALE_HELD_HOLD_MS', 45_000, 1_000))
  },
}

const standbyRefillBoundary: Scenario = {
  name: 'standby-refill-boundary',
  async run(ctx) {
    if (!TIMELINE_FILE) {
      throw new Error('AUTOSCALE_TIMELINE is required for standby refill lifecycle proof')
    }
    const started = Date.now()
    const count = integerEnv(
      'AUTOSCALE_REFILL_PRESSURE',
      FLOOR_HOSTS * SLOTS_PER_HOST + 64,
      1
    )
    await ctx.createMany(count, 'refill')
    await ctx.holdAndProbe(integerEnv('AUTOSCALE_REFILL_HOLD_MS', 240_000, 181_000))
    const placementDelayMs = integerEnv('AUTOSCALE_PLACEMENT_DELAY_MS', 210_000, 1_000)
    const settleDeadline =
      Date.now() + integerEnv('AUTOSCALE_REFILL_SETTLE_MS', 240_000, 1_000)
    for (;;) {
      ctx.assertHealthy()
      const pending = pendingStandbyRefillSuspensions(
        readTimeline(TIMELINE_FILE),
        started,
        placementDelayMs
      )
      if (!pending.length) break
      if (Date.now() >= settleDeadline) {
        throw new Error(
          `fresh refills did not reach SUSPENDED before settle deadline: ${pending.join(', ')}`
        )
      }
      await ctx.holdAndProbe(Math.min(5_000, settleDeadline - Date.now()))
    }
  },
}

const gradualRamp: Scenario = {
  name: 'gradual-ramp',
  async run(ctx) {
    const total = integerEnv('AUTOSCALE_RAMP_TOTAL', FLOOR_HOSTS * SLOTS_PER_HOST + 72, 1)
    const batch = integerEnv('AUTOSCALE_RAMP_BATCH', 12, 1)
    const interval = integerEnv('AUTOSCALE_RAMP_INTERVAL_MS', 3_000, 100)
    for (let start = 0; start < total; start += batch) {
      await ctx.createMany(Math.min(batch, total - start), `ramp-${start}`, batch)
      await ctx.probeAll()
      if (start + batch < total) await sleep(interval)
    }
    await ctx.holdAndProbe(integerEnv('AUTOSCALE_RAMP_HOLD_MS', 30_000, 1_000))
  },
}

const secondWave: Scenario = {
  name: 'second-wave',
  async run(ctx) {
    const first = integerEnv('AUTOSCALE_SECOND_WAVE_FIRST', FLOOR_HOSTS * SLOTS_PER_HOST + 16, 1)
    const second = integerEnv('AUTOSCALE_SECOND_WAVE_SECOND', SLOTS_PER_HOST * 2, 1)
    const gap = integerEnv('AUTOSCALE_SECOND_WAVE_GAP_MS', 2_000, 100)
    const firstPromise = ctx.createMany(first, 'wave-1')
    await sleep(gap)
    const secondPromise = ctx.createMany(second, 'wave-2')
    await Promise.all([firstPromise, secondPromise])
    await ctx.holdAndProbe(integerEnv('AUTOSCALE_SECOND_WAVE_HOLD_MS', 45_000, 1_000))
  },
}

const longLived: Scenario = {
  name: 'long-lived-reconcile',
  async run(ctx) {
    const anchors = await ctx.createMany(
      integerEnv('AUTOSCALE_LONG_LIVED_ANCHORS', Math.max(8, Math.floor(SLOTS_PER_HOST / 2)), 1),
      'anchor'
    )
    await ctx.holdAndProbe(integerEnv('AUTOSCALE_ANCHOR_SETTLE_MS', 5_000, 1_000))
    const pressureCount = integerEnv(
      'AUTOSCALE_LONG_LIVED_PRESSURE',
      FLOOR_HOSTS * SLOTS_PER_HOST + SLOTS_PER_HOST,
      1
    )
    let pressureDone = false
    const pressure = ctx.createMany(pressureCount, 'pressure')
    void pressure.then(
      () => { pressureDone = true },
      () => { pressureDone = true }
    )
    while (!pressureDone) {
      await mapItems(anchors, PROBE_CONCURRENCY, (held) => ctx.probe(held))
      await sleep(PROBE_INTERVAL_MS)
    }
    await pressure
    await ctx.holdAndProbe(integerEnv('AUTOSCALE_RECONCILE_HOLD_MS', 45_000, 1_000))
  },
}

// Snapshot operations used to bypass the fleet queue and stay pinned to a
// full snapshot owner. A fanout then 503'd even when another worker had room,
// and its N clones were not reserved atomically. Exercise that path under the
// same scale-out pressure as ordinary creates, then explicitly pause/resume the
// clones while unrelated sandboxes remain busy.
const snapshotFanoutResume: Scenario = {
  name: 'snapshot-fanout-resume',
  async run(ctx) {
    const opts = {
      apiUrl: API_URL,
      apiKey: API_KEY,
      timeoutMs: TTL_MS,
      requestTimeoutMs: REQUEST_TIMEOUT_MS,
      hibernateAfterMs: -1,
    }
    let snapshotId = ''
    try {
      const source = await ctx.create('snapshot-source')
      await source.sandbox.files.write('/home/sandbox/snapshot-seed', 'fleet-snapshot-state')
      const snapshot = await source.sandbox.snapshot({
        name: `autoscale-${RUN_ID}-fleet-snapshot`.slice(0, 63),
      })
      snapshotId = snapshot.snapshotId

      // Explicit identity-preserving hibernate/resume before baking traffic
      // from the snapshot. The source remains tracked and independently probed.
      await source.sandbox.pause()
      await source.sandbox.resume()
      await ctx.probe(source)
      ctx.assertHealthy()
      await ctx.kill(source)
      ctx.assertHealthy()

      // Fill the floor so the snapshot owner cannot satisfy the fanout. Start
      // ordinary pressure just behind the fanout request: the fanout must hold
      // one atomic N-slot reservation, queue, and cause scale-out rather than
      // being fragmented or rejected while another worker comes online.
      const anchors = await ctx.createMany(
        integerEnv('AUTOSCALE_SNAPSHOT_ANCHORS', FLOOR_HOSTS * SLOTS_PER_HOST, 1),
        'snapshot-anchor'
      )
      const fanoutCount = integerEnv('AUTOSCALE_SNAPSHOT_FANOUT', 8, 1)
      const fanoutStarted = Date.now()
      const fanoutPromise = Sandbox.fanout(snapshotId, fanoutCount, opts)
      await sleep(integerEnv('AUTOSCALE_SNAPSHOT_MIXED_DELAY_MS', 250, 1))
      const mixedPromise = ctx.createMany(
        integerEnv('AUTOSCALE_SNAPSHOT_MIXED_CREATES', Math.max(8, Math.floor(SLOTS_PER_HOST / 2)), 1),
        'snapshot-mixed'
      )
      const [clones, mixed] = await Promise.all([fanoutPromise, mixedPromise])
      const fanoutLatency = Date.now() - fanoutStarted
      for (const clone of clones) {
        ctx.trackSnapshotCreate(clone, source.marker, fanoutLatency)
      }
      if (clones.length !== fanoutCount) {
        throw new Error(`snapshot fanout returned ${clones.length}/${fanoutCount} clones`)
      }

      await mapItems(clones, fanoutCount, async (clone) => {
        const state = await clone.files.read('/home/sandbox/snapshot-seed')
        if (state !== 'fleet-snapshot-state') {
          throw new Error(`fanout clone ${clone.sandboxId} lost prepared disk state`)
        }
      })

      await mapItems(clones, fanoutCount, (clone) => clone.pause())
      await Promise.all([
        mapItems(clones, fanoutCount, (clone) => clone.resume()),
        mapItems([...anchors, ...mixed].slice(0, 24), 24, (held) => ctx.probe(held)),
      ])
      await ctx.probeAll()

      const restoreStarted = Date.now()
      const restored = await Sandbox.restore(snapshotId, opts)
      const restoredHeld = ctx.trackSnapshotCreate(
        restored,
        source.marker,
        Date.now() - restoreStarted
      )
      await ctx.probe(restoredHeld)
      ctx.assertHealthy()
    } finally {
      await ctx.killAll()
      if (snapshotId) {
        try {
          await Sandbox.deleteSnapshot(snapshotId, { apiUrl: API_URL, apiKey: API_KEY })
        } catch (error) {
          ctx.record('snapshot-delete', error)
        }
      }
    }
  },
}

const churn: Scenario = {
  name: 'create-exec-kill-churn',
  async run(ctx) {
    const count = integerEnv('AUTOSCALE_CHURN_COUNT', 360, 1)
    const concurrency = integerEnv('AUTOSCALE_CHURN_CONCURRENCY', SLOTS_PER_HOST * 2, 1)
    await mapLimit(count, concurrency, async (i) => {
      let held: Held | undefined
      try {
        held = await ctx.create(`churn-${i}`)
        await ctx.probe(held)
        await ctx.probe(held)
      } catch (error) {
        ctx.record('churn-lifecycle', error, held)
      } finally {
        if (held) await ctx.kill(held)
      }
      return undefined
    })
  },
}

const sawtooth: Scenario = {
  name: 'sawtooth-scale-cycle',
  async run(ctx) {
    const cycles = integerEnv('AUTOSCALE_SAWTOOTH_CYCLES', 3, 2)
    const burst = integerEnv('AUTOSCALE_SAWTOOTH_BURST', FLOOR_HOSTS * SLOTS_PER_HOST + 64, 1)
    // SCALE_DOWN_WINDOW is 15m, but its clock can be reset by a late scale-up
    // action and the ensuing GCE/Nomad/gateway reconciliation needs additional
    // time after the target changes. The first live run proved 16m from
    // sandbox cleanup was too short: targetSize had reached 2, but host
    // liveness had not converged. Keep seven minutes of control-plane headroom.
    const waitMs = integerEnv('AUTOSCALE_SCALE_IN_WAIT_MS', 22 * 60_000, 1_000)
    for (let cycle = 0; cycle < cycles; cycle++) {
      const before = (await getHosts()).filter((host) => host.alive).length
      await ctx.createMany(burst, `cycle-${cycle}`)
      await ctx.holdAndProbe(integerEnv('AUTOSCALE_SAWTOOTH_HOLD_MS', 30_000, 1_000))
      const peak = (await getHosts()).filter((host) => host.alive).length
      if (peak <= before) throw new Error(`cycle ${cycle}: no scale-out observed (${before} -> ${peak})`)
      await ctx.killAll()
      let stableSince = 0
      await waitFor(
        async () => {
          const alive = (await getHosts()).filter((host) => host.alive).length
          const mig = getMigSnapshot()
          const atFloor = alive === FLOOR_HOSTS &&
            mig.target_size === FLOOR_HOSTS &&
            mig.running === FLOOR_HOSTS &&
            mig.transitioning === 0 &&
            Date.now() - mig.ts_ms < 15_000
          if (!atFloor) {
            stableSince = 0
            return false
          }
          if (!stableSince) stableSince = Date.now()
          return Date.now() - stableSince >=
            integerEnv('AUTOSCALE_SCALE_IN_STABLE_MS', 30_000, 1_000)
        },
        waitMs,
        5_000,
        `stable physical and gateway scale-in to ${FLOOR_HOSTS} hosts after cycle ${cycle}`,
        () => ctx.assertHealthy()
      )
      await assertCleanGateway()
    }
  },
}

/**
 * Exercises gateway-owned scale-in end to end: grow past the floor, release the
 * load, and require the fleet to come back to the floor THROUGH a cordon and a
 * drain rather than by deleting a busy host.
 *
 * The assertions are about mechanism, not just the final count. The old Nomad
 * autoscaler also returned the fleet to MIG_MIN — it just did so by purging
 * whichever instance GCE picked, which on 2026-08-16 was the host the gateway
 * had added seconds earlier, killing 11 running trials. A test that only checked
 * "we ended at 2 hosts" passed happily through that. So this one requires:
 *
 *   - a cordon actually happened (sandbox_scale_in_cordons_total advanced), and
 *   - no sandbox died while it happened (every held sandbox stays reachable
 *     across the whole cycle, which holdAndProbe enforces), and
 *   - the removal was a drain, not a purge (removed advanced, and the fleet
 *     never dipped below the floor).
 */
const scaleInDrain: Scenario = {
  name: 'scale-in-drain',
  async run(ctx) {
    const before = await getGatewayMetrics()
    const cordonsBefore = metric(before, 'sandbox_scale_in_cordons_total')
    const removedBefore = metric(before, 'sandbox_scale_in_removed_total')

    // Grow past the floor. With memory overrides this is a small burst.
    const burst = integerEnv('AUTOSCALE_DRAIN_BURST', (FLOOR_HOSTS + 1) * perHostCapacity(), 1)
    await ctx.createMany(burst, 'drain')
    await waitFor(
      async () => (await getHosts()).filter((host) => host.alive).length > FLOOR_HOSTS,
      10 * 60_000,
      5_000,
      `scale-out past the floor of ${FLOOR_HOSTS} hosts`,
      () => ctx.assertHealthy()
    )
    const peak = (await getHosts()).filter((host) => host.alive).length

    // Keep a few sandboxes alive across the whole drain. The cordon must not
    // touch them, and the host holding them must not be deleted under them.
    const survivors = Math.min(ctx.heldCount(), integerEnv('AUTOSCALE_DRAIN_SURVIVORS', 4, 1))
    await ctx.killAllBut(survivors)

    // Demand is now low. Wait out the gateway's window plus its 30s evaluation
    // tick, then require evidence of the mechanism.
    await waitFor(
      async () => metric(await getGatewayMetrics(), 'sandbox_scale_in_cordons_total') > cordonsBefore,
      SCALE_IN_AFTER_MS + 5 * 60_000,
      5_000,
      'the gateway to cordon a host for draining',
      () => ctx.assertHealthy()
    )

    await waitFor(
      async () => {
        const alive = (await getHosts()).filter((host) => host.alive).length
        const now = await getGatewayMetrics()
        return alive === FLOOR_HOSTS &&
          metric(now, 'sandbox_scale_in_removed_total') > removedBefore &&
          metric(now, 'sandbox_hosts_draining') === 0
      },
      SCALE_IN_AFTER_MS + 15 * 60_000,
      5_000,
      `drained scale-in back to ${FLOOR_HOSTS} hosts with no host left cordoned`,
      () => ctx.assertHealthy()
    )

    // The survivors must have lived through the entire cycle. This is the
    // property the old autoscaler violated.
    await ctx.holdAndProbe(integerEnv('AUTOSCALE_DRAIN_VERIFY_MS', 15_000, 1_000))
    const after = await getGatewayMetrics()
    console.log(
      `   scale-in: ${peak} -> ${FLOOR_HOSTS} hosts, ` +
        `cordons +${metric(after, 'sandbox_scale_in_cordons_total') - cordonsBefore}, ` +
        `removed +${metric(after, 'sandbox_scale_in_removed_total') - removedBefore}, ` +
        `${survivors} sandbox(es) survived`
    )
    await ctx.killAll()
  },
}

/**
 * Sandboxes one host can hold. With a memory override this is the memory
 * admission bound, which is what actually limits density — the slot count
 * overstates it by ~4x for a 4 GiB sandbox.
 */
function perHostCapacity(): number {
  if (MEM_MIB <= 0) return SLOTS_PER_HOST
  const budgetMib = integerEnv('AUTOSCALE_MEM_BUDGET_MIB', SLOTS_PER_HOST * 1180, 1)
  const perSandbox = MEM_MIB + 156 // VMM overhead, matching the registry's admission check
  return Math.max(1, Math.floor(budgetMib / perSandbox))
}

// Sawtooth must start from the preflight floor. Other scenarios intentionally
// reuse whatever running capacity a prior scenario caused; sawtooth itself
// proves return-to-floor behavior between its cycles.
const ALL: Scenario[] = [
  sawtooth,
  scaleInDrain,
  standbyRefillBoundary,
  snapshotFanoutResume,
  heldBurst,
  gradualRamp,
  secondWave,
  longLived,
  churn,
]
let interrupted = false
let activeContext: ScenarioContext | undefined
let baselineSandboxIds = new Set<string>()

async function main(): Promise<void> {
  if (process.env.LIVE_AUTOSCALE_BENCHMARK !== LIVE_ACK) {
    throw new Error(`refusing live run: set LIVE_AUTOSCALE_BENCHMARK=${LIVE_ACK}`)
  }
  await preflight()
  const wanted = process.argv.slice(2)
  const unknown = wanted.filter((name) => !ALL.some((scenario) => scenario.name === name))
  if (unknown.length) throw new Error(`unknown scenario(s): ${unknown.join(', ')}; available: ${ALL.map((s) => s.name).join(', ')}`)
  const scenarios = wanted.length ? ALL.filter((scenario) => wanted.includes(scenario.name)) : ALL
  const startedAt = new Date().toISOString()
  const results: ScenarioResult[] = []

  for (const scenario of scenarios) {
    if (interrupted) break
    await assertCleanGateway()
    console.log(`\n== ${scenario.name} ==`)
    const ctx = new ScenarioContext(scenario.name)
    activeContext = ctx
    ctx.startMonitor()
    const started = Date.now()
    try {
      await scenario.run(ctx)
      assertLatencyAcceptance(ctx)
    } catch (error) {
      ctx.record('scenario', error)
    } finally {
      await ctx.stopMonitor()
      await ctx.killAll()
      try {
        await assertCleanGateway()
      } catch (error) {
        ctx.record('postflight-cleanup', error)
      }
    }
    results.push({
      name: scenario.name,
      startedAt: new Date(started).toISOString(),
      wallMs: Date.now() - started,
      creates: ctx.creates,
      createLatencyMs: ctx.createLatencyMs,
      probes: ctx.probes,
      failures: ctx.failures,
      hostPeak: ctx.hostPeak,
    })
    activeContext = undefined
    console.log(
      `${ctx.failures.length ? 'FAIL' : 'PASS'} ${scenario.name}: ` +
      `${ctx.creates} creates, ${ctx.probes} survivability probes, peak ${ctx.hostPeak} hosts`
    )
    if (ctx.failures.length) break
  }

  mkdirSync(dirname(OUTPUT), { recursive: true })
  const report = {
    target: API_URL,
    runId: RUN_ID,
    expectedRelease: EXPECTED_RELEASE,
    startedAt,
    finishedAt: new Date().toISOString(),
    config: {
      floorHosts: FLOOR_HOSTS,
      slotsPerHost: SLOTS_PER_HOST,
      ttlMs: TTL_MS,
      requestTimeoutMs: REQUEST_TIMEOUT_MS,
      probeIntervalMs: PROBE_INTERVAL_MS,
      maxHosts: MAX_HOSTS,
      maxLiveSandboxes: MAX_LIVE_SANDBOXES,
      maxCreateP95Ms: MAX_CREATE_P95_MS,
      maxCreateMs: MAX_CREATE_MS,
      baselineSandboxIds: [...baselineSandboxIds],
    },
    results,
    summary: {
      scenarios: results.length,
      failedScenarios: results.filter((result) => result.failures.length).length,
      creates: results.reduce((sum, result) => sum + result.creates, 0),
      probes: results.reduce((sum, result) => sum + result.probes, 0),
      failures: results.reduce((sum, result) => sum + result.failures.length, 0),
    },
  }
  writeFileSync(OUTPUT, JSON.stringify(report, null, 2))
  console.log(`\nReport: ${OUTPUT}`)
  if (report.summary.failures || interrupted) process.exitCode = interrupted ? 130 : 1
}

async function preflight(): Promise<void> {
  if (FLOOR_HOSTS * SLOTS_PER_HOST > MAX_LIVE_SANDBOXES) {
    throw new Error(
      `preflight: floor capacity ${FLOOR_HOSTS * SLOTS_PER_HOST} exceeds ` +
      `AUTOSCALE_MAX_LIVE_SANDBOXES=${MAX_LIVE_SANDBOXES}`
    )
  }
  const sandboxes = await getSandboxes()
  if (sandboxes.length) {
    if (!ALLOW_HIBERNATED_BASELINE) {
      throw new Error(`preflight: gateway already owns ${sandboxes.length} sandboxes`)
    }
    const active = sandboxes.filter((sandbox) => sandbox.status !== 'hibernated')
    if (active.length) {
      throw new Error(
        `preflight: refusing a baseline with ${active.length} non-hibernated sandbox(es)`
      )
    }
  }
  baselineSandboxIds = new Set(sandboxes.map((sandbox) => sandbox.id))
  const hosts = await getHosts()
  assertHostInvariants(hosts)
  const alive = hosts.filter((host) => host.alive)
  if (alive.length !== FLOOR_HOSTS) {
    throw new Error(`preflight: expected exactly ${FLOOR_HOSTS} alive floor hosts, got ${alive.length}`)
  }
  for (const host of alive) {
    if (host.slots_used !== 0 || host.free + (host.warm_ready ?? 0) !== SLOTS_PER_HOST) {
      throw new Error(
        `preflight: ${host.id} is not empty ${SLOTS_PER_HOST}-slot floor ` +
        `(used=${host.slots_used}, free=${host.free}, warm=${host.warm_ready ?? 0})`
      )
    }
  }
}

function assertHostInvariants(hosts: RawHost[]): void {
  const ids = duplicateIds(hosts.map((host) => host.id))
  if (ids.length) throw new Error(`duplicate host ids: ${ids.join(',')}`)
  const alive = hosts.filter((candidate) => candidate.alive)
  if (alive.length > MAX_HOSTS) {
    throw new Error(`alive host count ${alive.length} exceeds AUTOSCALE_MAX_HOSTS=${MAX_HOSTS}`)
  }
  for (const host of alive) {
    if (host.release !== EXPECTED_RELEASE || host.release_compatible !== true) {
      if (host.free !== 0) {
        throw new Error(
          `incompatible host ${host.id} advertised free=${host.free}: ` +
          `release=${host.release ?? '<missing>'}, compatible=${String(host.release_compatible)}, ` +
          `expected=${EXPECTED_RELEASE}`
        )
      }
      continue
    }
    if (host.slots_total !== SLOTS_PER_HOST) {
      throw new Error(`host ${host.id}: slots_total=${host.slots_total}, expected ${SLOTS_PER_HOST}`)
    }
    if (host.slots_used < 0 || host.slots_used > host.slots_total) {
      throw new Error(`host ${host.id}: slots_used=${host.slots_used} outside [0,${host.slots_total}]`)
    }
    if (host.free < 0 || host.free > host.slots_total) {
      throw new Error(`host ${host.id}: free=${host.free} outside [0,${host.slots_total}]`)
    }
    if (host.slots_used + host.free > host.slots_total) {
      throw new Error(
        `host ${host.id}: used+free=${host.slots_used + host.free} exceeds total=${host.slots_total}`
      )
    }
  }
}

function assertLatencyAcceptance(ctx: ScenarioContext): void {
  if (!ctx.createLatencyMs.length) {
    throw new Error(`${ctx.scenario}: no successful creates to evaluate`)
  }
  const p95 = percentile(ctx.createLatencyMs, 95)
  const max = Math.max(...ctx.createLatencyMs)
  if (p95 > MAX_CREATE_P95_MS || max > MAX_CREATE_MS) {
    throw new Error(
      `${ctx.scenario}: create latency outside acceptance bounds: ` +
      `p95=${p95}ms (limit ${MAX_CREATE_P95_MS}ms), ` +
      `max=${max}ms (limit ${MAX_CREATE_MS}ms)`
    )
  }
}

function percentile(values: number[], p: number): number {
  const sorted = [...values].sort((a, b) => a - b)
  const index = Math.min(sorted.length - 1, Math.max(0, Math.ceil((p / 100) * sorted.length) - 1))
  return sorted[index]
}

interface MigSnapshot {
  ts_ms: number
  target_size: number
  running: number
  transitioning: number
}

function getMigSnapshot(): MigSnapshot {
  if (!MIG_SNAPSHOT) {
    throw new Error('AUTOSCALE_MIG_SNAPSHOT is required for sawtooth physical scale-in proof')
  }
  try {
    const value = JSON.parse(readFileSync(MIG_SNAPSHOT, 'utf8')) as Partial<MigSnapshot>
    if (
      !Number.isFinite(value.ts_ms) ||
      !Number.isInteger(value.target_size) ||
      !Number.isInteger(value.running) ||
      !Number.isInteger(value.transitioning)
    ) {
      throw new Error('snapshot fields are missing or invalid')
    }
    return value as MigSnapshot
  } catch (error) {
    throw new Error(`cannot read MIG snapshot ${MIG_SNAPSHOT}: ${String((error as Error).message ?? error)}`)
  }
}

function readTimeline(path: string): TimelineEvent[] {
  try {
    return readFileSync(path, 'utf8')
      .split('\n')
      .filter((line) => line.trim())
      .map((line) => JSON.parse(line) as TimelineEvent)
  } catch (error) {
    throw new Error(`cannot read timeline ${path}: ${String((error as Error).message ?? error)}`)
  }
}

/**
 * Proves that newly created standby-refill instances never become placement
 * eligible during GCE's 180s initial-delay boundary. Instance names present
 * before the scenario are resumed standbys and are intentionally eligible
 * immediately; only names first observed after scenario start are refill VMs.
 *
 * For every refill VM, correlate MIG instance name -> Nomad node id -> gateway
 * host id. Any capacity heartbeat must be at least placementDelayMs after the
 * MIG first reports the instance being created. The production gate uses
 * Linux boot age, so Nomad task StartedAt is deliberately not the delay
 * anchor: startup work consumes part of the gate.
 *
 * A refill still creating or suspending is pending lifecycle work, not a hard
 * violation. The caller keeps holding and probing live sandboxes until this
 * returns no pending instances or its bounded settle deadline expires.
 */
function pendingStandbyRefillSuspensions(
  events: TimelineEvent[],
  scenarioStartedMs: number,
  placementDelayMs: number
): string[] {
  const migEvents = events.filter((event) => event.event === 'mig_instance_state')
  const baseline = new Set(
    migEvents
      .filter((event) => event.ts_ms < scenarioStartedMs)
      .map((event) => String(event.data.instance ?? ''))
      .filter(Boolean)
  )
  const fresh = new Set(
    migEvents
      .filter((event) => event.ts_ms >= scenarioStartedMs)
      .map((event) => String(event.data.instance ?? ''))
      .filter((name) => name && !baseline.has(name))
  )
  if (!fresh.size) {
    throw new Error('standby refill proof observed no newly created MIG instance')
  }

  const pending: string[] = []
  for (const instance of fresh) {
    const firstMigObserved = migEvents
      .filter(
        (event) =>
          event.ts_ms >= scenarioStartedMs &&
          event.data.instance === instance
      )
      .map((event) => event.ts_ms)
      .sort((a, b) => a - b)[0]
    if (!Number.isFinite(firstMigObserved)) {
      throw new Error(`fresh refill ${instance} has no MIG creation observation`)
    }

    const instanceMigEvents = migEvents.filter(
      (event) =>
        event.ts_ms >= scenarioStartedMs &&
        event.data.instance === instance
    )
    const terminal = instanceMigEvents.find(
      (event) =>
        event.data.status === 'TERMINATED' ||
        event.data.status === 'STOPPED' ||
        event.data.current_action === 'ABANDONING' ||
        event.data.current_action === 'DELETING'
    )
    if (terminal) {
      throw new Error(
        `fresh refill ${instance} entered terminal state ` +
        `${String(terminal.data.status)}/${String(terminal.data.current_action)}`
      )
    }

    const suspended = instanceMigEvents.some((event) => event.data.status === 'SUSPENDED')

    const nodeEvent = events.find(
      (event) =>
        event.event === 'nomad_node_state' &&
        String(event.data.name ?? '').split('.')[0] === instance
    )
    if (!nodeEvent) {
      pending.push(`${instance} (awaiting Nomad registration)`)
      continue
    }
    const nodeID = String(nodeEvent.data.node_id ?? '')

    const eligible = events
      .filter(
        (event) =>
          event.event === 'gateway_host_eligible_observed' &&
          event.data.host_id === nodeID &&
          event.ts_ms >= scenarioStartedMs
      )
      .map((event) => event.ts_ms)
      .sort((a, b) => a - b)[0]
    if (eligible !== undefined && eligible - firstMigObserved < placementDelayMs) {
      throw new Error(
        `fresh refill ${instance} advertised capacity ${eligible - firstMigObserved}ms after ` +
        `first MIG creation observation; placement delay requires >=${placementDelayMs}ms`
      )
    }
    if (!suspended) pending.push(`${instance} (awaiting SUSPENDED)`)
  }
  return pending
}

async function assertCleanGateway(): Promise<void> {
  await waitFor(async () => {
    const [sandboxes, hosts] = await Promise.all([getSandboxes(), getHosts()])
    const ids = new Set(sandboxes.map((sandbox) => sandbox.id))
    const baselineIntact = ids.size === baselineSandboxIds.size &&
      [...baselineSandboxIds].every((id) => ids.has(id))
    return baselineIntact && hosts.filter((host) => host.alive).every((host) => host.slots_used === 0)
  }, 60_000, 500, 'only the preserved baseline sandboxes and zero advertised host usage')
  const hosts = await getHosts()
  assertHostInvariants(hosts)
}

// Host inventory is fleet control, not a tenant API: it discloses per-host
// addresses and capacity, so the gateway gates it on the worker-control
// credential. Use the /internal/v1 path, which has always been gated that way.
async function getHosts(): Promise<RawHost[]> {
  return apiJson<RawHost[]>('/internal/v1/hosts', CONTROL_KEY)
}

async function getSandboxes(): Promise<RawSandbox[]> {
  return apiJson<RawSandbox[]>('/sandboxes')
}

/**
 * Scrape the gateway's own counters. Scale-in is only observable here: a
 * cordoned host still heartbeats and still serves, so /hosts alone cannot
 * distinguish "draining" from "idle", and the delete is invisible until the
 * host disappears entirely.
 */
async function getGatewayMetrics(): Promise<Map<string, number>> {
  const response = await fetch(`${API_URL.replace(/\/$/, '')}/metrics`, {
    headers: { Authorization: `Bearer ${API_KEY}` },
    signal: AbortSignal.timeout(15_000),
  })
  if (!response.ok) throw new Error(`GET /metrics: HTTP ${response.status}`)
  const out = new Map<string, number>()
  for (const line of (await response.text()).split('\n')) {
    if (!line || line.startsWith('#')) continue
    // Only unlabelled families are needed here, so take `name value` and skip
    // anything carrying labels rather than half-parsing it.
    const match = /^([a-zA-Z_:][a-zA-Z0-9_:]*) ([0-9.eE+-]+)$/.exec(line.trim())
    if (match) out.set(match[1], Number(match[2]))
  }
  return out
}

function metric(metrics: Map<string, number>, name: string): number {
  const value = metrics.get(name)
  if (value === undefined) {
    throw new Error(
      `gateway /metrics has no ${name}: the deployed gateway predates ` +
        'gateway-owned scale-in, so this scenario cannot verify anything'
    )
  }
  return value
}

async function apiJson<T>(path: string, key: string = API_KEY): Promise<T> {
  const response = await fetch(`${API_URL.replace(/\/$/, '')}${path}`, {
    headers: { Authorization: `Bearer ${key}` },
    signal: AbortSignal.timeout(15_000),
  })
  if (!response.ok) throw new Error(`GET ${path}: HTTP ${response.status}: ${(await response.text()).slice(0, 200)}`)
  return response.json() as Promise<T>
}

async function waitFor(
  predicate: () => Promise<boolean>,
  timeoutMs: number,
  intervalMs: number,
  description: string,
  failFast?: () => void
): Promise<void> {
  const deadline = Date.now() + timeoutMs
  let lastError: unknown
  while (Date.now() < deadline) {
    failFast?.()
    try {
      if (await predicate()) return
    } catch (error) {
      lastError = error
    }
    await sleep(intervalMs)
  }
  throw new Error(
    `timed out after ${timeoutMs}ms waiting for ${description}` +
    (lastError ? `: ${String((lastError as Error).message ?? lastError)}` : '')
  )
}

async function mapLimit<T>(
  count: number,
  concurrency: number,
  fn: (index: number) => Promise<T>
): Promise<T[]> {
  const result = new Array<T>(count)
  let next = 0
  await Promise.all(
    Array.from({ length: Math.min(count, concurrency) }, async () => {
      while (true) {
        const index = next++
        if (index >= count) return
        result[index] = await fn(index)
      }
    })
  )
  return result
}

async function mapItems<T>(
  items: readonly T[],
  concurrency: number,
  fn: (item: T, index: number) => Promise<void>
): Promise<void> {
  await mapLimit(items.length, concurrency, (index) => fn(items[index], index))
}

function duplicateIds(ids: string[]): string[] {
  const seen = new Set<string>()
  const duplicates = new Set<string>()
  for (const id of ids) {
    if (seen.has(id)) duplicates.add(id)
    seen.add(id)
  }
  return [...duplicates]
}

function required(name: string): string {
  const value = process.env[name]
  if (!value) throw new Error(`missing required environment variable ${name}`)
  return value
}

function integerEnv(name: string, fallback: number, minimum: number): number {
  const value = Number(process.env[name] ?? fallback)
  if (!Number.isInteger(value) || value < minimum) {
    throw new Error(`${name} must be an integer >= ${minimum}, got ${process.env[name] ?? fallback}`)
  }
  return value
}

function shellSingle(value: string): string {
  return value.replaceAll("'", "'\"'\"'")
}

function stamp(): string {
  return new Date().toISOString().replace(/[:.]/g, '-')
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolveSleep) => setTimeout(resolveSleep, ms))
}

for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  process.on(signal, () => {
    if (interrupted) return
    interrupted = true
    console.error(`\n${signal}: cleanup requested`)
    void (async () => {
      if (activeContext) await activeContext.abortAndCleanup()
      process.exit(130)
    })()
  })
}

void main().catch(async (error) => {
  console.error(error)
  if (activeContext) await activeContext.killAll()
  process.exitCode = 1
})
