import assert from 'node:assert/strict'
import { test } from 'node:test'
import { runArrivals } from './open-loop-arrivals.js'

test('arrivals continue before any task settles, including after a rejection', { timeout: 2000 }, async () => {
  let release = () => {}
  const gate = new Promise<void>(resolve => { release = resolve })
  let reachedLast = () => {}
  const allDispatched = new Promise<void>(resolve => { reachedLast = resolve })
  const seen: number[] = []
  const run = runArrivals({ count: 4, rate: 100, task: async ({ index, scheduledMs, dispatchedMs }) => {
    seen.push(index)
    assert.equal(scheduledMs, index * 10)
    assert.ok(dispatchedMs >= scheduledMs)
    if (index === 3) reachedLast()
    if (index === 1) throw new Error('deliberate task failure')
    await gate
    return index
  } })
  try {
    await allDispatched
    assert.deepEqual(seen, [0, 1, 2, 3])
  } finally { release() }
  const result = await run
  assert.deepEqual(result.map(row => row.status), ['fulfilled', 'rejected', 'fulfilled', 'fulfilled'])
})
