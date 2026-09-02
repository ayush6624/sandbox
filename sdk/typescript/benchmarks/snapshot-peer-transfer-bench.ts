/**
 * Cross-host snapshot transport benchmark.
 *
 * A source worker owns a continuously dirtied sandbox. Every measured case
 * takes a fresh snapshot, waits for its GCS commit marker, and asks a second
 * worker to fan it out. The target is cold for that snapshot ID, so omitting
 * X-Sandbox-Snapshot-Peer measures GCS while including it measures direct
 * worker-to-worker streaming. Raw fan-out calls are chunked at eight, exactly
 * like createMany(), while the worker-wide create semaphore remains global.
 */
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { SandboxClient, type ClientSandbox } from '../src/index.js'
import { benchmarkMetadata } from './metadata.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const RESULTS_DIR = join(HERE, 'results')
const GUEST_SOURCE = join(HERE, 'snapshot-working-set-guest.ts')
const GUEST_PATH = '/tmp/snapshot-working-set-guest.ts'
const ROOT = '/tmp/snapshot-working-set'
const PID_PATH = '/tmp/snapshot-working-set.pid'
const RAW_FANOUT_LIMIT = 8

type Mode = 'gcs' | 'peer'

interface Args {
  sourceUrl: string
  targetUrl: string
  peerUrl: string
  apiKey: string
  workerKey: string
  counts: number[]
  rounds: number
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
    rounds: 2,
    memoryMiB: 384,
    diskMiB: 384,
    smallFiles: 5_000,
    sqliteMiB: 32,
    durableTimeoutMs: 10 * 60_000,
  }
  for (let index = 0; index < argv.length; index++) {
    const flag = argv[index]
    if (flag === '--source-url') values.sourceUrl = argv[++index]
    else if (flag === '--target-url') values.targetUrl = argv[++index]
    else if (flag === '--peer-url') values.peerUrl = argv[++index]
    else if (flag === '--api-key') values.apiKey = argv[++index]
    else if (flag === '--worker-key') values.workerKey = argv[++index]
    else if (flag === '--counts') values.counts = argv[++index]!.split(',').map((raw) => integer(raw, flag, 1, 48))
    else if (flag === '--rounds') values.rounds = integer(argv[++index], flag, 1)
    else if (flag === '--memory-mib') values.memoryMiB = integer(argv[++index], flag, 1)
    else if (flag === '--disk-mib') values.diskMiB = integer(argv[++index], flag, 1)
    else if (flag === '--small-files') values.smallFiles = integer(argv[++index], flag, 1)
    else if (flag === '--sqlite-mib') values.sqliteMiB = integer(argv[++index], flag, 1)
    else if (flag === '--durable-timeout-ms') values.durableTimeoutMs = integer(argv[++index], flag, 1)
    else if (flag === '--output') values.output = argv[++index]
    else throw new Error(`unknown argument: ${flag}`)
  }
  return {
    sourceUrl: required(values.sourceUrl, '--source-url'),
    targetUrl: required(values.targetUrl, '--target-url'),
    peerUrl: required(values.peerUrl, '--peer-url'),
    apiKey: required(values.apiKey, '--api-key'),
    workerKey: required(values.workerKey, '--worker-key'),
    counts: values.counts!,
    rounds: values.rounds!,
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
  return (after.get(name) ?? 0) - (before.get(name) ?? 0)
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

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2))
  const metadata = benchmarkMetadata('snapshot-peer-transfer', {
    counts: args.counts,
    rounds: args.rounds,
    memory_mib: args.memoryMiB,
    disk_mib: args.diskMiB,
    small_files: args.smallFiles,
    sqlite_mib: args.sqliteMiB,
    raw_fanout_limit: RAW_FANOUT_LIMIT,
  })
  const sourceClient = new SandboxClient({ baseUrl: args.sourceUrl, apiKey: args.apiKey })
  const targetClient = new SandboxClient({ baseUrl: args.targetUrl, apiKey: args.apiKey })
  const rows: Row[] = []
  let failure: unknown
  try {
    for (let round = 1; round <= args.rounds; round++) {
      const modes: Mode[] = round % 2 === 1 ? ['gcs', 'peer'] : ['peer', 'gcs']
      let order = 0
      for (const count of args.counts) {
        for (const mode of modes) rows.push(await runCase(args, sourceClient, targetClient, metadata.run_id, round, ++order, mode, count))
      }
    }
  } catch (error) {
    failure = error
  } finally {
    const timestamp = new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '').replace('T', '_')
    const output = args.output ?? join(RESULTS_DIR, `snapshot_peer_transfer_${timestamp}.json`)
    mkdirSync(dirname(output), { recursive: true })
    writeFileSync(output, JSON.stringify({ metadata, passed: failure === undefined, rows,
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
