/**
 * Cross-host snapshot transport benchmark.
 *
 * Direct transport cases take a fresh snapshot, wait for its GCS commit marker,
 * and ask a second worker to fan it out. The target is cold for that snapshot ID, so omitting
 * X-Sandbox-Snapshot-Peer measures GCS while including it measures direct
 * worker-to-worker streaming. Raw fan-out calls are chunked at eight, exactly
 * like createMany(), while the worker-wide create semaphore remains global.
 * gateway-pending instead submits through the public gateway while the source
 * is alive and upload is pending, then verifies placement and peer counters.
 */
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { NotFoundError, SandboxClient, type ClientSandbox } from '../src/index.js'
import { benchmarkMetadata, benchmarkResourceMetadata, observeBuilds, redactTarget, type BenchmarkMetadata } from './metadata.js'
import { observeBatch } from './observe-batch.js'
import { cleanupRunSandboxes, deleteSnapshotWithRetry } from './snapshot-batch-bench.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const RESULTS_DIR = join(HERE, 'results')
const GUEST_SOURCE = join(HERE, 'snapshot-working-set-guest.ts')
const GUEST_PATH = '/tmp/snapshot-working-set-guest.ts'
const ROOT = '/tmp/snapshot-working-set'
const PID_PATH = '/tmp/snapshot-working-set.pid'
const RAW_FANOUT_LIMIT = 8
const FILLER_MEMORY_MIB = 256

type Mode = 'gcs' | 'peer'
type CaseMode = Mode | 'gateway-pending'

interface Args {
  sourceUrl: string
  gatewayUrl?: string
  modes: CaseMode[]
  targetUrl: string
  peerUrl: string
  apiKey: string
  gatewayApiKey: string
  workerKey: string
  counts: number[]
  rounds: number
  sourceFillers: number
  memoryMiB: number
  diskMiB: number
  smallFiles: number
  sqliteMiB: number
  durableTimeoutMs: number
  output?: string
}

interface RawSandbox { id: string }

interface Row {
  round: number
  order: number
  mode: Mode
  count: number
  chunks: number[]
  snapshotId: string
  snapshotFormat: string
  sourceCreateMs: number
  sourceSetupMs: number
  snapshotCreateMs: number
  durableMs: number
  fanoutMs: number
  hydratedMs: number
  cleanupMs: number
  peerPulls: number
  peerPullFailures: number
  peerServes: number
  peerPayloadBytes: number
  gcsFallbacks: number
  ok: number
}

function integer(raw: string | undefined, flag: string, min: number, max = Number.MAX_SAFE_INTEGER): number {
  const value = Number(raw)
  if (!Number.isInteger(value) || value < min || value > max) {
    throw new Error(`${flag} must be an integer from ${min} to ${max}`)
  }
  return value
}

function required(raw: string | undefined, flag: string): string {
  if (!raw) throw new Error(`${flag} is required`)
  return raw.replace(/\/+$/, '')
}

