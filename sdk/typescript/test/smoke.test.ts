import assert from 'node:assert/strict'
import http from 'node:http'
import { after, before, test } from 'node:test'
import type { AddressInfo } from 'node:net'

import {
  AuthenticationError,
  CapacityError,
  CommandExitError,
  ConflictError,
  NotFoundError,
  Sandbox,
  SandboxError,
  TimeoutError,
} from '../src/index.js'

const API_KEY = 'test-key'
const SANDBOX_ID = '0f5e3a1c-1111-2222-3333-444455556666'
const SNAPSHOT_ID = '7b2c9d4e-aaaa-bbbb-cccc-ddddeeeeffff'
const GOLDEN_SNAPSHOT_ID = '1a1a1a1a-golden-0000-0000-000000000000'
const RESTORED_ID = 'c0ffee00-5555-6666-7777-888899990000'

// The server always reports effective resources: the template defaults
// (2 vCPUs / 1024 MiB here) unless the create carried an override.
const TEMPLATE_VCPUS = 2
const TEMPLATE_MEM_MIB = 1024

const sandboxRecord = {
  id: SANDBOX_ID,
  pid: 4242,
  vm_id: 'aaaa-bbbb',
  socket_path: '/run/fc-test.sock',
  tap_device: 'fc0',
  guest_ip: '172.16.0.10',
  rootfs_path: '/opt/fc/instances/test.ext4',
  status: 'running',
  created_at: '2026-06-10T12:00:00Z',
  vcpus: TEMPLATE_VCPUS,
  mem_mib: TEMPLATE_MEM_MIB,
  // Hot creates are clones of the golden snapshot, so the server reports the
  // base they came from; restores and fan-out clones have none.
  base_snapshot_id: GOLDEN_SNAPSHOT_ID,
}

const hostInfoRecord = {
  default_vcpus: TEMPLATE_VCPUS,
  default_mem_mib: TEMPLATE_MEM_MIB,
  max_vcpus: 32,
  max_mem_mib: 64_512,
  hot_create: true,
  hibernate_after_sec: 90,
  host_id: 'testvm-1',
}

// In-memory fake API state
const guestFiles = new Map<string, Buffer>()
const exposedPorts = new Map<number, number>() // guest_port -> host_port
let sandboxAlive = false
let sandboxName: string | undefined
let sandboxExpiresAt: string | undefined
let sandboxResources: { vcpus?: number; mem_mib?: number } = {}
let lastExecBody: Record<string, unknown> | undefined
let lastCreateBody: Record<string, unknown> | undefined
let lastTimeoutBody: Record<string, unknown> | undefined
let lastSnapshotBody: Record<string, unknown> | undefined
let lastRestoreBody: Record<string, unknown> | undefined
let lastFanoutBody: Record<string, unknown> | undefined
let snapshotTaken = false
let snapshotName: string | undefined
/**
 * When set, `POST /sandboxes` answers with this status instead of creating —
 * lets a test drive the capacity (503 + Retry-After) and conflict (409) paths
 * the real server uses to distinguish "full" from "broken".
 */
let createFailure: { status: number; retryAfter?: string; error: string } | undefined

let server: http.Server
let apiUrl: string

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms))

function currentSandboxRecord(): Record<string, unknown> {
  return {
    ...sandboxRecord,
    ...sandboxResources,
    ...(sandboxName ? { name: sandboxName } : {}),
    ...(sandboxExpiresAt !== undefined ? { expires_at: sandboxExpiresAt } : {}),
  }
}

/**
 * A user snapshot of a hot-created sandbox: stored as a diff against the
 * golden base, carrying the source's baked resources.
 */
function currentSnapshotRecord(): Record<string, unknown> {
  return {
    id: SNAPSHOT_ID,
    source_id: SANDBOX_ID,
    tap_device: 'fc0',
    guest_ip: '172.16.0.10',
    mem_path: '/opt/fc/snapshots/mem',
    state_path: '/opt/fc/snapshots/state',
    rootfs_path: '/opt/fc/snapshots/rootfs.ext4',
    source_rootfs_path: '/opt/fc/instances/test.ext4',
    created_at: '2026-06-10T12:10:00Z',
    format: 'diff',
    base_id: GOLDEN_SNAPSHOT_ID,
    vcpus: 4,
    mem_mib: 2048,
    ...(snapshotName ? { name: snapshotName } : {}),
  }
}

