/**
 * Idle hibernation: manual freeze via sandbox.hibernate(), transparent
 * wake-on-exec with memory state intact, the per-sandbox
 * hibernate_after_sec override (auto-freeze), and destroy-while-frozen.
 *
 * The auto-freeze test uses a short override window; the host's reaper ticks
 * every 30 s, so expect freeze within roughly window + 35 s.
 */

import { Sandbox } from '../../sdk/typescript/src/index.js'
import { SuiteDef, assert, assertEq, assertThrows, eventually, timed } from '../harness.js'

export const suite = new SuiteDef('hibernate')

/** Polls list() (which never wakes anything) for a sandbox's status. */
async function status(sandboxId: string): Promise<string | undefined> {
  const all = await Sandbox.list()
  return all.find((s) => s.sandboxId === sandboxId)?.status
}

suite.test('manual hibernate frees the sandbox and exec wakes it with state intact', async (ctx) => {
  const sbx = await ctx.createTracked()
  // Plant state a reboot would lose: an on-disk file AND a live process.
  const planted = await sbx.commands.run(
    'echo frozen-proof > /tmp/proof && nohup sleep 900 >/dev/null 2>&1 & sleep 0.2; pgrep -c -f "sleep 900"'
  )
  assertEq(planted.exitCode, 0, 'plant state')
  assert(parseInt(planted.stdout.trim(), 10) >= 1, 'background process must be running')

  await sbx.hibernate()
  assertEq(sbx.info.status, 'hibernated', 'hibernate() must update the handle status')
  assertEq(await status(sbx.sandboxId), 'hibernated', 'list() must show the frozen status')

  // Any exec must transparently wake it — and the frozen process must have
  // survived, which distinguishes hibernation from a quiet reboot.
  const { result: woken, ms } = await timed(() =>
    sbx.commands.run('cat /tmp/proof && pgrep -c -f "sleep 900"')
  )
  assertEq(woken.exitCode, 0, 'exec on a hibernated sandbox must wake it')
  const [proof, procs] = woken.stdout.trim().split('\n')
  assertEq(proof, 'frozen-proof', 'disk state must survive the freeze')
  assert(parseInt(procs ?? '0', 10) >= 1, 'in-memory process must survive the freeze')
  assertEq(await status(sbx.sandboxId), 'running', 'sandbox must be running again')
  // Generous bound: same-identity wake is ~50 ms server-side; even the
  // fresh-identity fallback (reidentify + GARP) lands well under this.
  assert(ms < 30_000, `wake+exec took ${ms}ms, expected well under 30s`)
})

suite.test('hibernate is idempotent-ish: second call on a frozen sandbox fails cleanly', async (ctx) => {
  const sbx = await ctx.createTracked()
  await sbx.hibernate()
  // Not running → the API refuses rather than double-freezing.
  await assertThrows(() => sbx.hibernate(), 'SandboxError', 'hibernate while hibernated')
  // Still wakeable afterwards.
  const res = await sbx.commands.run('echo ok')
  assertEq(res.stdout.trim(), 'ok', 'wake after failed double-hibernate')
})

suite.test('per-sandbox hibernate_after_sec auto-freezes an idle sandbox', async (ctx) => {
  // 35 s window + 30 s reaper tick → frozen within ~65 s of the last request.
  const sbx = await ctx.createTracked({ hibernateAfterMs: 35_000 })
  assertEq(sbx.info.hibernateAfterSec, 35, 'create must echo the override')

  await eventually(async () => (await status(sbx.sandboxId)) === 'hibernated', {
    timeoutMs: 120_000,
    intervalMs: 5_000,
    what: 'idle sandbox to auto-hibernate after its 35s window',
  })

  const res = await sbx.commands.run('echo back')
  assertEq(res.stdout.trim(), 'back', 'auto-frozen sandbox must wake on exec')
})

suite.test('hibernateAfterMs: -1 round-trips as the never-hibernate sentinel', async (ctx) => {
  const sbx = await ctx.createTracked({ hibernateAfterMs: -1 })
  assertEq(sbx.info.hibernateAfterSec, -1, 'create must record the opt-out')
  const again = await Sandbox.connect(sbx.sandboxId)
  assertEq(again.info.hibernateAfterSec, -1, 'opt-out must persist on connect')
})

suite.test('kill destroys a hibernated sandbox', async (ctx) => {
  const sbx = await ctx.createTracked()
  await sbx.hibernate()
  await sbx.kill()
  ctx.untrack(sbx)
  await assertThrows(() => Sandbox.connect(sbx.sandboxId), 'NotFoundError', 'connect after kill-while-frozen')
})
