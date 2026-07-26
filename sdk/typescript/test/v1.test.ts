import assert from 'node:assert/strict'
import http from 'node:http'
import type { AddressInfo } from 'node:net'
import { after, before, test } from 'node:test'

import { NotFoundError, SandboxClient } from '../src/index.js'

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
    json(res, 201, { id: 'port-1', sandbox_id: portMatch[1], guest_port: input.guest_port, host_port: 5200, status: 'active' })
    return
  }
  if (portMatch && req.method === 'GET') {
    json(res, 200, { port_forwards: [{ id: 'port-1', sandbox_id: portMatch[1], guest_port: 3000, host_port: 5200, status: 'active' }] })
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

  if (req.method === 'GET' && url.pathname === '/v1/templates') {
    json(res, 200, { templates: [{ id: 'default', revision: 'rev-1', resources: { vcpu: 2, memory_mib: 1024 } }] })
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
  await first.pause()
  assert.equal(first.status, 'paused')
  await first.resume()
  assert.equal(first.status, 'running')
  await first.update({ name: 'renamed', metadata: { run: '2' } })
  assert.equal(first.name, 'renamed')
  assert.equal((await first.createSnapshot({ name: 'ready' })).name, 'ready')
  assert.equal((await first.createPortForward(3000)).hostPort, 5200)
  assert.equal((await first.listPortForwards())[0]?.guestPort, 3000)
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
  for await (const template of client.templates.list()) templates.push(template.id)
  assert.deepEqual(templates, ['default'])
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

test('problem details expose stable code and request id', async () => {
  const client = new SandboxClient({ baseUrl, apiKey: API_KEY })
  await assert.rejects(
    client.sandboxes.get('missing'),
    (error: unknown) => error instanceof NotFoundError && error.code === 'not_found' && error.requestId === 'req-test',
  )
})
