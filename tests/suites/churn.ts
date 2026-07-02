/**
 * Churn: rapid create→use→kill cycles, sequential and batched. The point is
 * leak detection — after heavy churn the fleet must return exactly to its
 * pre-suite sandbox count, proving taps/IPs/ports/rootfs rows are recycled.
 */

import { Sandbox } from '../../sdk/typescript/src/index.js'
import {
  SuiteDef,
  assertEq,
  envInt,
  eventually,
  pool,
  statLine,
  timed,
} from '../harness.js'

export const suite = new SuiteDef('churn')

const CYCLES = envInt('STRESS_CHURN_CYCLES', 8)
const BATCH_ROUNDS = envInt('STRESS_CHURN_ROUNDS', 3)
const BATCH_SIZE = envInt('STRESS_CHURN_BATCH', 6)

let baseline = 0

suite.test('record baseline sandbox count', async (ctx) => {
  baseline = (await Sandbox.list()).length
  ctx.log(`baseline running sandboxes: ${baseline}`)
})

suite.test(`${CYCLES} sequential create→exec→kill cycles`, async (ctx) => {
  const cycleTimes: number[] = []
  for (let i = 0; i < CYCLES; i++) {
    const { ms } = await timed(async () => {
      const sbx = await ctx.createTracked()
      const res = await sbx.commands.run(`echo cycle-${i}`)
      assertEq(res.stdout.trim(), `cycle-${i}`, `cycle #${i} exec`)
      await sbx.kill()
      ctx.untrack(sbx)
    })
    cycleTimes.push(ms)
  }
  ctx.log(statLine('full cycle', cycleTimes))
})

suite.test(`${BATCH_ROUNDS} rounds of ${BATCH_SIZE} parallel create→exec→kill`, async (ctx) => {
  for (let round = 0; round < BATCH_ROUNDS; round++) {
    const { ms } = await timed(() =>
      pool(BATCH_SIZE, Array.from({ length: BATCH_SIZE }, (_, i) => i), async (i) => {
        const sbx = await ctx.createTracked()
        const res = await sbx.commands.run(`echo round-${round}-sbx-${i}`)
        assertEq(res.stdout.trim(), `round-${round}-sbx-${i}`, `round ${round} sandbox ${i}`)
        await sbx.kill()
        ctx.untrack(sbx)
      })
    )
    ctx.log(`round ${round + 1}/${BATCH_ROUNDS}: ${Math.round(ms)}ms`)
  }
})

suite.test('immediate create-kill-create reuses cleanly', async (ctx) => {
  // Kill and immediately create again, repeatedly — stresses pool release
  // racing with pool allocation.
  for (let i = 0; i < 5; i++) {
    const sbx = await ctx.createTracked()
    await sbx.kill()
    ctx.untrack(sbx)
    const next = await ctx.createTracked()
    const res = await next.commands.run('echo reuse')
    assertEq(res.stdout.trim(), 'reuse', `reuse iteration ${i}`)
    await next.kill()
    ctx.untrack(next)
  }
})

suite.test('no leaked sandboxes: count returns to baseline', async (ctx) => {
  await eventually(
    async () => (await Sandbox.list()).length <= baseline,
    { timeoutMs: 15_000, what: `sandbox count to return to baseline (${baseline})` }
  )
  const final = (await Sandbox.list()).length
  ctx.log(`final running sandboxes: ${final}`)
  assertEq(final, baseline, 'churn must not leak sandboxes')
})