function parseArgs(argv: string[]): Args {
  const values: Partial<Args> = {
    counts: [1, 8, 16],
    modes: ['gcs', 'peer'],
    rounds: 2,
    sourceFillers: 0,
    memoryMiB: 384,
    diskMiB: 384,
    smallFiles: 5_000,
    sqliteMiB: 32,
    durableTimeoutMs: 10 * 60_000,
  }
  for (let index = 0; index < argv.length; index++) {
    const flag = argv[index]
    if (flag === '--gateway-url') values.gatewayUrl = argv[++index]
    else if (flag === '--modes') {
      values.modes = (argv[++index] ?? '').split(',').map((value): CaseMode => {
        if (value === 'gcs' || value === 'peer' || value === 'gateway-pending') return value
        throw new Error('--modes must contain gcs, peer, or gateway-pending')
      })
    } else if (flag === '--source-url') values.sourceUrl = argv[++index]
    else if (flag === '--target-url') values.targetUrl = argv[++index]
    else if (flag === '--peer-url') values.peerUrl = argv[++index]
    else if (flag === '--api-key') values.apiKey = argv[++index]
    else if (flag === '--gateway-api-key') values.gatewayApiKey = argv[++index]
    else if (flag === '--worker-key') values.workerKey = argv[++index]
    else if (flag === '--counts') values.counts = argv[++index]!.split(',').map((raw) => integer(raw, flag, 1, 48))
    else if (flag === '--rounds') values.rounds = integer(argv[++index], flag, 1)
    else if (flag === '--source-fillers') values.sourceFillers = integer(argv[++index], flag, 0, 47)
    else if (flag === '--memory-mib') values.memoryMiB = integer(argv[++index], flag, 1)
    else if (flag === '--disk-mib') values.diskMiB = integer(argv[++index], flag, 1)
    else if (flag === '--small-files') values.smallFiles = integer(argv[++index], flag, 1)
    else if (flag === '--sqlite-mib') values.sqliteMiB = integer(argv[++index], flag, 1)
    else if (flag === '--durable-timeout-ms') values.durableTimeoutMs = integer(argv[++index], flag, 1)
    else if (flag === '--output') values.output = argv[++index]
    else throw new Error(`unknown argument: ${flag}`)
  }
  if (values.modes?.includes('gateway-pending') && !values.gatewayUrl) throw new Error('--gateway-url is required for gateway-pending')
  return {
    gatewayUrl: values.gatewayUrl,
    modes: values.modes!,
    sourceUrl: required(values.sourceUrl, '--source-url'),
    targetUrl: required(values.targetUrl, '--target-url'),
    peerUrl: values.modes?.includes('peer') ? required(values.peerUrl, '--peer-url') : '',
    apiKey: required(values.apiKey ?? process.env.SANDBOX_API_KEY, '--api-key or SANDBOX_API_KEY'),
    gatewayApiKey: values.gatewayApiKey ?? process.env.SANDBOX_GATEWAY_API_KEY ?? values.apiKey ?? process.env.SANDBOX_API_KEY ?? '',
    workerKey: values.modes?.some((mode) => mode !== 'gateway-pending')
      ? required(values.workerKey ?? process.env.SANDBOX_CONTROL_KEY, '--worker-key or SANDBOX_CONTROL_KEY') : '',
    counts: values.counts!,
    rounds: values.rounds!,
    sourceFillers: values.sourceFillers!,
    memoryMiB: values.memoryMiB!,
    diskMiB: values.diskMiB!,
    smallFiles: values.smallFiles!,
    sqliteMiB: values.sqliteMiB!,
    durableTimeoutMs: values.durableTimeoutMs!,
    ...(values.output === undefined ? {} : { output: values.output }),
  }
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`
}

async function request(url: string, key: string, init: RequestInit = {}, timeoutMs = 30_000): Promise<Response> {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const response = await fetch(url, {
      ...init,
      signal: controller.signal,
      headers: { Authorization: `Bearer ${key}`, ...init.headers },
    })
    if (!response.ok) throw new Error(`${init.method ?? 'GET'} ${url}: HTTP ${response.status}: ${await response.text()}`)
    return response
  } finally {
    clearTimeout(timer)
  }
}

async function prepareSource(source: ClientSandbox, args: Args, runId: string): Promise<void> {
  const encoded = Buffer.from(readFileSync(GUEST_SOURCE, 'utf8')).toString('base64')
  await source.commands.run(`printf %s ${shellQuote(encoded)} | base64 -d > ${GUEST_PATH}`)
  const holderArgs = [args.memoryMiB, args.diskMiB, args.smallFiles, args.sqliteMiB, runId]
    .map((value) => shellQuote(String(value))).join(' ')
  await source.commands.run(
    `nohup node --no-warnings ${GUEST_PATH} hold ${holderArgs} >/tmp/snapshot-working-set.log 2>&1 & ` +
    `for i in $(seq 1 600); do test -f ${ROOT}/ready && exit 0; sleep .25; done; ` +
    'cat /tmp/snapshot-working-set.log; exit 1',
    { timeoutMs: 180_000 },
  )
}

async function waitDurable(args: Args, snapshotId: string): Promise<number> {
  const started = Date.now()
  while (Date.now() - started < args.durableTimeoutMs) {
    const response = await request(`${args.sourceUrl}/snapshots`, args.apiKey)
    const snapshots = await response.json() as Array<{ id: string; durability?: string }> | null
    if (snapshots?.some((snapshot) => snapshot.id === snapshotId && snapshot.durability === 'durable')) {
      return Date.now() - started
    }
    await sleep(250)
  }
  throw new Error(`snapshot ${snapshotId} did not become durable within ${args.durableTimeoutMs}ms`)
}

function chunks(count: number): number[] {
  const out: number[] = []
  for (let remaining = count; remaining > 0; remaining -= RAW_FANOUT_LIMIT) {
    out.push(Math.min(remaining, RAW_FANOUT_LIMIT))
  }
  return out
}

async function metrics(args: Args, baseUrl: string): Promise<Map<string, number>> {
  const text = await (await request(`${baseUrl}/metrics`, args.apiKey)).text()
  const out = new Map<string, number>()
  for (const line of text.split('\n')) {
    const match = /^(sandbox_[a-z0-9_]+)\s+([-+0-9.eE]+)$/.exec(line)
    if (match) out.set(match[1]!, Number(match[2]))
  }
  return out
}

function delta(after: Map<string, number>, before: Map<string, number>, name: string): number {
  const previous = before.get(name)
  const current = after.get(name)
  if (previous === undefined || current === undefined) throw new Error(`missing path counter ${name}`)
  if (current < previous) throw new Error(`path counter ${name} reset during the case`)
  return current - previous
}

async function fanout(args: Args, mode: Mode, snapshotId: string, count: number): Promise<RawSandbox[]> {
  const calls = chunks(count).map(async (chunk) => {
    const headers: Record<string, string> = { 'Content-Type': 'application/json' }
    if (mode === 'peer') headers['X-Sandbox-Snapshot-Peer'] = args.peerUrl
    const response = await request(
      `${args.targetUrl}/snapshots/${encodeURIComponent(snapshotId)}/fanout`,
      args.apiKey,
      { method: 'POST', headers, body: JSON.stringify({ count: chunk, hibernate_after_sec: -1 }) },
      15 * 60_000,
    )
    return response.json() as Promise<RawSandbox[]>
  })
  return (await Promise.all(calls)).flat()
}

async function runCase(
  args: Args,
  sourceClient: SandboxClient,
  target: SandboxClient,
  runId: string,
  round: number,
  order: number,
  mode: Mode,
  count: number,
): Promise<Row> {
  let source: ClientSandbox | undefined
  let snapshotId: string | undefined
  let clones: ClientSandbox[] = []
  try {
    const sourceStarted = Date.now()
    source = await sourceClient.sandboxes.create({
      metadata: { benchmark: 'snapshot-peer-transfer', benchmark_run_id: runId },
      idleTimeoutMs: 0,
      requestTimeoutMs: 10 * 60_000,
    })
    const sourceCreateMs = Date.now() - sourceStarted
    const setupStarted = Date.now()
    await prepareSource(source, args, runId)
    const sourceSetupMs = Date.now() - setupStarted
    await source.commands.run(`test -f ${ROOT}/ready && kill -0 $(cat ${PID_PATH})`)
    await sleep(250)
    const snapshotStarted = Date.now()
    const snapshot = await source.createSnapshot({ name: `peer-bench-${mode}-r${round}-n${count}` })
    snapshotId = snapshot.id
    const snapshotCreateMs = Date.now() - snapshotStarted
    await source.terminate({ timeoutMs: 120_000 })
    source = undefined
    const durableMs = await waitDurable(args, snapshot.id)
    const meta = await (await request(
      `${args.sourceUrl}/internal/v1/snapshots/${encodeURIComponent(snapshot.id)}`,
      args.workerKey,
    )).json() as { format?: string }
    const snapshotFormat = meta.format ?? 'full'
    if (snapshotFormat !== 'diff') {
      throw new Error(`fresh hot-clone snapshot ${snapshot.id} has format ${snapshotFormat}, want diff`)
    }
    const [targetBefore, sourceBefore] = await Promise.all([
      metrics(args, args.targetUrl), metrics(args, args.sourceUrl),
    ])
    const fanoutStarted = Date.now()
    const raw = await fanout(args, mode, snapshot.id, count)
    const fanoutMs = Date.now() - fanoutStarted
    if (raw.length !== count) throw new Error(`${mode} n=${count} returned ${raw.length}/${count} clones`)
    clones = await Promise.all(raw.map((sandbox) => target.sandboxes.get(sandbox.id)))
    await Promise.all(clones.map(async (clone) => {
      await clone.commands.run(`node --no-warnings ${GUEST_PATH} verify ${shellQuote(runId)}`, { timeoutMs: 180_000 })
    }))
    const hydratedMs = Date.now() - fanoutStarted
    const [targetAfter, sourceAfter] = await Promise.all([
      metrics(args, args.targetUrl), metrics(args, args.sourceUrl),
    ])
    const cleanupStarted = Date.now()
    await Promise.all(clones.map((clone) => clone.terminate({ timeoutMs: 120_000 })))
    const cleanupMs = Date.now() - cleanupStarted
    await target.snapshots.delete(snapshot.id, { timeoutMs: 120_000 })
    await new SandboxClient({ baseUrl: args.sourceUrl, apiKey: args.apiKey }).snapshots.delete(snapshot.id, { timeoutMs: 120_000 })
    const row: Row = {
      round, order, mode, count, chunks: chunks(count), snapshotId: snapshot.id,
      snapshotFormat, sourceCreateMs, sourceSetupMs, snapshotCreateMs, durableMs,
      fanoutMs, hydratedMs, cleanupMs,
      peerPulls: delta(targetAfter, targetBefore, 'sandbox_snapshot_peer_pulls_total'),
      peerPullFailures: delta(targetAfter, targetBefore, 'sandbox_snapshot_peer_pull_failures_total'),
      peerServes: delta(sourceAfter, sourceBefore, 'sandbox_snapshot_peer_serves_total'),
      peerPayloadBytes: delta(sourceAfter, sourceBefore, 'sandbox_snapshot_peer_payload_bytes_total'),
      gcsFallbacks: delta(targetAfter, targetBefore, 'sandbox_snapshot_gcs_fallbacks_total'),
      ok: clones.length,
    }
    if (mode === 'peer' && (row.peerPulls !== 1 || row.peerPullFailures !== 0 || row.peerServes !== 3 || row.peerPayloadBytes <= 0)) {
      throw new Error(`peer path was not observed: ${JSON.stringify(row)}`)
    }
    if (mode === 'gcs' && row.peerPulls !== 0) throw new Error(`GCS case unexpectedly used peer transport`)
    console.log(
      `r${round} #${order} ${mode.padEnd(4)} n=${String(count).padStart(2)} ` +
      `snapshot=${snapshotCreateMs}ms durable=${durableMs}ms fanout=${fanoutMs}ms ` +
      `hydrated=${hydratedMs}ms peer=${(row.peerPayloadBytes / 1048576).toFixed(1)}MiB`,
    )
    return row
  } catch (error) {
    if (source) await source.terminate({ timeoutMs: 120_000 }).catch(() => {})
    await Promise.allSettled(clones.map((clone) => clone.terminate({ timeoutMs: 120_000 })))
    if (snapshotId) {
      await target.snapshots.delete(snapshotId, { timeoutMs: 120_000 }).catch(() => {})
      await sourceClient.snapshots.delete(snapshotId, { timeoutMs: 120_000 }).catch(() => {})
    }
    throw error
  }
}