/** The server-managed golden snapshot, which `GET /snapshots` also returns. */
const goldenSnapshotRecord = {
  id: GOLDEN_SNAPSHOT_ID,
  source_id: 'throwaway-source',
  tap_device: 'fc99',
  guest_ip: '172.16.0.99',
  mem_path: '/opt/fc/snapshots/golden/mem',
  state_path: '/opt/fc/snapshots/golden/state',
  rootfs_path: '/opt/fc/snapshots/golden/rootfs.ext4',
  source_rootfs_path: '/opt/fc/devbox-rootfs.ext4',
  created_at: '2026-06-10T11:00:00Z',
  golden: true,
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
    if (createFailure) {
      const headers: Record<string, string> = { 'Content-Type': 'application/json' }
      if (createFailure.retryAfter !== undefined) headers['Retry-After'] = createFailure.retryAfter
      res.writeHead(createFailure.status, headers)
      res.end(JSON.stringify({ error: createFailure.error }))
      return
    }
    sandboxAlive = true
    sandboxName =
      typeof lastCreateBody?.name === 'string' && lastCreateBody.name !== ''
        ? lastCreateBody.name
        : undefined
    exposedPorts.clear()
    sandboxExpiresAt =
      typeof lastCreateBody?.timeout_sec === 'number' && lastCreateBody.timeout_sec > 0
        ? '2026-06-10T12:05:00Z'
        : undefined
    sandboxResources = {
      ...(typeof lastCreateBody?.vcpus === 'number' ? { vcpus: lastCreateBody.vcpus } : {}),
      ...(typeof lastCreateBody?.mem_mib === 'number' ? { mem_mib: lastCreateBody.mem_mib } : {}),
    }
    sendJson(res, 201, currentSandboxRecord())
    return
  }

  if (req.method === 'GET' && path === '/info') {
    sendJson(res, 200, hostInfoRecord)
    return
  }

  // Gateway-only fleet view.
  if (req.method === 'GET' && path === '/hosts') {
    sendJson(res, 200, [
      {
        id: 'testvm-1',
        addr: '10.160.0.7:8080',
        slots_total: 64,
        slots_used: 12,
        hibernated: 3,
        free: 50,
        alive: true,
        last_seen_ms_ago: 1200,
      },
      {
        id: 'testvm-2',
        addr: '10.160.0.8:8080',
        slots_total: 64,
        slots_used: 64,
        hibernated: 0,
        free: 0,
        alive: false,
        last_seen_ms_ago: 31_000,
      },
    ])
    return
  }

  // --- snapshots ---

  if (req.method === 'POST' && path === `/sandboxes/${SANDBOX_ID}/snapshot`) {
    const raw = (await readBody(req)).toString()
    lastSnapshotBody = raw ? (JSON.parse(raw) as Record<string, unknown>) : undefined
    if (!sandboxAlive) {
      sendJson(res, 409, { error: `sandbox ${SANDBOX_ID} is not running in this server` })
      return
    }
    snapshotTaken = true
    snapshotName =
      typeof lastSnapshotBody?.name === 'string' && lastSnapshotBody.name !== ''
        ? lastSnapshotBody.name
        : undefined
    sendJson(res, 201, currentSnapshotRecord())
    return
  }

  if (req.method === 'GET' && path === '/snapshots') {
    sendJson(res, 200, [
      goldenSnapshotRecord,
      ...(snapshotTaken ? [currentSnapshotRecord()] : []),
    ])
    return
  }

  if (req.method === 'POST' && path === `/snapshots/${SNAPSHOT_ID}/rename`) {
    const body = JSON.parse((await readBody(req)).toString()) as Record<string, unknown>
    snapshotName = typeof body.name === 'string' && body.name !== '' ? body.name : undefined
    sendJson(res, 200, currentSnapshotRecord())
    return
  }

  if (req.method === 'POST' && path === `/snapshots/${SNAPSHOT_ID}/restore`) {
    const raw = (await readBody(req)).toString()
    lastRestoreBody = raw ? (JSON.parse(raw) as Record<string, unknown>) : undefined
    // A restore reuses the snapshot's baked identity, so a live source is 409.
    if (sandboxAlive) {
      sendJson(res, 409, { error: 'source sandbox still running' })
      return
    }
    const record = { ...currentSandboxRecord(), id: RESTORED_ID, vcpus: 4, mem_mib: 2048 }
    // Restores are not golden clones: resources come from the snapshot, and
    // there is no base to diff against.
    delete (record as Record<string, unknown>).base_snapshot_id
    sendJson(res, 201, record)
    return
  }

  if (req.method === 'POST' && path === `/snapshots/${SNAPSHOT_ID}/fanout`) {
    const body = JSON.parse((await readBody(req)).toString()) as Record<string, unknown>
    lastFanoutBody = body
    const count = Number(body.count ?? 0)
    // Partial success is normal: the last clone always fails here.
    const clones = Array.from({ length: Math.max(0, count - 1) }, (_, i) => ({
      ...currentSandboxRecord(),
      id: `clone-${i}`,
      guest_ip: `172.16.0.${20 + i}`,
      vcpus: 4,
      mem_mib: 2048,
    }))
    sendJson(res, 201, clones)
    return
  }

  if (req.method === 'DELETE' && path === `/snapshots/${SNAPSHOT_ID}`) {
    if (!snapshotTaken) {
      sendJson(res, 404, { error: 'snapshot not found' })
      return
    }
    snapshotTaken = false
    snapshotName = undefined
    res.writeHead(204)
    res.end()
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

  if (req.method === 'POST' && path === `/sandboxes/${SANDBOX_ID}/rename`) {
    const body = JSON.parse((await readBody(req)).toString()) as Record<string, unknown>
    sandboxName = typeof body.name === 'string' && body.name !== '' ? body.name : undefined
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

  if (req.method === 'POST' && path === `/sandboxes/${SANDBOX_ID}/hibernate`) {
    sendJson(res, 200, { ...currentSandboxRecord(), status: 'hibernated' })
    return
  }

  if (req.method === 'POST' && path === `/sandboxes/${SANDBOX_ID}/ports`) {
    const body = JSON.parse((await readBody(req)).toString()) as { guest_port: number }
    const guestPort = body.guest_port
    let hostPort = exposedPorts.get(guestPort)
    if (hostPort === undefined) {
      hostPort = 5200 + exposedPorts.size
      exposedPorts.set(guestPort, hostPort)
    }
    sendJson(res, 200, { guest_port: guestPort, host_port: hostPort })
    return
  }

  if (req.method === 'GET' && path === `/sandboxes/${SANDBOX_ID}/ports`) {
    const mappings: Array<{ guest_port: number; host_port: number }> = []
    for (const [guestPort, hostPort] of exposedPorts) {
      mappings.push({ guest_port: guestPort, host_port: hostPort })
    }
    mappings.sort((a, b) => a.guest_port - b.guest_port)
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

  // No port is forwarded until explicitly exposed.
  assert.throws(
    () => sbx.getHost(3000),
    (err: unknown) => err instanceof SandboxError && /exposePort\(3000\)/.test((err as Error).message)
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

test('create with name sends it and rename updates it', async () => {
  const sbx = await Sandbox.create({ ...opts(), name: 'my devbox' })
  assert.equal(lastCreateBody?.name, 'my devbox')
  assert.equal(sbx.info.name, 'my devbox')

  await sbx.rename('renamed')
  assert.equal(sbx.info.name, 'renamed')

  // an empty name clears it, and unnamed sandboxes surface no name at all
  await sbx.rename('')
  assert.equal(sbx.info.name, undefined)
  await sbx.kill()

  const plain = await Sandbox.create(opts())
  assert.equal(lastCreateBody, undefined)
  assert.equal(plain.info.name, undefined)
  await plain.kill()
})

test('create with vcpus/memMib sends the resource overrides and surfaces them', async () => {
  const sbx = await Sandbox.create({ ...opts(), vcpus: 4, memMib: 2048 })
  assert.equal(lastCreateBody?.vcpus, 4)
  assert.equal(lastCreateBody?.mem_mib, 2048)
  assert.equal(sbx.info.vcpus, 4)
  assert.equal(sbx.info.memMib, 2048)
  await sbx.kill()

  // plain create sends neither field; the server reports the effective
  // (template-default) resources, and the SDK surfaces them as-is.
  const plain = await Sandbox.create(opts())
  assert.equal(lastCreateBody, undefined)
  assert.equal(plain.info.vcpus, TEMPLATE_VCPUS)
  assert.equal(plain.info.memMib, TEMPLATE_MEM_MIB)
  await plain.kill()
})

test('hostInfo maps the /info payload to camelCase', async () => {
  const info = await Sandbox.hostInfo(opts())
  assert.deepEqual(info, {
    defaultVcpus: TEMPLATE_VCPUS,
    defaultMemMib: TEMPLATE_MEM_MIB,
    maxVcpus: 32,
    maxMemMib: 64_512,
    hotCreate: true,
    hibernateAfterSec: 90,
    hostId: 'testvm-1',
  })
})

test('create with hibernateAfterMs sends hibernate_after_sec; -1 passes through unscaled', async () => {
  const sbx = await Sandbox.create({ ...opts(), hibernateAfterMs: 90_500 })
  assert.equal(lastCreateBody?.hibernate_after_sec, 91) // ceil(90.5)
  await sbx.kill()

  const never = await Sandbox.create({ ...opts(), hibernateAfterMs: -1 })
  assert.equal(lastCreateBody?.hibernate_after_sec, -1)
  await never.kill()
})

test('hibernate() posts to the hibernate endpoint and updates status', async () => {
  const sbx = await Sandbox.create(opts())
  assert.equal(sbx.info.status, 'running')
  await sbx.hibernate()
  assert.equal(sbx.info.status, 'hibernated')
  await sbx.kill()
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
  assert.equal(host, '127.0.0.1:5200')
  assert.equal(sbx.getHost(8000), '127.0.0.1:5200')

  // idempotent: same guest port → same host port
  assert.equal(await sbx.exposePort(8000), '127.0.0.1:5200')

  // Port 3000 is ordinary and receives a mapping only when requested.
  assert.equal(await sbx.exposePort(3000), '127.0.0.1:5201')

  const ports = await sbx.listPorts()
  assert.deepEqual(ports, [
    { guestPort: 3000, hostPort: 5201 },
    { guestPort: 8000, hostPort: 5200 },
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

test('create with sshPubkey sends ssh_pubkey', async () => {
  const key = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample me@laptop'
  const sbx = await Sandbox.create({ ...opts(), sshPubkey: key })
  assert.equal(lastCreateBody?.ssh_pubkey, key)
  // The key is only useful once :22 is forwarded out.
  const addr = await sbx.exposePort(22)
  assert.match(addr, /^127\.0\.0\.1:\d+$/)
  await sbx.kill()

  const plain = await Sandbox.create(opts())
  assert.equal(lastCreateBody, undefined)
  await plain.kill()
})

test('create surfaces baseSnapshotId for golden clones', async () => {
  const sbx = await Sandbox.create(opts())
  assert.equal(sbx.info.baseSnapshotId, GOLDEN_SNAPSHOT_ID)
  await sbx.kill()
})

test('snapshot lifecycle: take → list → rename → delete', async () => {
  const sbx = await Sandbox.create(opts())

  const snap = await sbx.snapshot({ name: 'deps-installed' })
  assert.equal(lastSnapshotBody?.name, 'deps-installed')
  assert.equal(snap.snapshotId, SNAPSHOT_ID)
  assert.equal(snap.sourceId, SANDBOX_ID)
  assert.equal(snap.name, 'deps-installed')
  assert.ok(snap.createdAt instanceof Date)
  // Diff metadata and the snapshot's baked resources come through.
  assert.equal(snap.format, 'diff')
  assert.equal(snap.baseId, GOLDEN_SNAPSHOT_ID)
  assert.equal(snap.vcpus, 4)
  assert.equal(snap.memMib, 2048)
  assert.equal(snap.golden, undefined)

  // The golden snapshot is listed like any other, flagged so a UI can hide it.
  const listed = await Sandbox.listSnapshots(opts())
  assert.equal(listed.length, 2)
  const golden = listed.find((s) => s.snapshotId === GOLDEN_SNAPSHOT_ID)
  assert.equal(golden?.golden, true)
  assert.equal(golden?.format, undefined) // absent = full
  assert.equal(listed.find((s) => s.snapshotId === SNAPSHOT_ID)?.golden, undefined)

  const renamed = await Sandbox.renameSnapshot(SNAPSHOT_ID, 'ready', opts())
  assert.equal(renamed.name, 'ready')
  const cleared = await Sandbox.renameSnapshot(SNAPSHOT_ID, '', opts())
  assert.equal(cleared.name, undefined)

  await Sandbox.deleteSnapshot(SNAPSHOT_ID, opts())
  assert.deepEqual(
    (await Sandbox.listSnapshots(opts())).map((s) => s.snapshotId),
    [GOLDEN_SNAPSHOT_ID]
  )
  await assert.rejects(
    () => Sandbox.deleteSnapshot(SNAPSHOT_ID, opts()),
    (err: unknown) => err instanceof NotFoundError
  )
  await sbx.kill()
})

test('snapshot of a sandbox that is not running throws ConflictError', async () => {
  const sbx = await Sandbox.create(opts())
  await sbx.kill()
  await assert.rejects(
    () => sbx.snapshot(),
    (err: unknown) => {
      assert.ok(err instanceof ConflictError)
      assert.equal(err.status, 409)
      assert.match(err.message, /not running/)
      return true
    }
  )
})

test('restore sends timeout_sec + hibernate_after_sec and reports the snapshot resources', async () => {
  const sbx = await Sandbox.create(opts())
  await sbx.snapshot()

  // A live source still owns the snapshot's baked identity → 409.
  await assert.rejects(
    () => Sandbox.restore(SNAPSHOT_ID, opts()),
    (err: unknown) => err instanceof ConflictError
  )
  await sbx.kill()

  const restored = await Sandbox.restore(SNAPSHOT_ID, {
    ...opts(),
    name: 'from-snapshot',
    timeoutMs: 600_000,
    hibernateAfterMs: 120_500,
  })
  assert.equal(lastRestoreBody?.name, 'from-snapshot')
  assert.equal(lastRestoreBody?.timeout_sec, 600)
  assert.equal(lastRestoreBody?.hibernate_after_sec, 121) // ceil(120.5)
  // Never sent — the server 400s resource overrides on restore.
  assert.equal('vcpus' in (lastRestoreBody ?? {}), false)
  assert.equal('mem_mib' in (lastRestoreBody ?? {}), false)
  assert.equal(restored.sandboxId, RESTORED_ID)
  assert.equal(restored.info.vcpus, 4)
  assert.equal(restored.info.memMib, 2048)
  assert.equal(restored.info.baseSnapshotId, undefined)

  // hibernateAfterMs: -1 ("never") passes through unscaled here too.
  await Sandbox.restore(SNAPSHOT_ID, { ...opts(), hibernateAfterMs: -1 })
  assert.equal(lastRestoreBody?.hibernate_after_sec, -1)

  // An empty options object sends no body fields at all.
  await Sandbox.restore(SNAPSHOT_ID, opts())
  assert.equal(lastRestoreBody, undefined)
})

test('fanout sends count + hibernate_after_sec and tolerates partial success', async () => {
  const clones = await Sandbox.fanout(SNAPSHOT_ID, 4, {
    ...opts(),
    timeoutMs: 300_000,
    hibernateAfterMs: -1,
  })
  assert.equal(lastFanoutBody?.count, 4)
  assert.equal(lastFanoutBody?.timeout_sec, 300)
  assert.equal(lastFanoutBody?.hibernate_after_sec, -1)
  // The mock fails one clone, mirroring the API's documented partial success.
  assert.equal(clones.length, 3)
  assert.deepEqual(
    clones.map((c) => c.sandboxId),
    ['clone-0', 'clone-1', 'clone-2']
  )
  assert.equal(clones[0]?.info.vcpus, 4)

  await assert.rejects(() => Sandbox.fanout(SNAPSHOT_ID, 0, opts()), /count must be a positive integer/)
})

test('refresh() re-reads the sandbox and updates info in place', async () => {
  const sbx = await Sandbox.create({ ...opts(), timeoutMs: 60_000, name: 'before' })
  const info = sbx.info
  assert.ok(info.expiresAt instanceof Date)
  assert.equal(info.name, 'before')

  // Change state out of band, the way the reaper or another client would.
  await Sandbox.kill(SANDBOX_ID, opts())
  await assert.rejects(
    () => sbx.refresh(),
    (err: unknown) => err instanceof NotFoundError
  )

  const live = await Sandbox.create({ ...opts(), name: 'after' })
  const refreshed = await live.refresh()
  assert.equal(refreshed, live.info) // same object — held references stay live
  assert.equal(refreshed.name, 'after')
  // A cleared TTL is dropped, not left stale.
  assert.equal(refreshed.expiresAt, undefined)
  await live.kill()
})

test('a full fleet surfaces CapacityError with the Retry-After hint', async () => {
  createFailure = {
    status: 503,
    retryAfter: '5',
    error: 'no host with free capacity; retry shortly',
  }
  try {
    await assert.rejects(
      () => Sandbox.create(opts()),
      (err: unknown) => {
        assert.ok(err instanceof CapacityError)
        assert.ok(err instanceof SandboxError)
        assert.equal(err.status, 503)
        assert.equal(err.retryAfterMs, 5000)
        assert.match(err.message, /free capacity/)
        return true
      }
    )

    // 429 is the same class; a missing Retry-After just leaves the hint unset.
    createFailure = { status: 429, error: 'too many creates in flight' }
    await assert.rejects(
      () => Sandbox.create(opts()),
      (err: unknown) => {
        assert.ok(err instanceof CapacityError)
        assert.equal(err.status, 429)
        assert.equal(err.retryAfterMs, undefined)
        return true
      }
    )

    // A genuine server error stays a plain SandboxError — not retryable.
    createFailure = { status: 500, error: 'vm boot failure' }
    await assert.rejects(
      () => Sandbox.create(opts()),
      (err: unknown) => {
        assert.ok(err instanceof SandboxError)
        assert.equal(err instanceof CapacityError, false)
        assert.equal(err.status, 500)
        return true
      }
    )
  } finally {
    createFailure = undefined
  }
})

test('hosts() maps the gateway fleet view to camelCase', async () => {
  const hosts = await Sandbox.hosts(opts())
  assert.deepEqual(hosts, [
    {
      hostId: 'testvm-1',
      addr: '10.160.0.7:8080',
      slotsTotal: 64,
      slotsUsed: 12,
      hibernated: 3,
      free: 50,
      alive: true,
      lastSeenMsAgo: 1200,
    },
    {
      hostId: 'testvm-2',
      addr: '10.160.0.8:8080',
      slotsTotal: 64,
      slotsUsed: 64,
      hibernated: 0,
      free: 0,
      alive: false,
      lastSeenMsAgo: 31_000,
    },
  ])
})

test('errors carry the HTTP status they came from', async () => {
  await assert.rejects(
    () => Sandbox.connect('nope', opts()),
    (err: unknown) => {
      assert.ok(err instanceof NotFoundError)
      assert.equal(err.status, 404)
      return true
    }
  )
  await assert.rejects(
    () => Sandbox.list({ apiUrl, apiKey: 'wrong' }),
    (err: unknown) => {
      assert.ok(err instanceof AuthenticationError)
      assert.equal(err.status, 401)
      return true
    }
  )
})
