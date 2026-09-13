import assert from 'node:assert/strict'
import http from 'node:http'
import { test } from 'node:test'

import { SandboxClient } from '../src/index.js'
import type { CreateProgress } from '../src/index.js'
import type { components } from '../src/generated/api-v1.js'

type ApiOperation = components['schemas']['Operation']
const started = '2026-09-13T10:00:00Z'
const completed = '2026-09-13T10:00:01Z'
const observed = '2026-09-13T10:00:02Z'

function operation(progress?: components['schemas']['CreateProgress']): ApiOperation {
  return {
    id: 'op-progress', type: 'sandbox_batch_create', status: 'running',
    requested: 1, succeeded: 0, failed: 0, created_at: started,
    results: [{ index: 0, ...(progress === undefined ? {} : { progress }) }],
  }
}

async function serve(t: { after: (fn: () => Promise<void>) => void }, responses: ApiOperation[]) {
  let polls = 0
  const server = http.createServer((req, res) => {
    const response = responses[Math.min(polls, responses.length - 1)]
    assert.ok(response)
    assert.equal(req.method, 'GET')
    assert.equal(req.headers.authorization, 'Bearer progress-test')
    res.setHeader('Content-Type', 'application/json')
    if (req.url === '/v1/operations') {
      res.end(JSON.stringify({ operations: responses }))
    } else {
      assert.equal(req.url, '/v1/operations/op-progress')
      polls++
      res.end(JSON.stringify(response))
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
  return {
    client: new SandboxClient({ baseUrl: `http://127.0.0.1:${address.port}`, apiKey: 'progress-test' }),
    polls: () => polls,
  }
}

test('get and refresh retain retry history, unknown stages, and nested dates', async (t) => {
  const initial = operation({ coordination: { phase: 'queued' } })
  initial.request_id = 'accepted-request'
  const retrying = operation({
    coordination: { phase: 'retrying', updated_at: observed },
    worker: {
      attempt: 2, sequence: 9, condition: 'active', observed_at: observed,
      current: { stage: 'future_worker_stage', attempt: 2, started_at: started },
      last_completed: { stage: 'allocated', attempt: 1, started_at: started, completed_at: completed },
    },
  })
  retrying.request_id = initial.request_id
  const { client } = await serve(t, [initial, retrying])
  const handle = await client.operations.get(initial.id)
  assert.equal(handle.state.requestId, 'accepted-request')
  assert.deepEqual(handle.state.results[0]?.progress, { coordination: { phase: 'queued' } })
  assert.strictEqual(await handle.refresh(), handle)
  const progress: CreateProgress | undefined = handle.state.results[0]?.progress
  assert.deepEqual(progress, {
    coordination: { phase: 'retrying', updatedAt: new Date(observed) },
    worker: {
      attempt: 2, sequence: 9, condition: 'active', observedAt: new Date(observed),
      current: { stage: 'future_worker_stage', attempt: 2, startedAt: new Date(started) },
      lastCompleted: { stage: 'allocated', attempt: 1, startedAt: new Date(started), completedAt: new Date(completed) },
    },
  })
  assert.equal(handle.done, false)
  assert.equal(handle.state.results[0]?.value, undefined)
  assert.equal(handle.state.results[0]?.error, undefined)
})

test('list accepts old servers without progress or request IDs', async (t) => {
  const legacy = operation()
  const noResults = { ...legacy, results: undefined }
  const withProgress = operation({ coordination: { phase: 'placing', updated_at: started } })
  const { client } = await serve(t, [legacy, noResults, withProgress])
  const states = []
  for await (const handle of client.operations.list()) states.push(handle.state)
  assert.equal(states.length, 3)
  assert.equal(states[0]?.requestId, undefined)
  assert.deepEqual(states[0]?.results, [{ index: 0 }])
  assert.deepEqual(states[1]?.results, [])
  assert.deepEqual(states[2]?.results[0]?.progress, {
    coordination: { phase: 'placing', updatedAt: new Date(started) },
  })
})

test('wait requires operation completion after worker success and preserves the result', async (t) => {
  const pending = operation({
    coordination: { phase: 'assigned', updated_at: started },
    worker: {
      attempt: 1, sequence: 4, condition: 'succeeded', observed_at: observed,
      current: { stage: 'ready', attempt: 1, started_at: started, completed_at: completed },
    },
  })
  const done: ApiOperation = {
    ...pending, status: 'succeeded', succeeded: 1, completed_at: observed,
    results: [{ index: 0, sandbox: {
      id: 'created-sandbox', status: 'running', source: { type: 'default' },
      resources: { vcpu: 1, memory_mib: 128 }, lifecycle: {}, metadata: {}, created_at: started,
    }, progress: { coordination: { phase: 'completed', updated_at: observed } } }],
  }
  const { client, polls } = await serve(t, [pending, pending, done])
  const handle = await client.operations.get(pending.id)
  assert.equal(handle.done, false)
  assert.deepEqual(handle.state.results[0]?.progress?.worker?.current.completedAt, new Date(completed))
  const result = await handle.wait({ pollIntervalMs: 1, timeoutMs: 1000 })
  assert.equal(polls(), 3)
  assert.equal(handle.done, true)
  assert.deepEqual(result.completedAt, new Date(observed))
  assert.equal(result.results[0]?.value?.id, 'created-sandbox')
  assert.equal(result.results[0]?.progress?.coordination.phase, 'completed')
})

test('terminal failures retain last completed stage and public error', async (t) => {
  const failed = operation({
    coordination: { phase: 'completed', updated_at: observed },
    worker: {
      attempt: 1, sequence: 3, condition: 'failed', observed_at: observed,
      current: { stage: 'guest_agent', attempt: 1, started_at: completed },
      last_completed: { stage: 'guest_network', attempt: 1, started_at: started, completed_at: completed },
    },
  })
  failed.status = 'failed'
  failed.failed = 1
  failed.completed_at = observed
  const error = { type: 'https://sandbox.dev/problems/create_failed', title: 'Create failed', status: 500, code: 'create_failed', request_id: 'failed-request' }
  failed.results = failed.results?.map((item) => ({ ...item, error }))
  const { client, polls } = await serve(t, [failed])
  const handle = await client.operations.get(failed.id)
  const result = await handle.wait()
  assert.equal(polls(), 1)
  assert.equal(handle.done, true)
  assert.deepEqual(result.results[0]?.error, error)
  assert.equal(result.results[0]?.value, undefined)
  assert.equal(result.results[0]?.progress?.worker?.condition, 'failed')
  assert.deepEqual(result.results[0]?.progress?.worker?.lastCompleted, {
    stage: 'guest_network', attempt: 1, startedAt: new Date(started), completedAt: new Date(completed),
  })
})
