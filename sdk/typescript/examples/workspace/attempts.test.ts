import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { mkdtempSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs'
import http from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { setTimeout as sleep } from 'node:timers/promises'
import { test, type TestContext } from 'node:test'
import { SandboxClient } from '../../src/index.js'
import { driveWorkspaceAttempts } from './attempts.js'
import { isJournalError, readAttemptJournal, withAttemptJournal, type StartAttempts } from './attempts-journal.js'

function json(response: http.ServerResponse, status: number, value: unknown) {
  response.writeHead(status, { 'content-type': 'application/json' })
  response.end(JSON.stringify(value))
}
function missing(response: http.ServerResponse) {
  json(response, 404, { type: 'about:blank', title: 'Missing', status: 404, code: 'not_found' })
}
async function body(request: http.IncomingMessage): Promise<Record<string, unknown>> {
  let text = ''
  for await (const chunk of request) text += chunk
  const value: unknown = JSON.parse(text)
  assert.ok(value && typeof value === 'object' && !Array.isArray(value))
  return Object.fromEntries(Object.entries(value))
}
function object(value: unknown): Record<string, unknown> {
  assert.ok(value && typeof value === 'object' && !Array.isArray(value))
  return Object.fromEntries(Object.entries(value))
}

async function fixture(t: TestContext) {
  const directory = mkdtempSync(join(tmpdir(), 'workspace-driver-'))
  t.after(() => rmSync(directory, { recursive: true, force: true }))
  const control: {
    accepted: boolean; lostAcceptance: boolean; loseCommand: boolean; loseDelete: boolean
    reportUsage: boolean; terminal: boolean; failedIndex: number; expiredIndex: number; commands: number; deletes: number
    posts: Array<{ key: string; body: string }>; metadata: Record<string, unknown>
    beforeCommand?: () => Promise<void>; afterCommand?: () => Promise<void>
  } = {
    accepted: false, lostAcceptance: false, loseCommand: false, loseDelete: false,
    reportUsage: false, terminal: true, failedIndex: -1, expiredIndex: -1, commands: 0, deletes: 0,
    posts: [], metadata: {},
  }
  const deleted = new Set<string>()
  const sandbox = (index: number) => ({ id: `child-${index}`, status: 'running',
    source: { type: 'snapshot', id: 'saved' }, metadata: control.metadata,
    resources: { vcpu: 2, memory_mib: 128 }, lifecycle: {}, created_at: '2026-09-13T12:00:00Z' })
  const operation = () => ({ id: 'batch', type: 'sandbox_batch_create', status: control.terminal ? 'succeeded' : 'running',
    requested: 2, succeeded: control.terminal ? 2 : 1, failed: control.failedIndex >= 0 ? 1 : 0,
    created_at: '2026-09-13T12:00:00Z', ...(control.terminal ? { completed_at: '2026-09-13T12:00:01Z' } : {}),
    results: (control.terminal ? [0, 1] : [0]).map((index) => index === control.failedIndex
      ? { index, error: { type: 'about:blank', title: 'Capacity failed', status: 503, code: 'capacity', request_id: 'worker-original' } }
      : { index, sandbox: sandbox(index) }),
  })
  const errors: unknown[] = []
  const server = http.createServer((request, response) => {
    void (async () => {
      if (request.url === '/v1/snapshots/saved') {
        json(response, 200, { id: 'saved', name: 'prepared', state: 'durable', created_at: '2026-09-13T12:00:00Z' }); return
      }
      if (request.url === '/v1/sandbox-batches') {
        let raw = ''
        for await (const chunk of request) raw += chunk
        const parsed: unknown = JSON.parse(raw)
        control.metadata = object(object(parsed).sandbox).metadata ? object(object(object(parsed).sandbox).metadata) : {}
        control.posts.push({ key: String(request.headers['idempotency-key']), body: raw })
        control.accepted = true
        if (control.lostAcceptance) { control.lostAcceptance = false; request.socket.destroy(); return }
        json(response, 202, operation()); return
      }
      if (request.url === '/v1/operations/batch') { json(response, 200, operation()); return }
      const match = request.url?.match(/^\/v1\/sandboxes\/child-(\d+)$/)
      if (match) {
        const index = Number(match[1])
        if (deleted.has(`child-${index}`) || index === control.expiredIndex) { missing(response); return }
        if (request.method === 'DELETE') {
          control.deletes++; deleted.add(`child-${index}`)
          if (control.loseDelete) { control.loseDelete = false; request.socket.destroy(); return }
          response.writeHead(204); response.end(); return
        }
        json(response, 200, sandbox(index)); return
      }
      if (request.url?.endsWith('/usage')) {
        if (control.reportUsage) {
          json(response, 200, { intervals: [], totals: { intervals: 1, open_intervals: 1,
            duration_seconds: 2, vcpu_seconds: 4, memory_mib_seconds: 256, cpu_seconds: 0.5 },
          window: { selection: 'overlap' }, coverage: { hosts_reporting: 1, scope: 'live_hosts', truncated: false } }); return
        }
        json(response, 503, { type: 'about:blank', title: 'Usage unavailable', status: 503 }); return
      }
      if (request.url?.match(/^\/sandboxes\/child-\d+\/exec$/)) {
        const input = await body(request)
        assert.equal(typeof input.cmd, 'string')
        if (typeof input.cmd !== 'string') throw new Error('Missing command')
        control.commands++
        await control.beforeCommand?.()
        const child = spawn('/bin/sh', ['-c', input.cmd], {
          env: { ...process.env, WORKSPACE_RECEIPT_ROOT: join(directory, 'guest') }, stdio: ['ignore', 'pipe', 'pipe'],
        })
        let stdout = ''; let stderr = ''
        child.stdout.on('data', (data: Buffer) => { stdout += data.toString() })
        child.stderr.on('data', (data: Buffer) => { stderr += data.toString() })
        const [exitCode] = await once(child, 'close')
        await control.afterCommand?.()
        if (control.loseCommand) { control.loseCommand = false; request.socket.destroy(); return }
        json(response, 200, { stdout, stderr, exit_code: exitCode, duration_ms: 1, timed_out: false }); return
      }
      throw new Error(`Unexpected ${request.method} ${request.url}`)
    })().catch((error: unknown) => { errors.push(error); json(response, 500, { message: String(error) }) })
  })
  server.listen(0, '127.0.0.1'); await once(server, 'listening')
  t.after(async () => { server.closeAllConnections(); await new Promise<void>((resolve) => server.close(() => resolve())); assert.deepEqual(errors, []) })
  const address = server.address(); assert.ok(address && typeof address !== 'string')
  const target = `http://127.0.0.1:${address.port}`
  const client = new SandboxClient({ baseUrl: target, apiKey: 'test', maxRetries: 0, requestTimeoutMs: 1000 })
  const runDirectory = join(directory, 'run')
  const start: StartAttempts = { target, snapshotId: 'saved', count: 2, maxParallelism: 2,
    script: 'printf result', timeoutMs: 5000, ttlMs: 60_000 }
  return { control, client, target, directory, runDirectory, start, deleted }
}

test('lost acceptance replays exact intent and expired/failed members retain indexed outcomes', async (t) => {
  const f = await fixture(t)
  f.control.lostAcceptance = true; f.control.failedIndex = 1; f.control.expiredIndex = 0
  await withAttemptJournal({ directory: f.runDirectory, start: f.start }, async (journal) => {
    await driveWorkspaceAttempts(journal, f.client)
    assert.equal(journal.state.operation.kind, 'unaccepted')
  })
  await withAttemptJournal({ directory: f.runDirectory, target: f.target }, async (journal) => {
    const state = await driveWorkspaceAttempts(journal, f.client)
    assert.equal(state.phase, 'complete')
    const first = state.attempts[0]; const second = state.attempts[1]
    assert.ok(first?.kind === 'ready' && first.command.kind === 'unavailable')
    assert.ok(second?.kind === 'create-failed'); assert.equal(second.error.requestId, 'worker-original')
  })
  assert.equal(f.control.posts.length, 2); assert.deepEqual(f.control.posts[0], f.control.posts[1])
  assert.equal(f.control.commands, 0)
})

test('lost command response recovers real guest receipt without executing twice, then resolves lost delete', async (t) => {
  const f = await fixture(t)
  const counter = join(f.directory, 'counter')
  f.start.script = `printf x >> '${counter}'; printf result`
  f.control.loseCommand = true
  await withAttemptJournal({ directory: f.runDirectory, start: f.start }, async (journal) => {
    const state = await driveWorkspaceAttempts(journal, f.client)
    assert.equal(state.phase, 'running')
    assert.ok(state.attempts.some((attempt) => attempt.kind === 'ready' && attempt.command.kind === 'uncertain'))
  })
  assert.equal(readFileSync(counter, 'utf8'), 'xx')
  f.control.loseDelete = true
  await withAttemptJournal({ directory: f.runDirectory, target: f.target }, async (journal) => {
    const state = await driveWorkspaceAttempts(journal, f.client)
    assert.equal(state.phase, 'complete')
    assert.ok(state.attempts.every((attempt) => attempt.kind === 'ready' && attempt.command.kind === 'completed' && attempt.cleanup === 'deleted'))
  })
  assert.equal(readFileSync(counter, 'utf8'), 'xx')
  assert.equal(readFileSync(join(f.runDirectory, 'attempt-0.stdout'), 'utf8'), 'result')
  assert.equal(f.control.deletes, 2)
})

test('explicit cleanup settles late creation before completing and never runs its command', async (t) => {
  const f = await fixture(t); f.control.terminal = false
  await withAttemptJournal({ directory: f.runDirectory, start: f.start }, async (journal) => {
    const state = await driveWorkspaceAttempts(journal, f.client, { cleanup: true, observationTimeoutMs: 80, pollIntervalMs: 10 })
    assert.equal(state.phase, 'cleaning'); assert.equal(state.attempts[1]?.kind, 'pending')
    assert.equal(f.control.deletes, 0)
  })
  f.control.terminal = true
  await withAttemptJournal({ directory: f.runDirectory, target: f.target }, async (journal) => {
    assert.equal((await driveWorkspaceAttempts(journal, f.client)).phase, 'complete')
  })
  assert.equal(f.control.commands, 0); assert.equal(f.control.deletes, 2)
})

test('a saved completed result survives later sandbox expiry while the remaining member completes', async (t) => {
  const f = await fixture(t); f.control.terminal = false; f.control.reportUsage = true
  await withAttemptJournal({ directory: f.runDirectory, start: f.start }, async (journal) => {
    const state = await driveWorkspaceAttempts(journal, f.client, { observationTimeoutMs: 500, pollIntervalMs: 20 })
    assert.equal(state.phase, 'running')
    const first = state.attempts[0]
    assert.ok(first?.kind === 'ready' && first.command.kind === 'completed' && first.usage.kind === 'reported')
    assert.equal(first.usage.totals.vcpuSeconds, 4)
  })
  f.control.terminal = true; f.control.expiredIndex = 0
  await withAttemptJournal({ directory: f.runDirectory, target: f.target }, async (journal) => {
    const state = await driveWorkspaceAttempts(journal, f.client)
    assert.equal(state.phase, 'complete')
    assert.ok(state.attempts.every((attempt) => attempt.kind === 'ready' && attempt.command.kind === 'completed'))
  })
  assert.equal(f.control.commands, 2)
  assert.equal(readFileSync(join(f.runDirectory, 'attempt-0.stdout'), 'utf8'), 'result')
})

test('cleanup refuses changed ownership without discarding completed command output', async (t) => {
  const f = await fixture(t)
  let completed = 0
  f.control.afterCommand = async () => { if (++completed === 2) f.control.metadata.purpose = 'another-workflow' }
  await withAttemptJournal({ directory: f.runDirectory, start: f.start }, async (journal) => {
    const state = await driveWorkspaceAttempts(journal, f.client)
    assert.equal(state.phase, 'cleaning')
    assert.ok(state.attempts.every((attempt) => attempt.kind === 'ready' && attempt.command.kind === 'completed'))
    assert.ok(state.observations.some((entry) => entry.message.includes('does not belong')))
  })
  assert.equal(f.control.deletes, 0)
  assert.equal(readFileSync(join(f.runDirectory, 'attempt-0.stdout'), 'utf8'), 'result')
  f.control.metadata.purpose = 'workspace-attempts'
  await withAttemptJournal({ directory: f.runDirectory, target: f.target }, async (journal) => {
    assert.equal((await driveWorkspaceAttempts(journal, f.client)).phase, 'complete')
  })
  assert.equal(f.control.commands, 2)
})

test('failed result checkpoint drains sibling requests and propagates JournalError', async (t) => {
  const f = await fixture(t)
  let release: () => void = () => {}
  const gate = new Promise<void>((resolve) => { release = resolve })
  let responses = 0
  f.control.afterCommand = async () => { responses++; if (responses === 1) { renameSync(f.runDirectory, `${f.runDirectory}-moved`); release() } else await gate }
  await assert.rejects(withAttemptJournal({ directory: f.runDirectory, start: f.start }, async (journal) => {
    await driveWorkspaceAttempts(journal, f.client)
  }), isJournalError)
  assert.equal(responses, 2); assert.equal(f.control.deletes, 0)
})

async function waitUntil(check: () => boolean, timeoutMs = 10_000) {
  const deadline = Date.now() + timeoutMs
  while (!check()) { if (Date.now() >= deadline) throw new Error('Fixture wait timed out'); await sleep(10) }
}
function cli(f: { target: string }, args: string[]) {
  const child = spawn(process.execPath, ['--import', 'tsx', 'examples/workspace-attempts.ts', ...args], {
    env: { ...process.env, SANDBOX_API_URL: f.target, SANDBOX_API_KEY: 'test' }, stdio: ['ignore', 'pipe', 'pipe'],
  })
  let stdout = ''; let stderr = ''
  child.stdout.on('data', (data: Buffer) => { stdout += data.toString() })
  child.stderr.on('data', (data: Buffer) => { stderr += data.toString() })
  const exited = once(child, 'close').then(([code]) => ({ code, stdout, stderr }))
  return { child, exited }
}

test('CLI SIGKILL after accepted operation preserves ownership and resumes real guest results', async (t) => {
  const f = await fixture(t)
  let release: () => void = () => {}
  const blocked = new Promise<void>((resolve) => { release = resolve })
  f.control.beforeCommand = () => blocked
  let finished = 0
  f.control.afterCommand = async () => { finished++ }
  const script = join(f.directory, 'attempt.sh'); writeFileSync(script, 'printf cli-result')
  const first = cli(f, ['start', '--snapshot', 'saved', '--count', '2', '--run', f.runDirectory, '--command-file', script])
  t.after(() => { first.child.kill('SIGKILL'); release() })
  await waitUntil(() => f.control.commands === 2)
  const second = cli(f, ['resume', '--run', f.runDirectory])
  const rejected = await second.exited
  assert.notEqual(rejected.code, 0); assert.match(rejected.stderr, /owned|busy|another/i)
  first.child.kill('SIGKILL'); await first.exited
  release(); f.control.beforeCommand = undefined
  await waitUntil(() => finished === 2)
  const resumed = await cli(f, ['resume', '--run', f.runDirectory]).exited
  assert.equal(resumed.code, 0, resumed.stderr)
  const saved = readAttemptJournal(f.runDirectory)
  assert.equal(saved.phase, 'complete'); assert.equal(f.control.posts.length, 1)
  assert.equal(readFileSync(join(f.runDirectory, 'attempt-1.stdout'), 'utf8'), 'cli-result')
})
