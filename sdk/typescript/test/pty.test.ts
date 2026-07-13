import assert from 'node:assert/strict'
import crypto from 'node:crypto'
import http from 'node:http'
import type { Duplex } from 'node:stream'
import { after, before, test } from 'node:test'
import type { AddressInfo } from 'node:net'

import { AuthenticationError, NotFoundError, Pty, Sandbox, SandboxError } from '../src/index.js'

const API_KEY = 'test-key'
const SANDBOX_ID = '0f5e3a1c-1111-2222-3333-444455556666'
const MISSING_ID = 'ffffffff-0000-0000-0000-000000000000'

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
  vcpus: 2,
  mem_mib: 1024,
}

let server: http.Server
let apiUrl: string
/** Query strings of accepted /shell upgrades, for asserting cols/rows/cwd. */
const shellQueries: URLSearchParams[] = []

const GUID = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11'

function handshake(req: http.IncomingMessage, socket: Duplex): void {
  const accept = crypto
    .createHash('sha1')
    .update(`${req.headers['sec-websocket-key']}${GUID}`)
    .digest('base64')
  socket.write(
    'HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n' +
      `Connection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`
  )
}

/** Builds an unmasked server→client frame. Opcode 2 = binary, 1 = text, 8 = close. */
function frame(opcode: number, payload: Buffer): Buffer {
  assert.ok(payload.length < 126, 'test frames stay under the 126-byte length escape')
  return Buffer.concat([Buffer.from([0x80 | opcode, payload.length]), payload])
}

function closeFrame(code: number, reason: string): Buffer {
  const payload = Buffer.alloc(2 + Buffer.byteLength(reason))
  payload.writeUInt16BE(code)
  payload.write(reason, 2)
  return frame(8, payload)
}

/** Decodes one (masked) client→server frame; returns null until it's complete. */
function decodeFrame(buf: Buffer): { opcode: number; payload: Buffer; rest: Buffer } | null {
  if (buf.length < 2) return null
  const opcode = buf[0]! & 0x0f
  const masked = (buf[1]! & 0x80) !== 0
  const len = buf[1]! & 0x7f
  assert.ok(len < 126, 'test client frames stay small')
  const headerLen = 2 + (masked ? 4 : 0)
  if (buf.length < headerLen + len) return null
  let payload = buf.subarray(headerLen, headerLen + len)
  if (masked) {
    const mask = buf.subarray(2, 6)
    payload = Buffer.from(payload.map((b, i) => b ^ mask[i % 4]!))
  }
  return { opcode, payload, rest: buf.subarray(headerLen + len) }
}

