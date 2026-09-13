import assert from 'node:assert/strict'
import http from 'node:http'
import { test } from 'node:test'
import type { TestContext } from 'node:test'

import { CreateAcceptanceError, NotFoundError, Sandbox, SandboxClient, SandboxError, TimeoutError } from '../src/index.js'
import type { components } from '../src/generated/api-v1.js'

const receipt = {
  id: 'single-operation', type: 'sandbox_create', status: 'pending',
  requested: 1, succeeded: 0, failed: 0, created_at: '2026-09-13T10:00:00Z',
  request_id: 'original-request',
} satisfies components['schemas']['Operation']

async function serve(t: TestContext, handler: http.RequestListener, maxRetries = 0) {
  const server = http.createServer(handler)
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
  const baseUrl = `http://127.0.0.1:${address.port}`
  return { baseUrl, client: new SandboxClient({ baseUrl, apiKey: 'single-test', maxRetries }) }
}

function accept(res: http.ServerResponse) {
  res.writeHead(202, { 'Content-Type': 'application/json' })
  res.end(JSON.stringify(receipt))
}

test('createAsync sends the source and key to the single endpoint and returns a refreshable handle', async (t) => {
  const requests: string[] = []
  const { client } = await serve(t, async (req, res) => {
    requests.push(`${req.method} ${req.url}`)
    assert.equal(req.headers.authorization, 'Bearer single-test')
    if (req.method === 'POST') {
      assert.equal(req.headers['idempotency-key'], 'caller-key')
      let body = ''
      for await (const chunk of req) body += chunk
      assert.deepEqual(JSON.parse(body), { source: { type: 'snapshot', id: 'snap-1' }, metadata: { task: 'example' } })
      accept(res)
    } else {
      res.end(JSON.stringify({ ...receipt, status: 'failed', failed: 1, completed_at: receipt.created_at,
        results: [{ index: 0, error: { type: 'about:blank', title: 'Failed', status: 500, code: 'create_failed', request_id: 'original-request' } }],
      }))
    }
  })
  const operation = await client.sandboxes.createAsync({ source: { snapshotId: 'snap-1' }, metadata: { task: 'example' }, idempotencyKey: 'caller-key' })
  assert.equal(operation.id, receipt.id)
  assert.equal(operation.state.type, 'sandbox_create')
  assert.equal(operation.state.requestId, 'original-request')
  assert.equal(operation.done, false)
  assert.equal(await operation.refresh(), operation)
  assert.equal((await operation.wait()).status, 'failed')
  assert.equal(operation.state.results[0]?.error?.code, 'create_failed')
  assert.deepEqual(requests, ['POST /v1/sandbox-creations', 'GET /v1/operations/single-operation'])
})

test('lost acceptance retries with the same generated key and body', async (t) => {
  const keys: unknown[] = []
  const bodies: string[] = []
  const { client } = await serve(t, async (req, res) => {
    assert.equal(req.url, '/v1/sandbox-creations')
    keys.push(req.headers['idempotency-key'])
    let body = ''
    for await (const chunk of req) body += chunk
    bodies.push(body)
    if (keys.length === 1) req.socket.destroy()
    else accept(res)
  }, 1)
  const operation = await client.sandboxes.createAsync({ name: 'recover-me' })
  assert.equal(operation.id, receipt.id)
  assert.equal(keys.length, 2)
  assert.equal(typeof keys[0], 'string')
  assert.ok(keys[0])
  assert.equal(keys[0], keys[1])
  assert.equal(bodies[0], bodies[1])
})

test('uncertain acceptance exposes a generated key for explicit same-options replay', async (t) => {
  const keys: unknown[] = []
  const { client } = await serve(t, (req, res) => {
    keys.push(req.headers['idempotency-key'])
    if (keys.length === 1) req.socket.destroy()
    else accept(res)
  })
  let recoveredKey = ''
  await assert.rejects(client.sandboxes.createAsync(), (error: unknown) => {
    assert.ok(error instanceof CreateAcceptanceError)
    assert.ok(error.cause instanceof Error)
    recoveredKey = error.idempotencyKey
    assert.equal(recoveredKey, keys[0])
    return true
  })
  assert.equal((await client.sandboxes.createAsync({ idempotencyKey: recoveredKey })).id, receipt.id)
  assert.deepEqual(keys, [recoveredKey, recoveredKey])
})

for (const status of [400, 404, 405, 409]) {
  test(`explicit ${status} rejection preserves the API error and never falls back`, async (t) => {
    const requests: string[] = []
    const { client } = await serve(t, (req, res) => {
      requests.push(`${req.method} ${req.url}`)
      res.writeHead(status, { 'Content-Type': 'application/problem+json' })
      res.end(JSON.stringify({ type: 'about:blank', title: 'Rejected', status, code: 'rejected', request_id: 'rejection' }))
    })
    await assert.rejects(client.sandboxes.createAsync(), (error: unknown) => {
      assert.ok(error instanceof SandboxError)
      assert.ok(!(error instanceof CreateAcceptanceError))
      if (status === 404) assert.ok(error instanceof NotFoundError)
      assert.equal(error.status, status)
      return true
    })
    assert.deepEqual(requests, ['POST /v1/sandbox-creations'])
  })
}

