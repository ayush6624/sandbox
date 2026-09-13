import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { mkdtemp, readFile, rm } from 'node:fs/promises'
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { setTimeout as sleep } from 'node:timers/promises'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'
import { SandboxClient } from '../src/index.js'
import { benchmarkMetadata, observeBuilds } from './metadata.js'
import { parseArgs, runBatch } from './snapshot-working-set-bench.js'

function record(value: unknown): Record<string, unknown> {
  assert.ok(value && typeof value === 'object' && !Array.isArray(value))
  return Object.fromEntries(Object.entries(value))
}

async function body(req: IncomingMessage): Promise<Record<string, unknown>> {
  let text = ''
  for await (const chunk of req) text += String(chunk)
  return record(JSON.parse(text || '{}'))
}

function json(res: ServerResponse, status: number, value: unknown): void {
  res.writeHead(status, { 'Content-Type': 'application/json' })
  res.end(JSON.stringify(value))
}

async function fixture(options: { failPause?: boolean; failDeleteOnce?: boolean; durable?: boolean; fallback?: boolean; failPoll?: boolean; gatewayBuild?: boolean; separatedKeys?: boolean } = {}) {
  const sandboxes = new Map<string, ReturnType<typeof sandbox>>()
  const events: string[] = []
  let polls = 0
  let deletions = 0
  let batch = { count: 2, sandbox: {} }
  let children: ReturnType<typeof sandbox>[] = []
  function sandbox(id: string, metadata: unknown = {}) {
    return { id, status: 'running', source: { type: 'default' }, metadata, lifecycle: {},
      resources: { vcpu: 2, memory_mib: 1024 }, created_at: new Date().toISOString() }
  }
  function operation(complete: boolean) {
    return { id: 'op', type: 'sandbox_batch_create', status: complete ? 'succeeded' : 'running',
      requested: batch.count, succeeded: complete ? batch.count : 1, failed: 0,
      created_at: new Date().toISOString(), ...(complete ? { completed_at: new Date().toISOString() } : {}),
      results: children.slice(0, complete ? batch.count : 1).map((sandbox, index) => ({ index, sandbox })) }
  }
  async function handle(req: IncomingMessage, res: ServerResponse) {
    const url = new URL(req.url ?? '/', 'http://localhost')
    const role = /^\/(source|target|gateway)(?=\/)/.exec(url.pathname)?.[1] ?? 'gateway'
    const path = url.pathname.replace(/^\/(source|target|gateway)(?=\/)/, '')
    assert.equal(req.headers.authorization, options.separatedKeys && role === 'gateway' ? 'Bearer fixture-gateway-key' : 'Bearer fixture-key')
    assert.equal(req.headers['x-sandbox-snapshot-peer'], undefined, 'public benchmark injected a private routing hint')
    if (path === '/metrics/hosts') {
      res.end('sandbox_build_info{component="worker",release="fixture-release",host="worker-1"} 1\nsandbox_host_scrape_ok{host="worker-2"} 0\n')
    } else if (path === '/metrics') {
      const active = children.length > 0 ? 1 : 0
      res.end(`sandbox_build_info{component="${options.gatewayBuild ? 'gateway' : 'worker'}",release="fixture-release"} 1\n` +
        `sandbox_snapshot_peer_pulls_total ${options.fallback ? 0 : active}\n` +
        `sandbox_snapshot_peer_pull_failures_total 0\n` +
        `sandbox_snapshot_gcs_fallbacks_total ${options.fallback ? active : 0}\n` +
        `sandbox_snapshot_peer_payload_bytes_total ${active * 100}\n`)
    } else if (path === '/v1/sandboxes' && req.method === 'POST') {
      const input = await body(req)
      const id = record(input.metadata ?? {}).benchmark_role === 'filler' ? `filler-${sandboxes.size}` : 'source'
      const value = sandbox(id, input.metadata)
      sandboxes.set(value.id, value)
      json(res, 201, value)
    } else if (path === '/v1/sandboxes' && req.method === 'GET') {
      json(res, 200, { sandboxes: [...sandboxes.values()] })
    } else if (path === '/v1/sandbox-batches') {
      assert.equal(role, 'gateway')
      events.push('batch-request')
      const input = await body(req)
      assert.equal(typeof input.count, 'number')
      batch = { count: Number(input.count), sandbox: record(input.sandbox) }
      const metadata = record(batch.sandbox).metadata
      children = Array.from({ length: batch.count }, (_, i) => sandbox(`child-${i}`, metadata))
      children.forEach((child) => sandboxes.set(child.id, child))
      json(res, 202, operation(false))
    } else if (path === '/v1/operations/op') {
      polls++
      if (polls === 3) events.push('operation-complete')
      if (options.failPoll && polls === 3) json(res, 400, { code: 'poll_failed', detail: 'poll failed', status: 400 })
      else json(res, 200, operation(polls >= 3))
    } else if (path.endsWith('/snapshots') && req.method === 'POST') {
      if (options.separatedKeys) assert.equal(role, 'gateway', 'snapshot must publish ownership through the gateway')
      events.push('snapshot')
      json(res, 201, { id: 'snap', state: options.durable ? 'durable' : 'local', source_sandbox_id: 'source', created_at: new Date().toISOString() })
    } else if (path === '/v1/snapshots/snap' && req.method === 'GET') {
      events.push('snapshot-state')
      json(res, 200, { id: 'snap', state: options.durable ? 'durable' : 'local', source_sandbox_id: 'source', created_at: new Date().toISOString() })
    } else if (path === '/v1/snapshots/snap' && req.method === 'DELETE') {
      res.writeHead(204).end()
    } else if (path.endsWith('/exec')) {
      const input = await body(req)
      const cmd = String(input.cmd)
      const id = path.split('/')[2]
      events.push(`exec:${id}`)
      const stdout = cmd.includes('verify') ? JSON.stringify({ runId: 'fixture-run', memoryCycleBefore: 1, memoryCycleAfter: 2 })
        : cmd.includes('working-set-ready') ? 'working-set-ready' : cmd === 'echo ready' ? 'ready' : 'benchmark-ready'
      json(res, 200, { stdout, stderr: '', exit_code: 0, duration_ms: 1 })
    } else if (path.endsWith(':pause') || path.endsWith(':resume')) {
      if (options.failPause && path.endsWith(':pause')) json(res, 400, { code: 'fixture_pause_failure', detail: 'pause failed', status: 400 })
      else json(res, 200, sandbox('source'))
    } else if (/^\/v1\/sandboxes\/[^/]+$/.test(path)) {
      const id = path.split('/').at(-1)!
      if (req.method === 'DELETE') {
        events.push(`delete:${id}`)
        deletions++
        if (options.failDeleteOnce && deletions === 1) json(res, 400, { code: 'delete_failed', detail: 'delete failed', status: 400 })
        else { await sleep(150); sandboxes.delete(id); res.writeHead(204).end() }
      } else if (sandboxes.has(id)) json(res, 200, sandboxes.get(id))
      else json(res, 404, { code: 'not_found', status: 404 })
    } else throw new Error(`unhandled ${req.method} ${path}`)
  }
  const server = createServer((req, res) => { handle(req, res).catch((error: unknown) => json(res, 500, { error: String(error) })) })
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
  const address = server.address()
  assert.ok(address && typeof address === 'object')
  return { url: `http://127.0.0.1:${address.port}`, events, sandboxes,
    close: () => new Promise<void>((resolve, reject) => { server.close((error) => error ? reject(error) : resolve()); server.closeAllConnections() }) }
}

