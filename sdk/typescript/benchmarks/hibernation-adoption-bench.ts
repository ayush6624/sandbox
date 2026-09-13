/** Direct two-worker hibernation adoption. Run after deployment with captured configs. */
import { readFileSync, mkdirSync, writeFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { randomUUID } from 'node:crypto'
import { benchmarkMetadata, benchmarkResourceMetadata, observeBuilds } from './metadata.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const GUEST_PATH = '/tmp/snapshot-working-set-guest.ts'
const ROOT = '/tmp/snapshot-working-set'
const sleep = (ms: number) => new Promise((done) => setTimeout(done, ms))
const elapsed = (start: number) => Math.round((performance.now() - start) * 100) / 100
const quote = (value: string) => `'${value.replaceAll("'", "'\\''")}'`

function object(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('expected an object')
  return Object.fromEntries(Object.entries(value))
}

function url(value: unknown): string {
  if (typeof value !== 'string') throw new Error('worker URL is required')
  const parsed = new URL(value)
  if (!['http:', 'https:'].includes(parsed.protocol) || parsed.username || parsed.password ||
      parsed.search || parsed.hash || parsed.pathname !== '/') throw new Error('use a worker origin without credentials')
  return parsed.origin
}

function positive(value: string | undefined, fallback: number): number {
  const n = value === undefined ? fallback : Number(value)
  if (!Number.isSafeInteger(n) || n < 1) throw new Error('numeric options must be positive integers')
  return n
}

interface SandboxRow {
  id: string
  status: string
  metadata: Record<string, unknown>
  observation: Record<string, unknown>
}

function sandbox(value: unknown): SandboxRow {
  const row = object(value)
  if (typeof row.id !== 'string' || typeof row.status !== 'string') throw new Error('invalid sandbox response')
  return { id: row.id, status: row.status, metadata: object(row.metadata ?? {}), observation: row }
}

async function main(): Promise<void> {
  const { values } = parseArgs({ options: {
    'source-url': { type: 'string' }, 'target-url': { type: 'string' }, mode: { type: 'string' }, transport: { type: 'string' }, handoff: { type: 'string' },
    'environment-file': { type: 'string' }, output: { type: 'string' }, iterations: { type: 'string' },
    'guest-memory-mib': { type: 'string' }, 'memory-mib': { type: 'string' }, 'disk-mib': { type: 'string' },
    'small-files': { type: 'string' }, 'sqlite-mib': { type: 'string' },
    'delay-ms': { type: 'string' }, roundtrip: { type: 'boolean' }, help: { type: 'boolean' },
  } })
  if (values.help) {
    console.log('Usage: node --import tsx benchmarks/hibernation-adoption-bench.ts --source-url URL --target-url URL --mode file|uffd --environment-file captured.json --output result.json [--transport auto|peer|gcs] [--handoff async|durable (default durable)] [--iterations 3] [--guest-memory-mib N] [--memory-mib 256] [--disk-mib 8] [--small-files 100] [--sqlite-mib 1] [--delay-ms 1000] [--roundtrip]')
    return
  }
  const source = url(values['source-url']), target = url(values['target-url'])
  if (source === target) throw new Error('source and target must be different workers')
  const transport = values.transport ?? 'auto'
  if (!['auto', 'peer', 'gcs'].includes(transport)) throw new Error('--transport must be auto, peer or gcs')
  const handoff = values.handoff ?? 'durable'
  if (handoff !== 'async' && handoff !== 'durable') throw new Error('--handoff must be async or durable')
  if (handoff === 'async' && transport === 'gcs') throw new Error('--transport gcs requires --handoff durable before removing the peer')
  const mode = values.mode
  if (mode !== 'file' && mode !== 'uffd') throw new Error('--mode must be file or uffd')
  if (!values.output || !values['environment-file']) throw new Error('--output and --environment-file are required')
  const token = process.env.HOST_TOKEN
  if (!token) throw new Error('HOST_TOKEN is required')
  const evidence = object(JSON.parse(readFileSync(values['environment-file'], 'utf8')))
  const capturedConfigs = [evidence.source, evidence.target].map((value, index) => {
    const captured = object(value), config = object(captured.config)
    if (url(captured.url) !== (index === 0 ? source : target)) throw new Error('captured config URL mismatch')
    if (config.vm_isolation !== 'jailer' || config.disable_seccomp !== false) throw new Error('captured config must retain jailer and seccomp')
    if (config.uffd_chunk_gcs !== true) throw new Error('chunk publication must be enabled on both workers')
    if ((index === 1 || values.roundtrip) && config.uffd_restore !== (mode === 'uffd')) throw new Error('captured restore backend does not match mode')
    return { url: captured.url, config: Object.fromEntries([
      'vm_isolation', 'disable_seccomp', 'uffd_restore', 'uffd_chunk_gcs', 'uffd_chunk_kib',
      'uffd_chunk_prefetch', 'vcpus', 'mem_mib', 'warm_pool_size',
    ].filter((key) => key in config).map((key) => [key, config[key]])) }
  })
  if (typeof evidence.cache_state !== 'string' || !evidence.cache_state.trim()) throw new Error('captured cache_state is required')
  const workload = {
    mode, transport, handoff, iterations: positive(values.iterations, 3), active_memory_mib: positive(values['memory-mib'], 256),
    disk_mib: positive(values['disk-mib'], 8), small_files: positive(values['small-files'], 100),
    sqlite_mib: positive(values['sqlite-mib'], 1), delay_ms: positive(values['delay-ms'], 1000),
    guest_memory_mib: values['guest-memory-mib'] ? positive(values['guest-memory-mib'], 1024) : undefined,
    roundtrip: values.roundtrip ?? false, memory_behavior: 'continuous sweep of every allocated anonymous page',
  }
  const metadata = benchmarkMetadata('hibernation-adoption', workload, target)
  metadata.environment.cache_state = evidence.cache_state
  const labels = benchmarkResourceMetadata(metadata)
  const rows: Record<string, unknown>[] = [], cleanupErrors: string[] = []
  const inventoryBefore: Record<string, unknown> = {}, inventoryAfter: Record<string, unknown> = {}
  const report: { metadata: typeof metadata; captured_configs: typeof capturedConfigs; passed: boolean;
    rows: typeof rows; cleanup_errors: string[]; inventory_before: typeof inventoryBefore;
    inventory_after: typeof inventoryAfter; error?: string } = {
    metadata, captured_configs: capturedConfigs, passed: false, rows, cleanup_errors: cleanupErrors,
    inventory_before: inventoryBefore, inventory_after: inventoryAfter,
  }
  const output = resolve(values.output)
  const save = () => { mkdirSync(dirname(output), { recursive: true }); writeFileSync(output, JSON.stringify(report, null, 2) + '\n') }
  const owned = new Set<string>()
  async function request(base: string, method: string, path: string, body?: unknown, timeoutMs = 240_000): Promise<{ status: number; value: unknown; generation: string | null }> {
    const response = await fetch(base + path, { method, headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json',
      ...(method === 'POST' && path === '/v1/sandboxes' ? { 'Idempotency-Key': randomUUID() } : {}) },
      body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(timeoutMs) })
    const raw = await response.text()
    const value: unknown = !raw ? null : response.headers.get('content-type')?.includes('application/json') ? JSON.parse(raw) : raw
    return { status: response.status, value, generation: response.headers.get('X-Sandbox-Hibernation-Generation') }
  }
  async function api(base: string, method: string, path: string, body?: unknown): Promise<unknown> {
    const result = await request(base, method, path, body)
    if (result.status < 200 || result.status >= 300) throw new Error(`${method} ${base}${path}: HTTP ${result.status}: ${JSON.stringify(result.value)}`)
    return result.value
  }
  async function inventory(base: string): Promise<SandboxRow[]> {
    const value = await api(base, 'GET', '/sandboxes')
    if (value === null) return []
    if (!Array.isArray(value)) throw new Error('invalid inventory response')
    return value.map(sandbox)
  }
  async function absent(base: string, id: string): Promise<void> {
    const result = await request(base, 'GET', `/sandboxes/${id}`)
    if (result.status !== 404) throw new Error(`sandbox ${id} remains on ${base}: HTTP ${result.status}`)
  }
  async function guest(base: string, id: string, cmd: string): Promise<string> {
    const result = object(await api(base, 'POST', `/sandboxes/${id}/exec`, { cmd }))
    if (result.exit_code !== 0 || typeof result.stdout !== 'string') throw new Error(`guest command failed on ${id}: exit ${String(result.exit_code)}`)
    return result.stdout.trim()
  }
  async function verify(base: string, id: string): Promise<unknown> {
    return JSON.parse(await guest(base, id, `node --no-warnings ${GUEST_PATH} verify ${quote(metadata.run_id)}`))
  }
  async function peerMetrics(base: string): Promise<Record<string, number>> {
    const response = await fetch(`${base}/metrics`, { headers: { authorization: `Bearer ${token}` }, signal: AbortSignal.timeout(15_000) })
    if (!response.ok) throw new Error(`metrics: HTTP ${response.status}`)
    const raw = await response.text()
    const counters: Record<string, number> = {}
    for (const line of raw.split('\n')) {
      const match = /^(sandbox_hib_peer_[a-z_]+) ([0-9.]+)$/.exec(line)
      if (match?.[1] && match[2]) counters[match[1]] = Number(match[2])
    }
    return counters
  }
  const peerBundles: Array<{ base: string; generation: string }> = []
  async function waitForPeerRemoval(base: string, generation: string): Promise<void> {
    const deadline = performance.now() + 120_000
    while (performance.now() < deadline) {
      const remaining = Math.max(1, Math.ceil(deadline - performance.now()))
      const result = await request(base, 'GET', `/internal/v1/hibernations/${generation}`, undefined, Math.min(15_000, remaining))
      if (result.status === 404) return
      if (result.status !== 200) throw new Error(`retained generation ${generation}: HTTP ${result.status}`)
      await sleep(Math.min(200, Math.max(0, deadline - performance.now())))
    }
    throw new Error(`generation ${generation} retained after 120 seconds waiting for backup and cache acknowledgment`)
  }
  async function move(from: string, to: string, id: string, row: Record<string, unknown>): Promise<void> {
    row.from = from; row.to = to
    const before = transport === 'auto' ? {} : await peerMetrics(to)
    let started = performance.now()
    const released = await request(from, 'POST', `/sandboxes/${id}/release${handoff === 'durable' ? '?durability=required' : ''}`)
    row.release_ms = elapsed(started)
    if (released.status !== 204) throw new Error(`release: HTTP ${released.status}`)
    row.peer_generation = released.generation
    if (released.generation) peerBundles.push({ base: from, generation: released.generation })
    if (transport !== 'auto' && !released.generation) throw new Error('release did not retain a peer generation')
    if (transport === 'gcs') {
      await api(from, 'DELETE', `/internal/v1/hibernations/${released.generation}`)
      if (released.generation) await waitForPeerRemoval(from, released.generation)
    }
    await absent(from, id)
    started = performance.now()
    const adopted = sandbox(await api(to, 'POST', `/sandboxes/${id}/adopt`))
    row.adopt_api_ms = elapsed(started)
    if (adopted.id !== id || adopted.status !== 'running' || adopted.metadata.benchmark_run_id !== metadata.run_id) throw new Error('adoption lost identity, running state or labels')
    row.adopted = adopted.observation
    const probeStarted = performance.now()
    if (await guest(to, id, 'echo ready') !== 'ready') throw new Error('readiness probe failed')
    row.first_probe_ms = elapsed(probeStarted)
    row.adopt_to_first_probe_ms = elapsed(started)
    started = performance.now()
    row.verification = await verify(to, id)
    row.full_verify_ms = elapsed(started)
    await sleep(workload.delay_ms)
    started = performance.now()
    row.delayed_verification = await verify(to, id)
    row.delayed_verify_ms = elapsed(started)
    if (transport !== 'auto') {
      const after = await peerMetrics(to)
      const delta = Object.fromEntries(Object.entries(after).map(([key, value]) => [key, value - (before[key] ?? 0)]))
      row.transport_counters = { before, after, delta }
      if (transport === 'peer' && !((delta.sandbox_hib_peer_chunks_total ?? 0) > 0 && (delta.sandbox_hib_peer_artifacts_total ?? 0) >= 2)) throw new Error('peer transport evidence missing')
      if (transport === 'gcs' && !((delta.sandbox_hib_peer_fallbacks_total ?? 0) > 0)) throw new Error('GCS fallback evidence missing')
    }
    await absent(from, id)
    row.passed = true
    save()
  }
  try {
    for (const base of [source, target]) {
      await observeBuilds(metadata, base, token)
      report.inventory_before[base] = (await inventory(base)).map((row) => row.observation)
    }
    save()
    const encoded = readFileSync(resolve(HERE, 'snapshot-working-set-guest.ts')).toString('base64')
    for (let iteration = 1; iteration <= workload.iterations; iteration++) {
      const row: Record<string, unknown> = { iteration, passed: false }
      rows.push(row)
      const started = performance.now()
      const vcpu = object(object(evidence.source).config).vcpus
      if (workload.guest_memory_mib !== undefined && (typeof vcpu !== 'number' || !Number.isInteger(vcpu) || vcpu < 1)) throw new Error('resource override requires captured source vcpus')
      const publicCreated = object(await api(source, 'POST', '/v1/sandboxes', {
        source: { type: 'default' }, lifecycle: { idle_timeout_seconds: 0 }, metadata: labels,
        ...(workload.guest_memory_mib === undefined ? {} : { resources: { vcpu, memory_mib: workload.guest_memory_mib } }),
      }))
      if (typeof publicCreated.id !== 'string') throw new Error('create returned no sandbox ID')
      owned.add(publicCreated.id)
      const created = sandbox(await api(source, 'GET', `/sandboxes/${publicCreated.id}`))
      if (created.metadata.benchmark_run_id !== metadata.run_id) throw new Error('create lost run labels')
      row.source = created.observation; row.create_ms = elapsed(started)
      save()
      await guest(source, created.id, `printf %s ${quote(encoded)} | base64 -d > ${GUEST_PATH}`)
      const args = [workload.active_memory_mib, workload.disk_mib, workload.small_files, workload.sqlite_mib, metadata.run_id].map((value) => quote(String(value))).join(' ')
      const setupStarted = performance.now()
      await guest(source, created.id,
        `nohup node --no-warnings ${GUEST_PATH} hold ${args} >/tmp/snapshot-working-set.log 2>&1 & ` +
        `for i in $(seq 1 600); do test -f ${ROOT}/ready && exit 0; sleep .25; done; exit 1`)
      row.setup_ms = elapsed(setupStarted)
      const outward: Record<string, unknown> = {}
      row.outward = outward
      await move(source, target, created.id, outward)
      if (workload.roundtrip) {
        const back: Record<string, unknown> = {}
        row.return = back
        await move(target, source, created.id, back)
      }
      const deleteStarted = performance.now()
      await api(workload.roundtrip ? source : target, 'DELETE', `/sandboxes/${created.id}`)
      await absent(source, created.id); await absent(target, created.id)
      owned.delete(created.id)
      row.cleanup_ms = elapsed(deleteStarted); row.passed = true
      save()
      console.log(`iteration ${iteration}/${workload.iterations} passed`)
    }
    report.passed = true
  } catch (error) {
    report.error = error instanceof Error ? error.message : String(error)
  } finally {
    // Discover a successful create whose HTTP response was lost, using this run's labels.
    for (const base of [source, target]) {
      try {
        for (const row of await inventory(base)) if (row.metadata.benchmark_run_id === metadata.run_id) owned.add(row.id)
      } catch (error) { cleanupErrors.push(`inventory ${base}: ${String(error)}`) }
    }
    for (const id of owned) {
      try {
        let found = false
        for (const base of [source, target]) {
          const current = await request(base, 'GET', `/sandboxes/${id}`)
          if (current.status === 404) continue
          if (current.status !== 200 || sandbox(current.value).metadata.benchmark_run_id !== metadata.run_id) throw new Error('cleanup could not establish ownership')
          found = true
          await api(base, 'DELETE', `/sandboxes/${id}`)
        }
        if (!found) {
          // A release may have succeeded before adoption failed. Reclaim its durable record for deletion.
          const reclaimed = sandbox(await api(source, 'POST', `/sandboxes/${id}/adopt`))
          if (reclaimed.metadata.benchmark_run_id !== metadata.run_id) throw new Error('reclaimed resource labels differ')
          await api(source, 'DELETE', `/sandboxes/${id}`)
        }
        await absent(source, id); await absent(target, id)
      } catch (error) { cleanupErrors.push(`${id}: ${String(error)}`) }
    }
    for (const peer of peerBundles) {
      try {
        const result = await request(peer.base, 'DELETE', `/internal/v1/hibernations/${peer.generation}`)
        if (result.status !== 404 && (result.status < 200 || result.status >= 300)) throw new Error(`DELETE generation: HTTP ${result.status}`)
        await waitForPeerRemoval(peer.base, peer.generation)
      }
      catch (error) { cleanupErrors.push(`peer ${peer.generation}: ${String(error)}`) }
    }
    for (const base of [source, target]) {
      try {
        report.inventory_after[base] = (await inventory(base)).map((row) => row.observation)
        await observeBuilds(metadata, base, token)
      } catch (error) { cleanupErrors.push(`final inventory ${base}: ${String(error)}`) }
    }
    report.passed &&= cleanupErrors.length === 0
    save()
  }
  console.log(`${output}: ${report.passed ? 'passed' : 'failed'}`)
  if (!report.passed) process.exitCode = 1
}

await main()
