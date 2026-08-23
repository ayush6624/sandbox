import assert from 'node:assert/strict'
import http from 'node:http'
import type { AddressInfo } from 'node:net'
import { after, before, test } from 'node:test'

import { NotFoundError, Sandbox, SandboxClient } from '../src/index.js'

const API_KEY = 'v1-test-key'
const sandboxes = new Map<string, Record<string, unknown>>()
const mutationKeys: string[] = []
let createAttempts = 0
let operationPolls = 0
let server: http.Server
let baseUrl: string

function problem(res: http.ServerResponse, status: number, code: string, detail: string): void {
  res.writeHead(status, { 'Content-Type': 'application/problem+json', 'X-Request-Id': 'req-test' })
  res.end(JSON.stringify({
    type: `https://sandbox.dev/problems/${code}`,
    title: http.STATUS_CODES[status],
    status,
    detail,
    instance: '/v1/test',
    code,
    request_id: 'req-test',
  }))
}

function json(res: http.ServerResponse, status: number, body: unknown): void {
  res.writeHead(status, { 'Content-Type': 'application/json', 'X-Request-Id': 'req-test' })
  res.end(JSON.stringify(body))
}

async function body(req: http.IncomingMessage): Promise<Record<string, any>> {
  const chunks: Buffer[] = []
  for await (const chunk of req) chunks.push(Buffer.from(chunk))
  return chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : {}
}

function sandbox(id: string, input: Record<string, any> = {}): Record<string, unknown> {
  return {
    id,
    name: input.name,
    status: 'running',
    source: input.source ?? { type: 'default' },
    lifecycle: input.lifecycle ?? {},
    resources: input.resources ?? { vcpu: 2, memory_mib: 1024 },
    metadata: input.metadata ?? {},
    created_at: '2026-07-26T00:00:00Z',
  }
}

