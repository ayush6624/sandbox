/**
 * Port forwarding: the always-on primary mapping (guest 3000), on-demand
 * exposePort, listPorts, and actually reaching guest servers from outside
 * through the forwarded host ports.
 */

import { SuiteDef, assert, assertEq, assertThrows, eventually } from '../harness.js'
import type { Ctx } from '../harness.js'
import { Sandbox } from '../../sdk/typescript/src/index.js'

export const suite = new SuiteDef('ports')

const SERVER_JS = `
const http = require('http')
const port = Number(process.env.PORT || 3000)
http
  .createServer((req, res) => {
    res.setHeader('content-type', 'text/plain')
    res.end('pong:' + port + ':' + req.url)
  })
  .listen(port, () => console.log('listening on', port))
`

async function startServer(sbx: Sandbox, port: number): Promise<void> {
  await sbx.files.write('/home/sandbox/srv.js', SERVER_JS)
  await sbx.commands.run(
    `nohup env PORT=${port} node /home/sandbox/srv.js >/tmp/srv-${port}.log 2>&1 & echo ok`
  )
}

async function fetchText(url: string): Promise<string> {
  const res = await fetch(url, { signal: AbortSignal.timeout(3_000) })
  assert(res.ok, `GET ${url} responded ${res.status}`)
  return res.text()
}

suite.test('guest port 3000 is reachable via getHost() from outside', async (ctx) => {
  const sbx = await ctx.createTracked()
  await startServer(sbx, 3000)
  const host = sbx.getHost(3000)
  assert(/^[\d.]+:\d+$/.test(host), `getHost must return host:port, got ${host}`)
  const body = await eventually(() => fetchText(`http://${host}/hello`), {
    timeoutMs: 15_000,
    what: `guest server on 3000 to answer via ${host}`,
  })
  assertEq(body, 'pong:3000:/hello', 'response must come from the guest server')
})

suite.test('getHost throws for a port that was never exposed', async (ctx) => {
  const sbx = await ctx.createTracked()
  await assertThrows(async () => sbx.getHost(8123), 'SandboxError', 'getHost on unexposed port')
})

suite.test('exposePort forwards an extra guest port end-to-end', async (ctx) => {
  const sbx = await ctx.createTracked()
  await startServer(sbx, 8000)
  const hostPort = await sbx.exposePort(8000)
  assert(/^[\d.]+:\d+$/.test(hostPort), `exposePort must return host:port, got ${hostPort}`)
  assertEq(sbx.getHost(8000), hostPort, 'getHost must work after exposePort')

  const body = await eventually(() => fetchText(`http://${hostPort}/api`), {
    timeoutMs: 15_000,
    what: `guest server on 8000 to answer via ${hostPort}`,
  })
  assertEq(body, 'pong:8000:/api', 'response must come from the second server')
})

suite.test('exposePort is idempotent', async (ctx) => {
  const sbx = await ctx.createTracked()
  const first = await sbx.exposePort(8080)
  const second = await sbx.exposePort(8080)
  assertEq(second, first, 'exposing the same port twice must return the same mapping')
})

suite.test('listPorts reports the primary and extra mappings', async (ctx) => {
  const sbx = await ctx.createTracked()
  await sbx.exposePort(8000)
  await sbx.exposePort(9000)
  const ports = await sbx.listPorts()
  const guestPorts = ports.map((p) => p.guestPort).sort((a, b) => a - b)
  assertEq(JSON.stringify(guestPorts), JSON.stringify([3000, 8000, 9000]), 'guest ports listed')
  const primary = ports.find((p) => p.guestPort === 3000)
  assertEq(primary?.hostPort, sbx.info.hostPort, 'primary mapping must match create-time port')
  const hostPorts = new Set(ports.map((p) => p.hostPort))
  assertEq(hostPorts.size, ports.length, 'host ports must be distinct')
})

suite.test('two sandboxes get distinct host ports and isolated servers', async (ctx) => {
  const [a, b] = await Promise.all([ctx.createTracked(), ctx.createTracked()])
  await Promise.all([startServer(a, 3000), startServer(b, 3000)])
  const [bodyA, bodyB] = await Promise.all([
    eventually(() => fetchText(`http://${a.getHost(3000)}/A`), {
      timeoutMs: 15_000,
      what: 'sandbox A server',
    }),
    eventually(() => fetchText(`http://${b.getHost(3000)}/B`), {
      timeoutMs: 15_000,
      what: 'sandbox B server',
    }),
  ])
  assertEq(bodyA, 'pong:3000:/A', 'sandbox A must answer its own port')
  assertEq(bodyB, 'pong:3000:/B', 'sandbox B must answer its own port')
  if (a.info.hostAddr === b.info.hostAddr) {
    assert(a.info.hostPort !== b.info.hostPort, 'same host ⇒ ports must differ')
  }
})

suite.test('killed sandbox stops answering on its forwarded port', async (ctx) => {
  const sbx = await ctx.createTracked()
  await startServer(sbx, 3000)
  const host = sbx.getHost(3000)
  await eventually(() => fetchText(`http://${host}/up`), {
    timeoutMs: 15_000,
    what: 'server up before kill',
  })
  await sbx.kill()
  ctx.untrack(sbx)
  await eventually(
    async () => {
      try {
        await fetchText(`http://${host}/gone`)
        return false // still answering
      } catch {
        return true // connection refused / timed out — listener closed
      }
    },
    { timeoutMs: 10_000, what: 'forwarded port to stop answering after kill' }
  )
})

suite.test('connecting to a forwarded port wakes a hibernated sandbox', async (ctx) => {
  const sbx = await ctx.createTracked()
  await startServer(sbx, 3000)
  const host = sbx.getHost(3000)
  await eventually(() => fetchText(`http://${host}/pre-freeze`), {
    timeoutMs: 15_000,
    what: 'guest server up before hibernating',
  })

  await sbx.hibernate()
  const frozen = (await Sandbox.list()).find((s) => s.sandboxId === sbx.sandboxId)
  assertEq(frozen?.status, 'hibernated', 'sandbox must be frozen before the connect')

  // Plain TCP to the forwarded port — no API call — must wake it: the
  // userspace proxy holds the listener through hibernation, wakes on accept,
  // and dials the guest's (possibly new) IP. Generous timeout: the wake
  // happens inline, inside this request.
  const res = await fetch(`http://${host}/wake-on-connect`, {
    signal: AbortSignal.timeout(60_000),
  })
  assert(res.ok, `GET through hibernated port responded ${res.status}`)
  assertEq(await res.text(), 'pong:3000:/wake-on-connect', 'guest server must answer post-wake')

  const woken = (await Sandbox.list()).find((s) => s.sandboxId === sbx.sandboxId)
  assertEq(woken?.status, 'running', 'forwarded-port traffic must have woken the sandbox')
})
