import assert from 'node:assert/strict'
import http from 'node:http'
import { after, before, test } from 'node:test'
import type { AddressInfo } from 'node:net'

import {
  AuthenticationError,
  CommandExitError,
  NotFoundError,
  Sandbox,
  SandboxError,
  TimeoutError,
} from '../src/index.js'

const API_KEY = 'test-key'
const SANDBOX_ID = '0f5e3a1c-1111-2222-3333-444455556666'

const sandboxRecord = {
  id: SANDBOX_ID,
  pid: 4242,
  vm_id: 'aaaa-bbbb',
  socket_path: '/run/fc-test.sock',
  tap_device: 'fc0',
  guest_ip: '172.16.0.10',
  host_port: 5200,
  rootfs_path: '/opt/fc/instances/test.ext4',
  status: 'running',
  created_at: '2026-06-10T12:00:00Z',
}

// In-memory fake API state
const guestFiles = new Map<string, Buffer>()
const exposedPorts = new Map<number, number>() // guest_port -> host_port
let sandboxAlive = false
let sandboxExpiresAt: string | undefined
let lastExecBody: Record<string, unknown> | undefined
let lastCreateBody: Record<string, unknown> | undefined
let lastTimeoutBody: Record<string, unknown> | undefined

let server: http.Server
let apiUrl: string

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms))

function currentSandboxRecord(): Record<string, unknown> {
  return sandboxExpiresAt !== undefined
    ? { ...sandboxRecord, expires_at: sandboxExpiresAt }
    : { ...sandboxRecord }
}

function readBody(req: http.IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = []
    req.on('data', (c: Buffer) => chunks.push(c))
    req.on('end', () => resolve(Buffer.concat(chunks)))
    req.on('error', reject)
  })
}

function sendJson(res: http.ServerResponse, status: number, body: unknown): void {
  res.writeHead(status, { 'Content-Type': 'application/json' })
  res.end(JSON.stringify(body))
}