async function handle(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
  const url = new URL(req.url ?? '/', 'http://localhost')
  if (req.headers.authorization !== `Bearer ${API_KEY}`) {
    problem(res, 401, 'unauthorized', 'bad key')
    return
  }
  if (['POST', 'PATCH', 'DELETE'].includes(req.method ?? '')) {
    const key = String(req.headers['idempotency-key'] ?? '')
    assert.ok(key, 'v1 mutation omitted Idempotency-Key')
    mutationKeys.push(key)
  }

  if (req.method === 'POST' && url.pathname === '/v1/sandboxes') {
    const input = await body(req)
    createAttempts++
    if (input.name === 'retry' && createAttempts === 1) {
      res.setHeader('Retry-After', '0')
      problem(res, 503, 'capacity_unavailable', 'try again')
      return
    }
    const id = `sandbox-${sandboxes.size + 1}`
    const value = sandbox(id, input)
    sandboxes.set(id, value)
    json(res, 201, value)
    return
  }

  if (req.method === 'GET' && url.pathname === '/v1/sandboxes') {
    const values = [...sandboxes.values()]
    const offset = url.searchParams.get('page_token') ? 1 : 0
    json(res, 200, {
      sandboxes: values.slice(offset, offset + 1),
      ...(offset + 1 < values.length ? { next_page_token: 'next' } : {}),
    })
    return
  }

  const sandboxMatch = url.pathname.match(/^\/v1\/sandboxes\/([^/:]+)$/)
  if (sandboxMatch) {
    const id = sandboxMatch[1]!
    const current = sandboxes.get(id)
    if (!current) {
      problem(res, 404, 'not_found', 'sandbox missing')
      return
    }
    if (req.method === 'GET') json(res, 200, current)
    else if (req.method === 'PATCH') {
      const patch = await body(req)
      const updated = { ...current, ...patch }
      sandboxes.set(id, updated)
      json(res, 200, updated)
    } else if (req.method === 'DELETE') {
      sandboxes.delete(id)
      res.writeHead(204, { 'X-Request-Id': 'req-test' })
      res.end()
    }
    return
  }

  const actionMatch = url.pathname.match(/^\/v1\/sandboxes\/([^/]+):(pause|resume)$/)
  if (req.method === 'POST' && actionMatch) {
    const id = actionMatch[1]!
    const current = sandboxes.get(id)!
    const updated = { ...current, status: actionMatch[2] === 'pause' ? 'paused' : 'running' }
    sandboxes.set(id, updated)
    json(res, 200, updated)
    return
  }

  const snapshotMatch = url.pathname.match(/^\/v1\/sandboxes\/([^/]+)\/snapshots$/)
  if (req.method === 'POST' && snapshotMatch) {
    const input = await body(req)
    json(res, 201, {
      id: 'snapshot-1', name: input.name, source_sandbox_id: snapshotMatch[1], state: 'durable',
      created_at: '2026-07-26T00:01:00Z',
    })
    return
  }

  const portMatch = url.pathname.match(/^\/v1\/sandboxes\/([^/]+)\/port-forwards$/)
  if (portMatch && req.method === 'POST') {
    const input = await body(req)
    const base = {
      id: 'port-1', sandbox_id: portMatch[1], guest_port: input.guest_port, status: 'active',
      url: `https://${input.guest_port}-${portMatch[1]}.sandboxes.example.com`,
    }
    if (input.mode === 'raw') {
      json(res, 201, {
        id: 'port-raw', sandbox_id: portMatch[1], guest_port: input.guest_port,
        mode: 'raw', public_host: 'sbx.example.com', public_port: 20002, status: 'active',
      })
      return
    }
    // A URL-only exposure has no worker-local host port, and the server omits
    // the field rather than reporting an undialable 0.
    json(res, 201, input.host_port === false
      ? { ...base, mode: 'url' }
      : { ...base, mode: 'both', host_port: 5200 })
    return
  }
  if (portMatch && req.method === 'GET') {
    json(res, 200, { port_forwards: [
      {
        id: 'port-1', sandbox_id: portMatch[1], guest_port: 3000, host_port: 5200,
        mode: 'both', url: `https://3000-${portMatch[1]}.sandboxes.example.com`, status: 'active',
      },
      {
        id: 'port-2', sandbox_id: portMatch[1], guest_port: 8080,
        mode: 'url', url: `https://8080-${portMatch[1]}.sandboxes.example.com`, status: 'active',
      },
      {
        id: 'port-raw', sandbox_id: portMatch[1], guest_port: 22,
        mode: 'raw', public_host: 'sbx.example.com', public_port: 20002, status: 'active',
      },
    ] })
    return
  }

  if (req.method === 'POST' && url.pathname === '/v1/sandbox-batches') {
    const input = await body(req)
    json(res, 202, {
      id: 'operation-1', type: 'sandbox_batch_create', status: 'running', requested: input.count,
      succeeded: 0, failed: 0, created_at: '2026-07-26T00:02:00Z',
    })
    return
  }
  if (req.method === 'GET' && url.pathname === '/v1/operations/operation-1') {
    operationPolls++
    json(res, 200, {
      id: 'operation-1', type: 'sandbox_batch_create', status: 'partially_succeeded', requested: 2,
      succeeded: 1, failed: 1, created_at: '2026-07-26T00:02:00Z', completed_at: '2026-07-26T00:02:01Z',
      results: [
        { index: 0, sandbox: sandbox('batch-1') },
        { index: 1, error: { type: 'https://sandbox.dev/problems/capacity_unavailable', title: 'Service Unavailable', status: 503, code: 'capacity_unavailable', request_id: 'req-batch' } },
      ],
    })
    return
  }

  // Two intervals for one sandbox: a VM that was paused, and the VM the resume
  // started. The pause between them bills nothing, which is why there are two
  // rows rather than one long span.
  const usageIntervals = [
    {
      id: 'sandbox-1:2', sandbox_id: 'sandbox-1', sequence: 2,
      state: 'open', resources: { vcpu: 2, memory_mib: 1024 },
      started_at: '2026-07-26T00:30:00Z',
      duration_seconds: 300, vcpu_seconds: 600, memory_mib_seconds: 307200, cpu_seconds: 42.5,
      metadata: { run: '1' },
    },
    {
      id: 'sandbox-1:1', sandbox_id: 'sandbox-1', sequence: 1,
      state: 'closed', resources: { vcpu: 2, memory_mib: 1024 },
      started_at: '2026-07-26T00:00:00Z', ended_at: '2026-07-26T00:10:00Z',
      duration_seconds: 600, vcpu_seconds: 1200, memory_mib_seconds: 614400, cpu_seconds: 90,
      end_reason: 'hibernate', metadata: { run: '1' },
    },
  ]
  function usageReport(offset: number): Record<string, unknown> {
    const size = Number(url.searchParams.get('page_size') ?? usageIntervals.length)
    return {
      intervals: usageIntervals.slice(offset, offset + size),
      // Totals cover the whole selection, not the page — that is the property
      // a caller must be able to rely on when paging.
      totals: {
        intervals: 2, open_intervals: 1, duration_seconds: 900,
        vcpu_seconds: 1800, memory_mib_seconds: 921600, cpu_seconds: 132.5,
      },
      window: { selection: 'overlap', ...(url.searchParams.get('from') ? { from: url.searchParams.get('from') } : {}) },
      coverage: { hosts_reporting: 1, scope: 'live_hosts', truncated: false },
      ...(offset + size < usageIntervals.length ? { next_page_token: 'next' } : {}),
    }
  }
  if (req.method === 'GET' && url.pathname === '/v1/usage') {
    json(res, 200, usageReport(url.searchParams.get('page_token') ? 1 : 0))
    return
  }
  const usageMatch = url.pathname.match(/^\/v1\/sandboxes\/([^/]+)\/usage$/)
  if (req.method === 'GET' && usageMatch) {
    if (!sandboxes.has(usageMatch[1]!)) {
      problem(res, 404, 'sandbox_not_found',
        'sandbox not found; usage for a deleted sandbox is available from GET /v1/usage?sandbox_id=')
      return
    }
    json(res, 200, usageReport(0))
    return
  }

  if (req.method === 'GET' && url.pathname === '/v1/templates') {
    json(res, 200, { templates: [{ id: 'default', revision: 'rev-1', warm_target: 8, resources: { vcpu: 2, memory_mib: 1024 } }] })
    return
  }
  if (req.method === 'PATCH' && url.pathname === '/v1/templates/template-py') {
    const input = await body(req)
    json(res, 200, { id: 'template-py', revision: 'rev-py', warm_target: input.warm_target, resources: { vcpu: 4, memory_mib: 2048 } })
    return
  }
  problem(res, 404, 'not_found', 'resource missing')
}

