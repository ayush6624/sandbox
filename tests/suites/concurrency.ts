/**
 * Concurrency stress: burst creates through the gateway, unique resource
 * allocation under racing creates, parallel execs against one agent, and
 * parallel teardown. Sized by STRESS_BURST (default 24).
 */

import { Sandbox } from '../../sdk/typescript/src/index.js'
import {
  SuiteDef,
  assert,
  assertEq,
  envInt,
  eventually,
  pool,
  statLine,
  timed,
} from '../harness.js'

export const suite = new SuiteDef('concurrency')

const BURST = envInt('STRESS_BURST', 24)

suite.test(`burst: ${BURST} concurrent creates all come up usable`, async (ctx) => {
  const createTimes: number[] = []
  const sandboxes = await Promise.all(
    Array.from({ length: BURST }, async () => {
      const { result, ms } = await timed(() => ctx.createTracked())
      createTimes.push(ms)
      return result
    })
  )
  ctx.log(statLine('create', createTimes))

  // Unique ids, and unique (host, port) pairs — the pools must never collide.
  const ids = new Set(sandboxes.map((s) => s.sandboxId))
  assertEq(ids.size, BURST, 'sandbox ids must be unique')
  const endpoints = new Set(sandboxes.map((s) => `${s.info.hostAddr ?? 'local'}:${s.info.hostPort}`))
  assertEq(endpoints.size, BURST, 'host:port endpoints must be unique')
  const guestKeys = new Set(sandboxes.map((s) => `${s.info.hostAddr ?? 'local'}|${s.info.guestIp}`))
  assertEq(guestKeys.size, BURST, 'guest IPs must be unique per host')

  // Placement spread (informational — burst placement is known to favor the
  // least-loaded host as of the 5s heartbeat granularity).
  const byHost = new Map<string, number>()
  for (const s of sandboxes) {
    const h = s.info.hostAddr ?? 'direct'
    byHost.set(h, (byHost.get(h) ?? 0) + 1)
  }
  ctx.log(
    'placement: ' +
      [...byHost.entries()].map(([h, n]) => `${h.split(':')[0]}=${n}`).join(' ')
  )

  // Every sandbox must actually execute a command.
  const execTimes: number[] = []
  await pool(12, sandboxes, async (sbx, i) => {
    const { result, ms } = await timed(() => sbx.commands.run(`echo sbx-${i}`))
    execTimes.push(ms)
    assertEq(result.stdout.trim(), `sbx-${i}`, `exec on sandbox #${i}`)
  })
  ctx.log(statLine('first exec', execTimes))

  // The gateway's scatter-gather list must see all of them.
  const listed = new Set((await Sandbox.list()).map((s) => s.sandboxId))
  for (const s of sandboxes) {
    assert(listed.has(s.sandboxId), `sandbox ${s.sandboxId} missing from list()`)
  }

  // Parallel teardown.
  const { ms: killMs } = await timed(() =>
    pool(12, sandboxes, async (sbx) => {
      await sbx.kill()
      ctx.untrack(sbx)
    })
  )
  ctx.log(`killed ${BURST} in ${Math.round(killMs)}ms`)

  await eventually(
    async () => {
      const after = new Set((await Sandbox.list()).map((s) => s.sandboxId))
      return sandboxes.every((s) => !after.has(s.sandboxId))
    },
    { timeoutMs: 15_000, what: 'all burst sandboxes to disappear from list()' }
  )
})

suite.test('16 parallel execs against a single sandbox', async (ctx) => {
  const sbx = await ctx.createTracked()
  const results = await Promise.all(
    Array.from({ length: 16 }, (_, i) =>
      sbx.commands.run(`echo par-${i}; sleep 0.2; echo done-${i}`)
    )
  )
  results.forEach((res, i) => {
    assertEq(res.exitCode, 0, `parallel exec #${i} exit code`)
    assertEq(res.stdout, `par-${i}\ndone-${i}\n`, `parallel exec #${i} output`)
  })
})

suite.test('parallel mixed workload on one sandbox: exec + files + ports', async (ctx) => {
  const sbx = await ctx.createTracked()
  await Promise.all([
    sbx.commands.run('for i in $(seq 1 20); do echo $i; done'),
    sbx.files.write('/home/sandbox/mixed.txt', 'x'.repeat(100_000)),
    sbx.exposePort(8000),
    sbx.commands.run('dd if=/dev/zero of=/tmp/dd.bin bs=1M count=16 2>/dev/null && sync'),
    sbx.files.list('/home/sandbox'),
  ])
  const back = await sbx.files.read('/home/sandbox/mixed.txt')
  assertEq(back.length, 100_000, 'file written during mixed load must be intact')
  const ports = await sbx.listPorts()
  assert(ports.some((p) => p.guestPort === 8000), 'port exposed during mixed load must exist')
})

suite.test('create while others are being killed (churny overlap)', async (ctx) => {
  const first = await Promise.all(Array.from({ length: 6 }, () => ctx.createTracked()))
  const [replacements] = await Promise.all([
    Promise.all(Array.from({ length: 6 }, () => ctx.createTracked())),
    pool(6, first, async (sbx) => {
      await sbx.kill()
      ctx.untrack(sbx)
    }),
  ])
  await pool(6, replacements, async (sbx, i) => {
    const res = await sbx.commands.run(`echo overlap-${i}`)
    assertEq(res.stdout.trim(), `overlap-${i}`, `replacement sandbox #${i} must work`)
  })
})