async function handle(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
  const url = new URL(req.url ?? '/', 'http://localhost')
  const path = url.pathname

  if (req.headers.authorization !== `Bearer ${API_KEY}`) {
    sendJson(res, 401, { error: 'invalid or missing API key' })
    return
  }

  if (req.method === 'POST' && path === '/sandboxes') {
    const raw = (await readBody(req)).toString()
    lastCreateBody = raw ? (JSON.parse(raw) as Record<string, unknown>) : undefined
    sandboxAlive = true
    exposedPorts.clear()
    sandboxExpiresAt =
      typeof lastCreateBody?.timeout_sec === 'number' && lastCreateBody.timeout_sec > 0
        ? '2026-06-10T12:05:00Z'
        : undefined
    sendJson(res, 201, currentSandboxRecord())
    return
  }

  if (req.method === 'GET' && path === '/sandboxes') {
    sendJson(res, 200, sandboxAlive ? [currentSandboxRecord()] : [])
    return
  }

  if (req.method === 'GET' && path === `/sandboxes/${SANDBOX_ID}`) {
    if (!sandboxAlive) {
      sendJson(res, 404, { error: 'sandbox not found' })
      return
    }
    sendJson(res, 200, currentSandboxRecord())
    return
  }

  if (req.method === 'POST' && path === `/sandboxes/${SANDBOX_ID}/timeout`) {
    lastTimeoutBody = JSON.parse((await readBody(req)).toString()) as Record<string, unknown>
    const sec = Number(lastTimeoutBody.timeout_sec ?? 0)
    sandboxExpiresAt = sec > 0 ? '2026-06-10T12:30:00Z' : undefined
    sendJson(res, 200, currentSandboxRecord())
    return
  }

  if (req.method === 'POST' && path === `/sandboxes/${SANDBOX_ID}/ports`) {
    const body = JSON.parse((await readBody(req)).toString()) as { guest_port: number }
    const guestPort = body.guest_port
    if (guestPort === 3000) {
      sendJson(res, 200, { guest_port: 3000, host_port: sandboxRecord.host_port })
      return
    }
    let hostPort = exposedPorts.get(guestPort)
    if (hostPort === undefined) {
      hostPort = 5201 + exposedPorts.size
      exposedPorts.set(guestPort, hostPort)
    }
    sendJson(res, 200, { guest_port: guestPort, host_port: hostPort })
    return
  }

  if (req.method === 'GET' && path === `/sandboxes/${SANDBOX_ID}/ports`) {
    const mappings = [{ guest_port: 3000, host_port: sandboxRecord.host_port }]
    for (const [guestPort, hostPort] of exposedPorts) {
      mappings.push({ guest_port: guestPort, host_port: hostPort })
    }
    sendJson(res, 200, mappings)
    return
  }

  if (req.method === 'POST' && path === `/sandboxes/${SANDBOX_ID}/exec/stream`) {
    const body = JSON.parse((await readBody(req)).toString()) as Record<string, unknown>
    lastExecBody = body
    const cmd = String(body.cmd ?? '')
    res.writeHead(200, { 'Content-Type': 'application/x-ndjson' })
    if (cmd.includes('sleep-forever')) {
      res.write('{"type":"stdout","data":"started\\n"}\n')
      await sleep(5)
      res.end('{"type":"exit","exit_code":-1,"timed_out":true,"duration_ms":5000}\n')
      return
    }
    if (cmd.includes('exit 3')) {
      res.write('{"type":"stdout","data":"partial output"}\n')
      await sleep(5)
      res.write('{"type":"stderr","data":"boom: it broke"}\n')
      await sleep(5)
      res.end('{"type":"exit","exit_code":3,"timed_out":false,"duration_ms":12}\n')
      return
    }
    // Split one event across two chunks to exercise partial-line buffering.
    res.write('{"type":"stdout","data":"hel')
    await sleep(5)
    res.write('lo "}\n{"type":"stdout","data":"world\\n"}\n')
    await sleep(5)
    res.write('{"type":"stderr","data":"warn\\n"}\n')
    await sleep(5)
    res.end('{"type":"exit","exit_code":0,"timed_out":false,"duration_ms":42}\n')
    return
  }

  if (req.method === 'DELETE' && path === `/sandboxes/${SANDBOX_ID}`) {
    if (!sandboxAlive) {
      sendJson(res, 404, { error: 'sandbox not found' })
      return
    }
    sandboxAlive = false
    res.writeHead(204)
    res.end()
    return
  }

  if (req.method === 'POST' && path === `/sandboxes/${SANDBOX_ID}/exec`) {
    const body = JSON.parse((await readBody(req)).toString()) as Record<string, unknown>
    lastExecBody = body
    const cmd = String(body.cmd ?? '')
    if (cmd.includes('sleep-forever')) {
      sendJson(res, 200, {
        stdout: '',
        stderr: '',
        exit_code: -1,
        timed_out: true,
        duration_ms: 5000,
      })
      return
    }
    if (cmd.includes('exit 3')) {
      sendJson(res, 200, {
        stdout: 'partial output',
        stderr: 'boom: it broke',
        exit_code: 3,
        timed_out: false,
        duration_ms: 12,
      })
      return
    }
    sendJson(res, 200, {
      stdout: 'v22.0.0\n',
      stderr: '',
      exit_code: 0,
      timed_out: false,
      duration_ms: 34,
    })
    return
  }

  if (path === `/sandboxes/${SANDBOX_ID}/files`) {
    const filePath = url.searchParams.get('path') ?? ''
    if (req.method === 'PUT') {
      const data = await readBody(req)
      guestFiles.set(filePath, data)
      sendJson(res, 201, { path: filePath, bytes: data.length })
      return
    }
    if (req.method === 'GET') {
      const data = guestFiles.get(filePath)
      if (data === undefined) {
        sendJson(res, 404, { error: `no such file: ${filePath}` })
        return
      }
      res.writeHead(200, { 'Content-Type': 'application/octet-stream' })
      res.end(data)
      return
    }
  }

  if (req.method === 'GET' && path === `/sandboxes/${SANDBOX_ID}/dir`) {
    sendJson(res, 200, [
      {
        name: 'App.tsx',
        size: 120,
        mode: '-rw-r--r--',
        is_dir: false,
        mtime: '2026-06-10T12:05:00Z',
      },
      {
        name: 'assets',
        size: 4096,
        mode: 'drwxr-xr-x',
        is_dir: true,
        mtime: '2026-06-10T12:00:00Z',
      },
    ])
    return
  }

  sendJson(res, 404, { error: `unknown route: ${req.method} ${path}` })
}

before(async () => {
  server = http.createServer((req, res) => {
    handle(req, res).catch(() => {
      sendJson(res, 500, { error: 'mock server error' })
    })
  })
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
  const { port } = server.address() as AddressInfo
  apiUrl = `http://127.0.0.1:${port}`
})

after(async () => {
  await new Promise<void>((resolve, reject) =>
    server.close((err) => (err ? reject(err) : resolve()))
  )
})

const opts = () => ({ apiUrl, apiKey: API_KEY })