interface PendingRow {
  round: number
  order: number
  mode: 'gateway-pending'
  count: number
  passed: boolean
  snapshotId?: string
  snapshotStateAtRequest?: 'local' | 'durable'
  snapshotCreateMs?: number
  acceptedMs?: number
  operationMs?: number
  operationId?: string
  snapshotToHydratedMs?: number
  peerPulls?: number
  peerPullFailures?: number
  gcsFallbacks?: number
  peerPayloadBytes?: number
  items: Array<{ index: number; sandboxId: string; commandReadyMs?: number; hydratedMs?: number; destinationVerified?: boolean; error?: string }>
  cleanupErrors: string[]
  error?: string
}

async function runPendingCase(
  args: Args, source: SandboxClient, target: SandboxClient, metadata: BenchmarkMetadata,
  round: number, order: number, count: number,
): Promise<PendingRow> {
  if (!args.gatewayUrl) throw new Error('gateway-pending requires --gateway-url')
  const gateway = new SandboxClient({ baseUrl: args.gatewayUrl, apiKey: args.gatewayApiKey })
  const labels = { ...benchmarkResourceMetadata(metadata), benchmark_case: `${round}-${order}`, benchmark_role: 'clone' }
  const sourceLabels = { ...labels, benchmark_role: 'source' }
  const tracked = new Map<string, ClientSandbox>()
  const row: PendingRow = { round, order, mode: 'gateway-pending', count, passed: false, items: [], cleanupErrors: [] }
  let sandbox: ClientSandbox | undefined
  try {
    sandbox = await source.sandboxes.create({ metadata: sourceLabels, idleTimeoutMs: 0 })
    await prepareSource(sandbox, args, metadata.run_id)
    const discoveryDeadline = performance.now() + 15_000
    let gatewaySource: ClientSandbox
    for (;;) {
      try {
        gatewaySource = await gateway.sandboxes.get(sandbox.id, AbortSignal.timeout(5_000))
        break
      } catch (error) {
        if (!(error instanceof NotFoundError) || performance.now() >= discoveryDeadline) throw error
        await sleep(250)
      }
    }
    const [targetBefore, sourceBefore] = await Promise.all([metrics(args, args.targetUrl), metrics(args, args.sourceUrl)])
    const snapshotStarted = performance.now()
    const snapshot = await gatewaySource.createSnapshot()
    row.snapshotId = snapshot.id
    row.snapshotCreateMs = performance.now() - snapshotStarted
    row.snapshotStateAtRequest = (await source.snapshots.get(snapshot.id)).state
    if (row.snapshotStateAtRequest !== 'local') throw new Error('snapshot is already durable; upload-pending precondition was not observed')
    const started = performance.now()
    const operation = await gateway.sandboxes.createMany({
      count, maxParallelism: Math.min(count, 32), source: { snapshotId: snapshot.id }, metadata: labels, idleTimeoutMs: 0,
    })
    row.acceptedMs = performance.now() - started
    row.operationId = operation.id
    row.operationMs = row.acceptedMs + await observeBatch({
      operation, timeoutMs: 15 * 60_000,
      onSandbox: async (clone, index) => {
        tracked.set(clone.id, clone)
        const item: PendingRow['items'][number] = { index, sandboxId: clone.id }
        row.items.push(item)
        try {
          const ready = await clone.commands.run('echo benchmark-ready', { timeoutMs: 30_000 })
          if (ready.stdout.trim() !== 'benchmark-ready') throw new Error('unexpected command readiness output')
          item.commandReadyMs = performance.now() - started
          await clone.commands.run(`node --no-warnings ${GUEST_PATH} verify ${shellQuote(metadata.run_id)}`, { timeoutMs: 180_000 })
          item.hydratedMs = performance.now() - started
        } catch (error) {
          item.error = error instanceof Error ? error.message : String(error)
        }
      },
    })
    row.snapshotToHydratedMs = row.items.length === count && row.items.every((item) => item.hydratedMs !== undefined)
      ? started - snapshotStarted + Math.max(...row.items.map((item) => item.hydratedMs ?? 0)) : undefined
    if (operation.state.status !== 'succeeded' || row.items.length !== count || row.items.some((item) => item.error)) {
      throw new Error('gateway batch or workload failed')
    }
    // These direct reads prove placement after the gateway-facing workload timer ends.
    for (const item of row.items) {
      await target.sandboxes.get(item.sandboxId)
      item.destinationVerified = true
    }
    const [targetAfter, sourceAfter] = await Promise.all([metrics(args, args.targetUrl), metrics(args, args.sourceUrl)])
    row.peerPulls = delta(targetAfter, targetBefore, 'sandbox_snapshot_peer_pulls_total')
    row.gcsFallbacks = delta(targetAfter, targetBefore, 'sandbox_snapshot_gcs_fallbacks_total')
    row.peerPayloadBytes = delta(sourceAfter, sourceBefore, 'sandbox_snapshot_peer_payload_bytes_total')
    row.peerPullFailures = delta(targetAfter, targetBefore, 'sandbox_snapshot_peer_pull_failures_total')
    if (row.peerPulls !== 1 || row.gcsFallbacks !== 0 || row.peerPullFailures !== 0 || row.peerPayloadBytes <= 0) {
      throw new Error('gateway did not use exactly one peer population on the intended target with zero fallback; check disposable fleet placement and snapshot advertisement')
    }
    row.passed = true
  } catch (error) {
    row.error = error instanceof Error ? error.message : String(error)
  } finally {
    // Discover creations whose HTTP responses or operation polls were lost.
    const cleanupResults = await Promise.all([
      cleanupRunSandboxes(gateway, tracked, labels),
      cleanupRunSandboxes(source, new Map(sandbox ? [[sandbox.id, sandbox]] : []), sourceLabels),
    ])
    row.cleanupErrors.push(...cleanupResults.flat())
    if (row.snapshotId) {
      // Local replicas must both be removed; the source call also joins its uploader.
      for (const client of [source, target]) {
        const error = await deleteSnapshotWithRetry(client, row.snapshotId)
        if (error) row.cleanupErrors.push(error)
      }
    }
    row.items.sort((a, b) => a.index - b.index)
    row.passed &&= row.cleanupErrors.length === 0
  }
  return row
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2))
  const metadata = benchmarkMetadata('snapshot-peer-transfer', {
    counts: args.counts,
    rounds: args.rounds,
    source_fillers: args.sourceFillers,
    source_filler_memory_mib: args.sourceFillers > 0 ? FILLER_MEMORY_MIB : undefined,
    memory_mib: args.memoryMiB,
    disk_mib: args.diskMiB,
    small_files: args.smallFiles,
    sqlite_mib: args.sqliteMiB,
    raw_fanout_limit: RAW_FANOUT_LIMIT,
    modes: args.modes,
    source: redactTarget(args.sourceUrl),
    target: redactTarget(args.targetUrl),
    gateway: args.gatewayUrl ? redactTarget(args.gatewayUrl) : undefined,
  }, args.gatewayUrl ?? args.targetUrl)
  await observeBuilds(metadata, args.sourceUrl, args.apiKey)
  await observeBuilds(metadata, args.targetUrl, args.apiKey)
  if (args.gatewayUrl) await observeBuilds(metadata, args.gatewayUrl, args.gatewayApiKey)
  const sourceClient = new SandboxClient({ baseUrl: args.sourceUrl, apiKey: args.apiKey })
  const targetClient = new SandboxClient({ baseUrl: args.targetUrl, apiKey: args.apiKey })
  const rows: Array<Row | PendingRow> = []
  const fillers = new Map<string, ClientSandbox>()
  const fillerLabels = { ...benchmarkResourceMetadata(metadata), benchmark_role: 'filler' }
  let failure: unknown
  try {
    for (let i = 0; i < args.sourceFillers; i++) {
      const filler = await sourceClient.sandboxes.create({
        metadata: fillerLabels, idleTimeoutMs: 0, resources: { vcpus: 1, memoryMib: FILLER_MEMORY_MIB },
      })
      fillers.set(filler.id, filler)
    }
    if (fillers.size) console.log(`reserved ${fillers.size} idle source sandboxes for placement`)
    for (let round = 1; round <= args.rounds; round++) {
      const modes = round % 2 === 1 ? args.modes : [...args.modes].reverse()
      let order = 0
      for (const count of args.counts) {
        for (const mode of modes) {
          if (mode === 'gateway-pending') {
            const row = await runPendingCase(args, sourceClient, targetClient, metadata, round, ++order, count)
            rows.push(row)
            if (!row.passed) throw new Error(row.error ?? `gateway-pending cleanup failed: ${row.cleanupErrors.join('; ')}`)
          } else rows.push(await runCase(args, sourceClient, targetClient, metadata.run_id, round, ++order, mode, count))
        }
      }
    }
  } catch (error) {
    failure = error
  } finally {
    const cleanupErrors = args.sourceFillers > 0 ? await cleanupRunSandboxes(sourceClient, fillers, fillerLabels) : []
    if (cleanupErrors.length && failure === undefined) failure = new Error(`source filler cleanup failed: ${cleanupErrors.join('; ')}`)
    await observeBuilds(metadata, args.sourceUrl, args.apiKey)
    await observeBuilds(metadata, args.targetUrl, args.apiKey)
    if (args.gatewayUrl) await observeBuilds(metadata, args.gatewayUrl, args.gatewayApiKey)
    const timestamp = new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '').replace('T', '_')
    const output = args.output ?? join(RESULTS_DIR, `snapshot_peer_transfer_${timestamp}.json`)
    mkdirSync(dirname(output), { recursive: true })
    writeFileSync(output, JSON.stringify({ metadata, passed: failure === undefined, rows, cleanupErrors,
      ...(failure === undefined ? {} : { error: String((failure as Error)?.stack ?? failure) }) }, null, 2))
    console.log(`saved ${output}`)
    if (failure !== undefined) throw failure
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(error instanceof Error ? (error.stack ?? error.message) : error)
    process.exit(1)
  })
}