async function run(script: string, args: string[], url: string) {
  const directory = await mkdtemp(join(tmpdir(), 'benchmark-test-'))
  const output = join(directory, 'report.json')
  const child = spawn(process.execPath, ['--import', 'tsx', fileURLToPath(new URL(script, import.meta.url)), ...args, '--output', output], {
    env: { ...process.env, SANDBOX_API_URL: url, SANDBOX_API_KEY: 'fixture-key', SANDBOX_CONTROL_KEY: 'fixture-key',
      BENCH_RUN_ID: 'fixture-run', SANDBOX_RELEASE: 'fixture-release' },
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  let stderr = ''
  child.stderr.on('data', (chunk) => { stderr += String(chunk) })
  child.stdout.resume()
  const timer = setTimeout(() => child.kill('SIGKILL'), 15_000)
  try {
    const code = await new Promise<number | null>((resolve, reject) => {
      child.on('error', reject)
      child.on('exit', resolve)
    })
    const report = record(JSON.parse(await readFile(output, 'utf8')))
    return { code, report, stderr }
  } finally {
    clearTimeout(timer)
    child.kill()
    await rm(directory, { recursive: true, force: true })
  }
}

test('template probes early children before the batch completes and excludes cleanup', async () => {
  const api = await fixture()
  try {
    const result = await run('./template-warm-bench.ts', ['--template-id', 'template', '--count', '2', '--rounds', '1'], api.url)
    assert.equal(result.code, 0, result.stderr)
    assert.equal(result.report.passed, true)
    assert.ok(api.events.indexOf('exec:child-0') < api.events.indexOf('operation-complete'))
    assert.ok(Array.isArray(result.report.rows))
    const row = record(result.report.rows[0])
    assert.ok(Number(row.command_probe_ms) < Number(row.cleanup_ms))
    assert.ok(Number(row.cleanup_ms) >= 150)
    assert.equal(api.sandboxes.size, 0)
  } finally { await api.close() }
})

test('lifecycle preserves partial samples, failed stage, and cleanup failure', async () => {
  const api = await fixture({ failPause: true, failDeleteOnce: true })
  try {
    const result = await run('./lifecycle-bench.ts', ['--iterations', '2'], api.url)
    assert.equal(result.code, 1)
    assert.equal(result.report.passed, false)
    assert.equal(result.report.completed, 0)
    assert.equal(result.report.pause, null)
    assert.ok(Array.isArray(result.report.failures))
    assert.equal(record(result.report.failures[0]).stage, 'pause')
    assert.ok(Array.isArray(result.report.cleanupErrors) && result.report.cleanupErrors.length > 0)
    assert.ok(record(result.report.create_ready).samples)
    assert.equal(api.sandboxes.size, 0)
  } finally { await api.close() }
})

test('working-set probes and hydrates each child before slower siblings appear', async () => {
  const api = await fixture()
  try {
    const client = new SandboxClient({ baseUrl: api.url, apiKey: 'fixture-key' })
    const row = await runBatch(client, 'snap', 2, 1, parseArgs([]), 'fixture-run', {}, new Map())
    assert.equal(row.ok, 2)
    assert.ok(row.items[0]?.commandReadyMs !== undefined && row.items[0].commandReadyMs < row.operationWallMs)
    assert.ok(row.items[0]?.hydrationMs !== undefined && row.items[0].hydrationMs < row.operationWallMs)
    assert.equal(api.sandboxes.size, 0)
  } finally { await api.close() }
})

for (const scenario of ['peer', 'already-durable', 'fallback'] as const) {
  test(`gateway upload-pending benchmark verifies ${scenario} through the public API`, async () => {
    const api = await fixture({ durable: scenario === 'already-durable', fallback: scenario === 'fallback', separatedKeys: true })
    try {
      const result = await run('./snapshot-peer-transfer-bench.ts', [
        '--source-url', `${api.url}/source`, '--target-url', `${api.url}/target`,
        '--gateway-url', `${api.url}/gateway`, '--modes', 'gateway-pending', '--counts', '2', '--rounds', '1',
        '--gateway-api-key', 'fixture-gateway-key',
        '--source-fillers', '1',
      ], api.url)
      assert.equal(result.code, scenario === 'peer' ? 0 : 1, result.stderr)
      assert.equal(result.report.passed, scenario === 'peer')
      assert.ok(api.events.includes('delete:filler-0'))
      assert.ok(Array.isArray(result.report.rows))
      const row = record(result.report.rows[0])
      assert.equal(row.mode, 'gateway-pending')
      if (scenario === 'peer') {
        assert.equal(row.snapshotStateAtRequest, 'local')
        assert.equal(row.peerPulls, 1)
        assert.equal(row.peerPullFailures, 0)
        assert.equal(row.gcsFallbacks, 0)
        assert.ok(Array.isArray(row.items) && row.items.every((item: unknown) => record(item).destinationVerified === true))
        assert.ok(api.events.indexOf('batch-request') < api.events.indexOf('delete:source'))
      } else if (scenario === 'already-durable') assert.equal(api.events.includes('batch-request'), false)
      assert.equal(api.sandboxes.size, 0)
    } finally { await api.close() }
  })
}


test('working-set does not pass a lost operation even when every observed child works', async () => {
  const api = await fixture({ failPoll: true })
  try {
    const client = new SandboxClient({ baseUrl: api.url, apiKey: 'fixture-key' })
    const row = await runBatch(client, 'snap', 1, 1, parseArgs([]), 'fixture-run', {}, new Map())
    assert.equal(row.ok, 1)
    assert.equal(row.passed, false)
    assert.ok(row.operationError)
  } finally { await api.close() }
})

test('gateway build collection retains worker identities and failed scrape evidence', async () => {
  const api = await fixture({ gatewayBuild: true })
  try {
    const metadata = benchmarkMetadata('fixture', {}, api.url)
    await observeBuilds(metadata, api.url, 'fixture-key')
    assert.equal(metadata.builds.length, 2)
    assert.equal(metadata.builds[0]?.releases[0]?.component, 'gateway')
    assert.equal(metadata.builds[1]?.releases[0]?.host, 'worker-1')
    assert.match(metadata.builds[1]?.error ?? '', /workers did not answer/)
  } finally { await api.close() }
})
