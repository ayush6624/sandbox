/**
 * Lifecycle correctness: create, connect, list, kill, error mapping, and the
 * TTL reaper. These are the invariants everything else builds on.
 */

import { Sandbox } from '../../sdk/typescript/src/index.js'
import { SuiteDef, assert, assertEq, assertThrows, eventually, sleep } from '../harness.js'

export const suite = new SuiteDef('lifecycle')

suite.test('create returns a usable sandbox with sane info', async (ctx) => {
  const sbx = await ctx.createTracked()
  assert(sbx.sandboxId.length > 0, 'sandboxId must be non-empty')
  assert(sbx.info.guestIp.length > 0, 'guestIp must be set')
  assert(sbx.info.hostPort > 0, 'hostPort must be allocated')
  assertEq(sbx.info.status, 'running', 'fresh sandbox must be running')
  assert(sbx.info.expiresAt === undefined, 'no TTL requested, expiresAt must be absent')
  // First exec proves the guest agent is genuinely up, not just registered.
  const res = await sbx.commands.run('echo ready')
  assertEq(res.stdout.trim(), 'ready', 'first exec round-trip')
  assertEq(res.exitCode, 0, 'exit code')
})

suite.test('connect by id yields a working handle', async (ctx) => {
  const sbx = await ctx.createTracked()
  const again = await Sandbox.connect(sbx.sandboxId)
  assertEq(again.sandboxId, sbx.sandboxId, 'connect must return the same id')
  assertEq(again.info.hostPort, sbx.info.hostPort, 'hostPort must match')
  const res = await again.commands.run('hostname')
  assertEq(res.exitCode, 0, 'exec through the connected handle')
})

suite.test('list contains created sandboxes and drops killed ones', async (ctx) => {
  const sbx = await ctx.createTracked()
  const listed = await Sandbox.list()
  assert(
    listed.some((s) => s.sandboxId === sbx.sandboxId),
    'created sandbox must appear in list()'
  )
  await sbx.kill()
  ctx.untrack(sbx)
  await eventually(
    async () => {
      const after = await Sandbox.list()
      return after.every((s) => s.sandboxId !== sbx.sandboxId)
    },
    { timeoutMs: 10_000, what: 'killed sandbox to disappear from list()' }
  )
})

suite.test('operations on a killed sandbox raise NotFoundError', async (ctx) => {
  const sbx = await ctx.createTracked()
  await sbx.kill()
  ctx.untrack(sbx)
  await assertThrows(() => Sandbox.connect(sbx.sandboxId), 'NotFoundError', 'connect after kill')
  await assertThrows(() => sbx.commands.run('echo hi'), 'NotFoundError', 'exec after kill')
  await assertThrows(() => sbx.kill(), 'NotFoundError', 'double kill')
})

suite.test('connect to a bogus id raises NotFoundError', async () => {
  await assertThrows(
    () => Sandbox.connect('does-not-exist-0000'),
    'NotFoundError',
    'connect to unknown id'
  )
})

suite.test('a bad API key raises AuthenticationError', async () => {
  await assertThrows(
    () => Sandbox.list({ apiKey: 'definitely-not-the-token' }),
    'AuthenticationError',
    'list with a wrong bearer token'
  )
})

suite.test('TTL: sandbox with timeoutMs is reaped after expiry', async (ctx) => {
  const sbx = await ctx.createTracked({ timeoutMs: 5_000 })
  assert(sbx.info.expiresAt instanceof Date, 'expiresAt must be set when timeoutMs is passed')
  // Reaper ticks every ~10s, so allow expiry + one full tick + slack.
  await eventually(
    async () => {
      const listed = await Sandbox.list()
      return listed.every((s) => s.sandboxId !== sbx.sandboxId)
    },
    { timeoutMs: 30_000, intervalMs: 1_000, what: 'TTL reaper to destroy the expired sandbox' }
  )
  ctx.untrack(sbx)
})

suite.test('setTimeout extends and clears the TTL', async (ctx) => {
  const sbx = await ctx.createTracked({ timeoutMs: 8_000 })
  const firstExpiry = sbx.info.expiresAt!.getTime()

  await sbx.setTimeout(120_000)
  assert(sbx.info.expiresAt !== undefined, 'expiresAt must survive a reset')
  assert(
    sbx.info.expiresAt.getTime() > firstExpiry + 60_000,
    'reset must push expiry well past the original deadline'
  )

  await sbx.setTimeout(0)
  assert(sbx.info.expiresAt === undefined, 'setTimeout(0) must clear the TTL')

  // Outlive the original 8s deadline to prove the clear actually stuck server-side.
  await sleep(11_000)
  const res = await sbx.commands.run('echo alive')
  assertEq(res.stdout.trim(), 'alive', 'sandbox must survive past the original TTL after clearing it')
})