before(async () => {
  server = http.createServer((req, res) => {
    // Plain HTTP is only used by Sandbox.connect here.
    const url = new URL(req.url ?? '/', 'http://localhost')
    if (req.method === 'GET' && url.pathname === `/sandboxes/${SANDBOX_ID}`) {
      res.writeHead(200, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify(sandboxRecord))
      return
    }
    res.writeHead(404, { 'Content-Type': 'application/json' })
    res.end(JSON.stringify({ error: 'not found' }))
  })

  server.on('upgrade', (req, socket) => {
    const url = new URL(req.url ?? '/', 'http://localhost')
    handshake(req, socket)

    // The server rejects WS errors AFTER the handshake, as close frames —
    // mirror internal/wsutil so the SDK's mapping is exercised for real.
    if (url.searchParams.get('access_token') !== API_KEY) {
      socket.end(closeFrame(4401, 'missing or invalid bearer token'))
      return
    }
    if (url.pathname === `/sandboxes/${MISSING_ID}/shell`) {
      socket.end(closeFrame(4404, `sandbox ${MISSING_ID} not found`))
      return
    }
    shellQueries.push(url.searchParams)

    // A live shell: greet in a binary frame, then react to client frames.
    socket.write(frame(2, Buffer.from('sandbox$ ')))
    let pending = Buffer.alloc(0)
    socket.on('data', (chunk: Buffer) => {
      pending = Buffer.concat([pending, chunk])
      for (;;) {
        const decoded = decodeFrame(pending)
        if (!decoded || socket.writableEnded) return
        pending = decoded.rest
        if (decoded.opcode === 2 && decoded.payload.toString() === 'exit\n') {
          // Echo, then close like sandboxd does on shell exit.
          socket.write(frame(2, Buffer.from('exit\r\n')))
          socket.end(closeFrame(1000, 'exit:7'))
        } else if (decoded.opcode === 1) {
          // Control message (resize) — echo it back in a binary frame so the
          // test can observe what arrived.
          socket.write(frame(2, Buffer.from(`ctl:${decoded.payload.toString()}`)))
        } else if (decoded.opcode === 8) {
          socket.end(closeFrame(1000, ''))
        }
      }
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

test('pty: connects, streams output, resizes, and reports the exit code', async () => {
  const sbx = await Sandbox.connect(SANDBOX_ID, opts())

  const chunks: string[] = []
  const decoder = new TextDecoder()
  const pty = await sbx.pty.create({
    cols: 120,
    rows: 30,
    cwd: '/home/sandbox',
    onData: (d) => chunks.push(decoder.decode(d)),
  })

  // Handshake query carried the terminal geometry, cwd, and auth.
  const q = shellQueries.at(-1)!
  assert.equal(q.get('cols'), '120')
  assert.equal(q.get('rows'), '30')
  assert.equal(q.get('cwd'), '/home/sandbox')

  pty.resize({ cols: 200, rows: 50 })
  pty.sendInput('exit\n')

  assert.equal(await pty.exited, 7)
  assert.ok(chunks[0]!.startsWith('sandbox$'), `greeting first, got ${JSON.stringify(chunks)}`)
  assert.ok(
    chunks.some((c) => c === 'ctl:{"type":"resize","cols":200,"rows":50}'),
    `resize control frame should reach the server, got ${JSON.stringify(chunks)}`
  )
  assert.ok(
    chunks.some((c) => c === 'exit\r\n'),
    'input should round-trip before exit'
  )
})

test('pty: kill() closes the connection and settles exited', async () => {
  const sbx = await Sandbox.connect(SANDBOX_ID, opts())
  const pty = await sbx.pty.create({ onData: () => {} })
  pty.kill()
  assert.equal(await pty.exited, 0)
})

test('pty: bad token surfaces AuthenticationError, not an opaque close', async () => {
  const sbx = await Sandbox.connect(SANDBOX_ID, opts())
  // Rebuild the handle with a bad key: connect() is not authenticated by the
  // mock, so this only affects the WebSocket.
  const bad = await Sandbox.connect(SANDBOX_ID, { apiUrl, apiKey: 'wrong-key' })
  await assert.rejects(
    () => bad.pty.create({ onData: () => {} }),
    (err: unknown) =>
      err instanceof AuthenticationError && /bearer token/.test((err as Error).message)
  )
  await assert.doesNotReject(async () => {
    const pty = await sbx.pty.create({ onData: () => {} })
    pty.kill()
    await pty.exited
  })
})

test('pty: unknown sandbox surfaces NotFoundError from close code 4404', async () => {
  const sbx = await Sandbox.connect(SANDBOX_ID, opts())
  const client = (sbx as unknown as { client: ConstructorParameters<typeof Pty>[0] }).client
  const ghost = new Pty(client, MISSING_ID)
  await assert.rejects(
    () => ghost.create({ onData: () => {} }),
    (err: unknown) =>
      err instanceof NotFoundError && new RegExp(MISSING_ID).test((err as Error).message)
  )
})

test('pty: unreachable server rejects with a descriptive SandboxError', async () => {
  const sbx = await Sandbox.connect(SANDBOX_ID, opts())
  const dead = await Sandbox.connect(SANDBOX_ID, opts())
  // Point the dead handle's client at a closed port.
  ;(dead as unknown as { client: { baseUrl: string } }).client.baseUrl = 'http://127.0.0.1:1'
  await assert.rejects(
    () => dead.pty.create({ onData: () => {}, connectTimeoutMs: 3_000 }),
    (err: unknown) => err instanceof SandboxError
  )
  // The good handle still works.
  const pty = await sbx.pty.create({ onData: () => {} })
  pty.kill()
  await pty.exited
})