test('full lifecycle: create → exec → write/read/list → kill', async () => {
  const sbx = await Sandbox.create(opts())
  assert.equal(sbx.sandboxId, SANDBOX_ID)
  assert.equal(sbx.info.hostPort, 5200)
  assert.equal(sbx.info.guestIp, '172.16.0.10')

  // exec — success
  const result = await sbx.commands.run('node --version', {
    cwd: '/home/sandbox/app',
    envs: { FOO: 'bar' },
    timeoutMs: 2500,
  })
  assert.equal(result.stdout, 'v22.0.0\n')
  assert.equal(result.exitCode, 0)
  assert.equal(result.durationMs, 34)
  // timeoutMs 2500 → timeout_sec ceil(2.5) = 3; cwd/env passed through
  assert.equal(lastExecBody?.timeout_sec, 3)
  assert.equal(lastExecBody?.cwd, '/home/sandbox/app')
  assert.deepEqual(lastExecBody?.env, { FOO: 'bar' })

  // exec — non-zero exit throws CommandExitError carrying the result
  await assert.rejects(
    () => sbx.commands.run('exit 3'),
    (err: unknown) => {
      assert.ok(err instanceof CommandExitError)
      assert.ok(err instanceof SandboxError)
      assert.equal(err.exitCode, 3)
      assert.equal(err.stdout, 'partial output')
      assert.equal(err.stderr, 'boom: it broke')
      assert.equal(err.result.durationMs, 12)
      return true
    }
  )

  // exec — guest-side timeout throws TimeoutError
  await assert.rejects(
    () => sbx.commands.run('sleep-forever'),
    (err: unknown) => err instanceof TimeoutError
  )

  // files — write string, read back as text and bytes
  await sbx.files.write('/home/sandbox/app/src/x.ts', 'export const x = 1\n')
  const text = await sbx.files.read('/home/sandbox/app/src/x.ts')
  assert.equal(text, 'export const x = 1\n')
  const bytes = await sbx.files.read('/home/sandbox/app/src/x.ts', { format: 'bytes' })
  assert.ok(bytes instanceof Uint8Array)
  assert.equal(new TextDecoder().decode(bytes), 'export const x = 1\n')

  // files — write binary round-trip
  const blob = new Uint8Array([0, 1, 2, 255, 254])
  const writeInfo = await sbx.files.write('/tmp/blob.bin', blob)
  assert.equal(writeInfo.bytes, 5)
  const back = await sbx.files.read('/tmp/blob.bin', { format: 'bytes' })
  assert.deepEqual(Array.from(back), [0, 1, 2, 255, 254])

  // files — missing file → NotFoundError
  await assert.rejects(
    () => sbx.files.read('/no/such/file'),
    (err: unknown) => err instanceof NotFoundError
  )

  // files — list maps is_dir/mtime to type/modifiedAt
  const entries = await sbx.files.list('/home/sandbox/app/src')
  assert.equal(entries.length, 2)
  assert.deepEqual(
    entries.map((e) => [e.name, e.type]),
    [
      ['App.tsx', 'file'],
      ['assets', 'dir'],
    ]
  )
  assert.ok(entries[0]!.modifiedAt instanceof Date)
  assert.equal(entries[0]!.size, 120)

  // getHost
  assert.equal(sbx.getHost(3000), '127.0.0.1:5200')
  assert.equal(sbx.getHost(), '127.0.0.1:5200')
  assert.throws(
    () => sbx.getHost(9999),
    (err: unknown) => err instanceof SandboxError && /3000/.test((err as Error).message)
  )

  // static list + connect while running
  const infos = await Sandbox.list(opts())
  assert.equal(infos.length, 1)
  assert.equal(infos[0]!.sandboxId, SANDBOX_ID)
  assert.ok(infos[0]!.createdAt instanceof Date)

  const reconnected = await Sandbox.connect(SANDBOX_ID, opts())
  assert.equal(reconnected.sandboxId, SANDBOX_ID)

  // kill
  await sbx.kill()
  const remaining = await Sandbox.list(opts())
  assert.equal(remaining.length, 0)
})

test('streaming exec: callbacks get chunks, result accumulates full output', async () => {
  const sbx = await Sandbox.create(opts())

  const outChunks: string[] = []
  const errChunks: string[] = []
  const result = await sbx.commands.run('stream-hello', {
    onStdout: (d) => outChunks.push(d),
    onStderr: (d) => errChunks.push(d),
  })
  // One event was split across two transport chunks; the parser must
  // reassemble it into a single 'hello ' callback.
  assert.deepEqual(outChunks, ['hello ', 'world\n'])
  assert.deepEqual(errChunks, ['warn\n'])
  assert.equal(result.stdout, 'hello world\n')
  assert.equal(result.stderr, 'warn\n')
  assert.equal(result.exitCode, 0)
  assert.equal(result.durationMs, 42)

  // streaming non-zero exit keeps CommandExitError semantics
  await assert.rejects(
    () => sbx.commands.run('exit 3', { onStdout: () => {} }),
    (err: unknown) => {
      assert.ok(err instanceof CommandExitError)
      assert.equal(err.exitCode, 3)
      assert.equal(err.stdout, 'partial output')
      assert.equal(err.stderr, 'boom: it broke')
      return true
    }
  )

  // streaming guest-side timeout keeps TimeoutError semantics
  const seen: string[] = []
  await assert.rejects(
    () => sbx.commands.run('sleep-forever', { onStdout: (d) => seen.push(d) }),
    (err: unknown) => err instanceof TimeoutError
  )
  assert.deepEqual(seen, ['started\n'])

  await sbx.kill()
})