const invalidReceipts: Array<{ name: string; status: number; body: unknown }> = [
  { name: 'HTTP 200 operation', status: 200, body: receipt },
  { name: 'HTTP 201 sandbox', status: 201, body: { id: 'sandbox' } },
  { name: 'HTTP 204', status: 204, body: undefined },
  { name: 'missing fields', status: 202, body: { id: receipt.id } },
  { name: 'batch operation', status: 202, body: { ...receipt, type: 'sandbox_batch_create' } },
  { name: 'two members', status: 202, body: { ...receipt, requested: 2 } },
  { name: 'invalid date', status: 202, body: { ...receipt, created_at: 'bad' } },
  { name: 'invalid request ID', status: 202, body: { ...receipt, request_id: 12 } },
  { name: 'terminal receipt', status: 202, body: { ...receipt, completed_at: receipt.created_at } },
  { name: 'unexpected results', status: 202, body: { ...receipt, results: [{ index: 0 }] } },
  { name: 'null receipt', status: 202, body: null },
]
for (const invalid of invalidReceipts) {
  test(`rejects ${invalid.name} with a recovery key`, async (t) => {
    let posts = 0
    const { client } = await serve(t, (req, res) => {
      posts++
      assert.equal(req.url, '/v1/sandbox-creations')
      res.writeHead(invalid.status)
      res.end(JSON.stringify(invalid.body))
    })
    await assert.rejects(client.sandboxes.createAsync({ idempotencyKey: 'recover-invalid' }), (error: unknown) => {
      assert.ok(error instanceof CreateAcceptanceError)
      assert.equal(error.idempotencyKey, 'recover-invalid')
      return true
    })
    assert.equal(posts, 1)
  })
}

test('invalid acceptance JSON retains the parse failure and recovery key', async (t) => {
  const { client } = await serve(t, (_req, res) => {
    res.writeHead(202)
    res.end('{')
  })
  await assert.rejects(client.sandboxes.createAsync({ idempotencyKey: 'invalid-json' }), (error: unknown) => {
    assert.ok(error instanceof CreateAcceptanceError)
    assert.ok(error.cause instanceof SyntaxError)
    assert.equal(error.idempotencyKey, 'invalid-json')
    return true
  })
})

test('Sandbox.createAsync facade returns the durable single handle', async (t) => {
  const { baseUrl } = await serve(t, (req, res) => {
    assert.equal(req.url, '/v1/sandbox-creations')
    assert.equal(req.headers['idempotency-key'], 'facade-key')
    accept(res)
  })
  const handle = await Sandbox.createAsync({ apiUrl: baseUrl, apiKey: 'single-test', idempotencyKey: 'facade-key' })
  assert.equal(handle.id, receipt.id)
  assert.equal(handle.state.type, 'sandbox_create')
})

for (const abort of [false, true]) {
  test(`create acceptance ${abort ? 'abort' : 'timeout'} interrupts a hanging response body`, async (t) => {
    const controller = new AbortController()
    const reason = new Error('caller stopped')
    const { client } = await serve(t, (_req, res) => {
      res.writeHead(202)
      res.write('{"id":')
      if (abort) setTimeout(() => controller.abort(reason), 10)
    })
    const started = Date.now()
    await assert.rejects(client.sandboxes.createAsync({ requestTimeoutMs: 70, signal: controller.signal, idempotencyKey: 'body-key' }), (error: unknown) => {
      assert.ok(error instanceof CreateAcceptanceError)
      assert.equal(error.idempotencyKey, 'body-key')
      if (abort) assert.equal(error.cause, reason)
      else assert.ok(error.cause instanceof TimeoutError)
      return true
    })
    assert.ok(Date.now() - started < 1000)
  })
}

for (const phase of ['headers', 'body', 'retry']) {
  test(`Operation.wait bounds hanging GET ${phase} by the remaining total deadline`, async (t) => {
    let gets = 0
    const { client } = await serve(t, (req, res) => {
      if (req.method === 'POST') return accept(res)
      gets++
      if (phase === 'body') { res.writeHead(200); res.write('{') }
      if (phase === 'retry') { res.writeHead(503, { 'Retry-After': '5' }); res.end('{}') }
    }, 2)
    const operation = await client.sandboxes.createAsync()
    const started = Date.now()
    await assert.rejects(operation.wait({ pollIntervalMs: 30, timeoutMs: 80 }), TimeoutError)
    assert.equal(gets, 1)
    assert.ok(Date.now() - started < 1000)
  })
}

test('Operation.wait aborts a hanging GET body and leaves remote work pending', async (t) => {
  const controller = new AbortController()
  const reason = new Error('stop waiting')
  const { client } = await serve(t, (req, res) => {
    if (req.method === 'POST') return accept(res)
    assert.equal(req.method, 'GET')
    res.writeHead(200)
    res.write('{')
    setTimeout(() => controller.abort(reason), 10)
  })
  const operation = await client.sandboxes.createAsync()
  await assert.rejects(operation.wait({ pollIntervalMs: 1, timeoutMs: 1000, signal: controller.signal }), (error: unknown) => error === reason)
  assert.equal(operation.done, false)
})

for (const status of [307, 308]) {
  test(`single-create rejects HTTP ${status} without following its mutation redirect`, async (t) => {
    const requests: string[] = []
    const { client } = await serve(t, (req, res) => {
      requests.push(`${req.method} ${req.url}`)
      if (req.url === '/v1/sandbox-creations') {
        assert.equal(req.headers['idempotency-key'], 'redirect-key')
        res.writeHead(status, { Location: '/v1/sandboxes' })
        res.end()
      } else {
        accept(res)
      }
    }, 2)
    await assert.rejects(client.sandboxes.createAsync({ idempotencyKey: 'redirect-key' }), (error: unknown) => {
      assert.ok(error instanceof CreateAcceptanceError)
      assert.equal(error.idempotencyKey, 'redirect-key')
      assert.ok(error.cause instanceof SandboxError)
      assert.equal(error.cause.status, status)
      return true
    })
    assert.deepEqual(requests, ['POST /v1/sandbox-creations'])
  })
}
