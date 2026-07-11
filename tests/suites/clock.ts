/**
 * Guest wall-clock correctness across snapshot-resume paths. Firecracker
 * restore leaves the guest's CLOCK_REALTIME frozen at snapshot-creation time
 * (hours stale for golden-snapshot hot creates on a long-lived server), and
 * NTP is not a fallback (some deployments block outbound UDP). The host steps
 * the clock via MMDS epoch_ms at resume plus a deterministic POST /clock
 * after the readiness gate — these tests assert the result.
 */

import type { Sandbox } from '../../sdk/typescript/src/index.js'
import { SuiteDef, assert, assertEq, sleep } from '../harness.js'

export const suite = new SuiteDef('clock')

/** Allowed skew between guest and host, generous for exec round-trip time. */
const SKEW_S = 5

/** Reads the guest's wall clock and asserts it brackets the host's. */
async function assertClockFresh(sbx: Sandbox, what: string): Promise<void> {
  const before = Date.now() / 1000
  const res = await sbx.commands.run('date +%s')
  const after = Date.now() / 1000
  assertEq(res.exitCode, 0, `${what}: date must run`)
  const guest = Number.parseInt(res.stdout.trim(), 10)
  assert(Number.isInteger(guest), `${what}: unparseable guest time ${JSON.stringify(res.stdout)}`)
  assert(
    guest >= before - SKEW_S && guest <= after + SKEW_S,
    `${what}: guest clock is ${Math.round(before - guest)}s behind host ` +
      `(guest=${guest} host=[${Math.floor(before)}, ${Math.ceil(after)}])`
  )
}

suite.test('hot-created sandbox has the host wall clock', async (ctx) => {
  // Every default create clones the golden snapshot, which may have been
  // built hours ago — a missed clock step shows up as exactly that much lag.
  const sbx = await ctx.createTracked()
  await assertClockFresh(sbx, 'hot create')
})

suite.test('hibernate + wake resteps the clock', async (ctx) => {
  const sbx = await ctx.createTracked()
  await assertClockFresh(sbx, 'before hibernate')

  await sbx.hibernate()
  assertEq(sbx.info.status, 'hibernated', 'hibernate must freeze the sandbox')
  // Let real time pass while frozen so a missing re-step is observable: a
  // woken guest without clock sync would read ~10s in the past.
  await sleep(10_000)

  // The exec transparently wakes it (same-identity restore or clone-path).
  await assertClockFresh(sbx, 'after wake')
})