test('create with timeoutMs sends timeout_sec and surfaces expiresAt', async () => {
  const sbx = await Sandbox.create({ ...opts(), timeoutMs: 300_500 })
  assert.equal(lastCreateBody?.timeout_sec, 301) // ceil(300.5)
  assert.ok(sbx.info.expiresAt instanceof Date)

  // plain create sends no body and has no expiry
  await sbx.kill()
  const plain = await Sandbox.create(opts())
  assert.equal(lastCreateBody, undefined)
  assert.equal(plain.info.expiresAt, undefined)
  await plain.kill()
})

test('setTimeout posts ceil(ms/1000) and updates expiresAt', async () => {
  const sbx = await Sandbox.create(opts())
  // Read through a function so assertions don't narrow the property type
  // (setTimeout mutates it behind TS control-flow analysis's back).
  const expiry = () => sbx.info.expiresAt
  assert.equal(expiry(), undefined)

  await sbx.setTimeout(2_500)
  assert.equal(lastTimeoutBody?.timeout_sec, 3)
  assert.ok(expiry() instanceof Date)

  await sbx.setTimeout(0)
  assert.equal(lastTimeoutBody?.timeout_sec, 0)
  assert.equal(expiry(), undefined)

  await sbx.kill()
})

test('exposePort allocates a host port and feeds the getHost cache', async () => {
  const sbx = await Sandbox.create(opts())

  // not exposed yet → sync getHost must throw with a pointer to exposePort
  assert.throws(
    () => sbx.getHost(8000),
    (err: unknown) => err instanceof SandboxError && /exposePort\(8000\)/.test((err as Error).message)
  )

  const host = await sbx.exposePort(8000)
  assert.equal(host, '127.0.0.1:5201')
  assert.equal(sbx.getHost(8000), '127.0.0.1:5201')

  // idempotent: same guest port → same host port
  assert.equal(await sbx.exposePort(8000), '127.0.0.1:5201')

  // exposing the primary port returns the existing primary mapping
  assert.equal(await sbx.exposePort(3000), '127.0.0.1:5200')

  const ports = await sbx.listPorts()
  assert.deepEqual(ports, [
    { guestPort: 3000, hostPort: 5200 },
    { guestPort: 8000, hostPort: 5201 },
  ])

  await sbx.kill()
})

test('static kill destroys a sandbox by id', async () => {
  const sbx = await Sandbox.create(opts())
  await Sandbox.kill(sbx.sandboxId, opts())
  await assert.rejects(
    () => Sandbox.connect(sbx.sandboxId, opts()),
    (err: unknown) => err instanceof NotFoundError
  )
})

test('connect to unknown sandbox throws NotFoundError with server message', async () => {
  await assert.rejects(
    () => Sandbox.connect(SANDBOX_ID, opts()),
    (err: unknown) =>
      err instanceof NotFoundError && /sandbox not found/.test((err as Error).message)
  )
})

test('bad API key throws AuthenticationError', async () => {
  await assert.rejects(
    () => Sandbox.list({ apiUrl, apiKey: 'wrong-key' }),
    (err: unknown) =>
      err instanceof AuthenticationError && /invalid or missing/.test((err as Error).message)
  )
})

test('missing apiUrl/apiKey fail fast with helpful messages', async () => {
  const savedUrl = process.env.SANDBOX_API_URL
  const savedKey = process.env.SANDBOX_API_KEY
  delete process.env.SANDBOX_API_URL
  delete process.env.SANDBOX_API_KEY
  try {
    await assert.rejects(
      () => Sandbox.list(),
      (err: unknown) =>
        err instanceof SandboxError && /SANDBOX_API_URL/.test((err as Error).message)
    )
    await assert.rejects(
      () => Sandbox.list({ apiUrl }),
      (err: unknown) =>
        err instanceof AuthenticationError && /SANDBOX_API_KEY/.test((err as Error).message)
    )
  } finally {
    if (savedUrl !== undefined) process.env.SANDBOX_API_URL = savedUrl
    if (savedKey !== undefined) process.env.SANDBOX_API_KEY = savedKey
  }
})
