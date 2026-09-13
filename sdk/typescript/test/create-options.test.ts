import assert from 'node:assert/strict'
import http from 'node:http'
import { test } from 'node:test'
import type { TestContext } from 'node:test'

import { SandboxClient } from '../src/index.js'
import type { CreateSandboxOptions, SandboxResourceOverrides, SandboxResources } from '../src/index.js'
import type { components } from '../src/generated/api-v1.js'

const sandbox = {
  id: 'options-sandbox', status: 'running', source: { type: 'default' },
  lifecycle: { idle_timeout_seconds: -1 }, resources: { vcpu: 2, memory_mib: 1024 },
  metadata: {}, created_at: '2026-09-13T10:00:00Z',
} satisfies components['schemas']['Sandbox']
const receipt = {
  id: 'options-operation', type: 'sandbox_create', status: 'pending',
  requested: 1, succeeded: 0, failed: 0, created_at: sandbox.created_at,
} satisfies components['schemas']['Operation']

async function serve(t: TestContext) {
  const requests: Array<{ method: string | undefined; url: string | undefined; body: string; key: unknown }> = []
  const server = http.createServer(async (req, res) => {
    let body = ''
    for await (const chunk of req) body += chunk
    requests.push({ method: req.method, url: req.url, body, key: req.headers['idempotency-key'] })
    res.setHeader('Content-Type', 'application/json')
    if (req.url === '/v1/sandbox-creations') {
      res.writeHead(202)
      res.end(JSON.stringify(receipt))
    } else if (req.url === '/v1/sandbox-batches') {
      res.writeHead(202)
      res.end(JSON.stringify({ ...receipt, type: 'sandbox_batch_create', requested: 2 }))
    } else {
      res.end(JSON.stringify(sandbox))
    }
  })
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', resolve)
  })
  t.after(() => new Promise<void>((resolve, reject) => {
    server.close((error) => error ? reject(error) : resolve())
    server.closeAllConnections()
  }))
  const address = server.address()
  assert.ok(address && typeof address !== 'string')
  return { client: new SandboxClient({ baseUrl: `http://127.0.0.1:${address.port}`, apiKey: 'options-test', maxRetries: 0 }), requests }
}

const resourceCases: Array<{ name: string; resources?: SandboxResourceOverrides; wire?: components['schemas']['ResourceOverrides'] }> = [
  { name: 'omitted' },
  { name: 'empty', resources: {}, wire: {} },
  { name: 'CPU only', resources: { vcpus: 4 }, wire: { vcpu: 4 } },
  { name: 'memory only', resources: { memoryMib: 128 }, wire: { memory_mib: 128 } },
  { name: 'both', resources: { vcpus: 4, memoryMib: 2048 }, wire: { vcpu: 4, memory_mib: 2048 } },
  { name: 'zeros', resources: { vcpus: 0, memoryMib: 0 }, wire: { vcpu: 0, memory_mib: 0 } },
]
for (const entry of resourceCases) {
  test(`create sends ${entry.name} resources and reads complete response resources`, async (t) => {
    const { client, requests } = await serve(t)
    const result = await client.sandboxes.create({ resources: entry.resources, idleTimeoutMs: -1 })
    assert.equal(requests[0]?.url, '/v1/sandboxes')
    assert.equal(requests[0]?.body, JSON.stringify({ resources: entry.wire, lifecycle: { idle_timeout_seconds: -1 } }))
    const actual: SandboxResources = result.resources
    assert.deepEqual(actual, { vcpus: 2, memoryMib: 1024 })
    assert.equal(result.idleTimeoutMs, -1)
  })
}

test('async and batch create preserve partial resources and the idle sentinel', async (t) => {
  const { client, requests } = await serve(t)
  await client.sandboxes.createAsync({ resources: { vcpus: 3 }, idleTimeoutMs: -1, idempotencyKey: 'async-key' })
  await client.sandboxes.createMany({ count: 2, resources: { memoryMib: 512 }, idleTimeoutMs: -1, idempotencyKey: 'batch-key' })
  assert.deepEqual(requests, [
    { method: 'POST', url: '/v1/sandbox-creations', body: '{"resources":{"vcpu":3},"lifecycle":{"idle_timeout_seconds":-1}}', key: 'async-key' },
    { method: 'POST', url: '/v1/sandbox-batches', body: '{"count":2,"sandbox":{"resources":{"memory_mib":512},"lifecycle":{"idle_timeout_seconds":-1}}}', key: 'batch-key' },
  ])
})