before(async () => {
  server = http.createServer((req, res) => { void handle(req, res) })
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
  baseUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}`
})

after(async () => new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve())))

test('configured client exposes standard resources and lifecycle methods', async () => {
  sandboxes.clear()
  mutationKeys.length = 0
  createAttempts = 0
  const client = new SandboxClient({ baseUrl, apiKey: API_KEY })
  const first = await client.sandboxes.create({
    source: { templateId: 'default' }, ttlMs: 10_001, idleTimeoutMs: 5_001,
    metadata: { run: '1' }, resources: { vcpus: 2, memoryMib: 1024 },
  })
  assert.equal(first.source.type, 'template')
  assert.equal(first.ttlMs, 11_000)
  assert.equal(first.resources.memoryMib, 1024)
  assert.deepEqual(first.sshInstructions, {
    command: 'sandbox ssh sandbox-1',
    description: 'Install the sandbox CLI, set SANDBOX_API_URL and SANDBOX_API_KEY, then run this command.',
  })
  await first.pause()
  assert.equal(first.status, 'paused')
  await first.resume()
  assert.equal(first.status, 'running')
  await first.update({ name: 'renamed', metadata: { run: '2' } })
  assert.equal(first.name, 'renamed')
  assert.equal((await first.createSnapshot({ name: 'ready' })).name, 'ready')
  assert.equal((await first.createPortForward(3000)).hostPort, 5200)
  assert.equal((await first.listPortForwards())[0]?.guestPort, 3000)

  // Public ingress: the URL rides along with an ordinary host-port exposure,
  // and `hostPort: false` asks for a URL-only one that reserves no host port.
  const both = await first.createPortForward(3000)
  assert.equal(both.mode, 'both')
  assert.equal(both.url, 'https://3000-sandbox-1.sandboxes.example.com')
  const urlOnly = await first.createPortForward(8080, { hostPort: false })
  assert.equal(urlOnly.mode, 'url')
  assert.equal(urlOnly.hostPort, undefined)
  assert.equal(urlOnly.url, 'https://8080-sandbox-1.sandboxes.example.com')

  const raw = await first.createRawPortForward(22)
  assert.equal(raw.mode, 'raw')
  assert.equal(raw.publicHost, 'sbx.example.com')
  assert.equal(raw.publicPort, 20002)
  assert.equal(raw.address, 'sbx.example.com:20002')
  const rawFromCollection = await client.portForwards.createRaw('sandbox-1', 22)
  assert.equal(rawFromCollection.address, 'sbx.example.com:20002')
  const rawFromGeneric = await client.portForwards.create('sandbox-1', 22, { mode: 'raw' })
  assert.equal(rawFromGeneric.address, 'sbx.example.com:20002')
  await assert.rejects(
    first.createPortForward(22, { mode: 'raw', hostPort: true }),
    (error: unknown) => error instanceof TypeError && error.message.includes('cannot be combined'),
  )

  const listed = await first.listPortForwards()
  assert.equal(listed[1]?.mode, 'url')
  assert.equal(listed[1]?.hostPort, undefined)
  assert.equal(listed[1]?.url, 'https://8080-sandbox-1.sandboxes.example.com')
  assert.equal(listed[2]?.address, 'sbx.example.com:20002')
  await first.terminate()
  assert.ok(mutationKeys.every(Boolean))
})

test('pagination is an AsyncIterable and safe retries reuse idempotency keys', async () => {
  sandboxes.clear()
  mutationKeys.length = 0
  createAttempts = 0
  const client = new SandboxClient({ baseUrl, apiKey: API_KEY, maxRetries: 1 })
  await client.sandboxes.create({ name: 'retry' })
  assert.equal(createAttempts, 2)
  assert.equal(mutationKeys[0], mutationKeys[1])
  await client.sandboxes.create({ name: 'second' })
  const listed = []
  for await (const item of client.sandboxes.list({ pageSize: 1 })) listed.push(item.id)
  assert.deepEqual(listed, ['sandbox-1', 'sandbox-2'])
  const templates = []
  for await (const template of client.templates.list()) templates.push(template)
  assert.deepEqual(templates.map((template) => template.id), ['default'])
  assert.equal(templates[0]?.warmTarget, 8)
  const warmed = await client.templates.updateWarmTarget('template-py', 3)
  assert.equal(warmed.warmTarget, 3)
  assert.equal(warmed.resources.memoryMib, 2048)
  await assert.rejects(client.templates.updateWarmTarget('template-py', -1), /non-negative integer/)
})

test('batch operations poll and retain indexed per-item errors', async () => {
  operationPolls = 0
  const client = new SandboxClient({ baseUrl, apiKey: API_KEY })
  const operation = await client.sandboxes.createMany({ count: 2, source: { snapshotId: 'snapshot-1' } })
  const completed = await operation.wait({ pollIntervalMs: 1, timeoutMs: 1_000 })
  assert.equal(operationPolls, 1)
  assert.equal(completed.status, 'partially_succeeded')
  assert.equal(completed.results[0]?.value?.id, 'batch-1')
  assert.equal(completed.results[1]?.error?.code, 'capacity_unavailable')
})

test('static facade supports source creation and createMany migration', async () => {
  sandboxes.clear()
  const created = await Sandbox.createFromSource(
    { snapshotId: 'snapshot-1' },
    { apiUrl: baseUrl, apiKey: API_KEY, ttlMs: 2_000 },
  )
  assert.equal(created.source.type, 'snapshot')
  const operation = await Sandbox.createMany({
    apiUrl: baseUrl,
    apiKey: API_KEY,
    count: 2,
    source: { snapshotId: 'snapshot-1' },
  })
  assert.equal((await operation.wait({ pollIntervalMs: 1 })).requested, 2)
})

test('problem details expose stable code and request id', async () => {
  const client = new SandboxClient({ baseUrl, apiKey: API_KEY })
  await assert.rejects(
    client.sandboxes.get('missing'),
    (error: unknown) => error instanceof NotFoundError && error.code === 'not_found' && error.requestId === 'req-test',
  )
})

test('usage separates billed quantities from recorded CPU', async () => {
  const client = new SandboxClient({ baseUrl, apiKey: API_KEY })
  const report = await client.usage.report({ from: new Date('2026-07-26T00:00:00Z') })

  // Newest first, so index 1 is the older interval that has already closed.
  const [open, closed] = [report.intervals[0]!, report.intervals[1]!]
  assert.equal(open.state, 'open')
  assert.equal(open.endedAt, undefined)
  assert.equal(closed.state, 'closed')
  assert.deepEqual(closed.endedAt, new Date('2026-07-26T00:10:00Z'))
  assert.equal(closed.endReason, 'hibernate')
  assert.equal(closed.vcpuSeconds, 1200)
  assert.equal(closed.memoryMibSeconds, 614400)
  assert.equal(closed.cpuSeconds, 90)
  assert.deepEqual(closed.resources, { vcpus: 2, memoryMib: 1024 })
  assert.equal(report.window.selection, 'overlap')
  assert.equal(report.coverage.scope, 'live_hosts')
  assert.equal(report.coverage.hostsReporting, 1)
  // Which worker ran a sandbox is infrastructure, not billing.
  assert.equal(JSON.stringify(report).includes('worker-1'), false)
})

// Paging the rows must not page the money: the totals describe the selection.
test('usage totals stay whole while intervals page', async () => {
  const client = new SandboxClient({ baseUrl, apiKey: API_KEY })
  const first = await client.usage.report({ pageSize: 1 })
  assert.equal(first.intervals.length, 1)
  assert.equal(first.totals.intervals, 2)
  assert.equal(first.totals.vcpuSeconds, 1800)
  assert.ok(first.nextPageToken)

  const seen: string[] = []
  for await (const interval of client.usage.list({ pageSize: 1 })) seen.push(interval.id)
  assert.deepEqual(seen, ['sandbox-1:2', 'sandbox-1:1'])
})

test('per-sandbox usage reads from the sandbox, and 404 points at the fleet ledger', async () => {
  sandboxes.clear()
  const client = new SandboxClient({ baseUrl, apiKey: API_KEY })
  const created = await client.sandboxes.create({})
  const report = await created.usage()
  assert.equal(report.totals.openIntervals, 1)
  assert.equal(report.intervals[0]!.sandboxId, 'sandbox-1')

  await created.terminate()
  await assert.rejects(
    client.usage.forSandbox('sandbox-1'),
    (error: unknown) => error instanceof NotFoundError && /\/v1\/usage\?sandbox_id=/.test(error.message),
  )
})