test('full create payload retains field order and caller key across replay', async (t) => {
  const { client, requests } = await serve(t)
  const options: CreateSandboxOptions = {
    name: 'ordered', source: { type: 'default' }, metadata: { z: 'last', a: 'first' },
    resources: { memoryMib: 1024, vcpus: 2 }, ttlMs: 1001, idleTimeoutMs: 2001, idempotencyKey: 'stable-key',
  }
  await client.sandboxes.createAsync(options)
  await client.sandboxes.createAsync(options)
  const expected = '{"name":"ordered","source":{"type":"default"},"metadata":{"z":"last","a":"first"},"resources":{"vcpu":2,"memory_mib":1024},"lifecycle":{"ttl_seconds":2,"idle_timeout_seconds":3}}'
  assert.deepEqual(requests.map(({ body, key }) => ({ body, key })), [{ body: expected, key: 'stable-key' }, { body: expected, key: 'stable-key' }])
})

test('idle updates pass the sentinel and retain zero and ceiling conversion', async (t) => {
  const { client, requests } = await serve(t)
  const result = await client.sandboxes.get(sandbox.id)
  for (const [milliseconds, seconds] of [[-1, -1], [0, 0], [0.5, 1], [1001, 2]]) {
    await result.update({ idleTimeoutMs: milliseconds })
    assert.equal(requests.at(-1)?.method, 'PATCH')
    assert.equal(requests.at(-1)?.body, JSON.stringify({ lifecycle: { idle_timeout_seconds: seconds } }))
    assert.equal(result.idleTimeoutMs, -1)
    await client.sandboxes.create({ idleTimeoutMs: milliseconds })
    assert.equal(requests.at(-1)?.body, JSON.stringify({ lifecycle: { idle_timeout_seconds: seconds } }))
  }
  await result.update({ ttlMs: 1001 })
  assert.equal(requests.at(-1)?.body, '{"lifecycle":{"ttl_seconds":2}}')
})

test('invalid resource and lifecycle options reject before any mutation', async (t) => {
  const { client, requests } = await serve(t)
  const result = await client.sandboxes.get(sandbox.id)
  const invalid: CreateSandboxOptions[] = [
    ...[-2, -1, 0.5, NaN, Infinity, -Infinity].map((vcpus) => ({ resources: { vcpus } })),
    ...[-1, 1, 127, 128.5, NaN, Infinity, -Infinity].map((memoryMib) => ({ resources: { memoryMib } })),
    ...[-2, -1.5, -0.5, NaN, Infinity, -Infinity].map((idleTimeoutMs) => ({ idleTimeoutMs })),
    { ttlMs: -1 },
    { source: { snapshotId: 'snapshot' }, resources: { vcpus: 1 } },
    { source: { templateId: 'custom-template' }, resources: { vcpus: 1 } },
    { source: { type: 'snapshot', id: 'snapshot' }, resources: { memoryMib: 128 } },
  ]
  for (const options of invalid) {
    const before = structuredClone(options)
    await assert.rejects(client.sandboxes.create(options))
    await assert.rejects(client.sandboxes.createAsync(options))
    await assert.rejects(client.sandboxes.createMany({ ...options, count: 2 }))
    if (options.idleTimeoutMs !== undefined || options.ttlMs !== undefined) {
      await assert.rejects(result.update({ name: 'must-not-change', ...options }))
      assert.equal(result.name, undefined)
      assert.equal(result.idleTimeoutMs, -1)
    }
    assert.deepEqual(options, before)
  }
  assert.equal(requests.length, 1)
  assert.equal(requests[0]?.method, 'GET')
})

test('snapshot creates accept empty and zero resource overrides', async (t) => {
  const { client, requests } = await serve(t)
  for (const resources of [{}, { vcpus: 0 }, { memoryMib: 0 }, { vcpus: 0, memoryMib: 0 }]) {
    await client.sandboxes.createAsync({ source: { snapshotId: 'snapshot' }, resources })
  }
  assert.equal(requests.length, 4)
})

test('the default template accepts positive resource overrides', async (t) => {
  const { client, requests } = await serve(t)
  await client.sandboxes.createAsync({ source: { templateId: 'default' }, resources: { vcpus: 4 } })
  assert.equal(requests[0]?.body, '{"source":{"type":"template","id":"default"},"resources":{"vcpu":4}}')
})
