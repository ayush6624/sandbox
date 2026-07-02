/**
 * Snapshot / restore / fan-out. These endpoints are host-local (the gateway
 * doesn't route them yet), so this suite talks to one host directly via
 * SANDBOX_HOST_URL / SANDBOX_HOST_KEY and is skipped when they're not set.
 *
 * What's verified:
 * - restore brings back disk state AND live memory state (a background
 *   counter process keeps counting after restore)
 * - fanout clones share the prepared state but have isolated writes and
 *   distinct identities
 * - snapshot housekeeping (list/delete)
 */

import { Sandbox } from '../../sdk/typescript/src/index.js'
import type { SandboxCreateOpts } from '../../sdk/typescript/src/index.js'
import {
  SkipSuite,
  SuiteDef,
  assert,
  assertEq,
  envInt,
  eventually,
  pool,
  sleep,
  statLine,
  timed,
} from '../harness.js'
import type { Ctx } from '../harness.js'

export const suite = new SuiteDef('snapshots')

const FANOUT_N = envInt('STRESS_FANOUT_N', 8)

function hostOpts(): SandboxCreateOpts {
  const apiUrl = process.env.SANDBOX_HOST_URL
  const apiKey = process.env.SANDBOX_HOST_KEY
  if (!apiUrl || !apiKey) {
    throw new SkipSuite('set SANDBOX_HOST_URL + SANDBOX_HOST_KEY (host-direct) to run snapshot tests')
  }
  return { apiUrl, apiKey }
}

/** Snapshots created by tests; the final test deletes any leftovers. */
const createdSnapshots: string[] = []

async function createOnHost(ctx: Ctx): Promise<Sandbox> {
  const sbx = await Sandbox.create(hostOpts())
  return ctx.track(sbx)
}

suite.test('snapshot → kill source → restore resumes disk AND memory state', async (ctx) => {
  const opts = hostOpts()
  const sbx = await createOnHost(ctx)

  // Disk state: a marker file. Memory state: a live counter process that
  // rewrites /tmp/count 5x/second — if restore truly resumes memory, the
  // counter keeps counting in the restored sandbox.
  await sbx.files.write('/home/sandbox/marker.txt', 'prepared-state')
  await sbx.commands.run(
    `nohup node -e 'let i=0;setInterval(()=>require("fs").writeFileSync("/tmp/count",String(++i)),200)' >/dev/null 2>&1 & echo ok`
  )
  await eventually(
    async () => Number((await sbx.files.read('/tmp/count')).trim()) > 0,
    { timeoutMs: 10_000, what: 'counter process to start before snapshot' }
  )

  const { result: snap, ms: snapMs } = await timed(() => sbx.snapshot())
  createdSnapshots.push(snap.snapshotId)
  assertEq(snap.sourceId, sbx.sandboxId, 'snapshot must record its source')
  ctx.log(`snapshot took ${Math.round(snapMs)}ms`)

  // The source keeps running after snapshot (pause+resume, not destroy).
  const alive = await sbx.commands.run('echo post-snapshot')
  assertEq(alive.stdout.trim(), 'post-snapshot', 'source must keep running after snapshot')

  // Restore requires the source to be gone (it reuses the baked identity).
  await sbx.kill()
  ctx.untrack(sbx)

  const { result: restored, ms: restoreMs } = await timed(() =>
    Sandbox.restore(snap.snapshotId, opts)
  )
  ctx.track(restored)
  ctx.log(`restore took ${Math.round(restoreMs)}ms`)
  assert(restored.sandboxId !== sbx.sandboxId, 'restore must mint a new sandbox id')

  // Disk state came back.
  assertEq(
    await restored.files.read('/home/sandbox/marker.txt'),
    'prepared-state',
    'marker file must survive restore'
  )

  // Memory state came back: the counter process is still counting.
  const c1 = Number((await restored.files.read('/tmp/count')).trim())
  await sleep(1_500)
  const c2 = Number((await restored.files.read('/tmp/count')).trim())
  assert(c1 > 0, `counter must exist after restore, got ${c1}`)
  assert(c2 > c1, `counter must still be counting after restore (${c1} → ${c2})`)

  await restored.kill()
  ctx.untrack(restored)
})

suite.test(`fanout ${FANOUT_N}: clones share prepared state, writes are isolated`, async (ctx) => {
  const opts = hostOpts()
  const sbx = await createOnHost(ctx)

  await sbx.files.write('/home/sandbox/shared.txt', 'from-the-source')
  const snap = await sbx.snapshot()
  createdSnapshots.push(snap.snapshotId)
  await sbx.kill()
  ctx.untrack(sbx)

  const { result: clones, ms } = await timed(() => Sandbox.fanout(snap.snapshotId, FANOUT_N, opts))
  clones.forEach((c) => ctx.track(c))
  assertEq(clones.length, FANOUT_N, 'every requested clone must come up')
  ctx.log(`fanout of ${FANOUT_N} took ${Math.round(ms)}ms (${Math.round(ms / FANOUT_N)}ms/clone)`)

  // Fresh identities: unique ids, IPs, and host ports.
  assertEq(new Set(clones.map((c) => c.sandboxId)).size, FANOUT_N, 'clone ids must be unique')
  assertEq(new Set(clones.map((c) => c.info.guestIp)).size, FANOUT_N, 'clone IPs must be unique')
  assertEq(new Set(clones.map((c) => c.info.hostPort)).size, FANOUT_N, 'clone ports must be unique')

  // All clones see the prepared state and are individually reachable.
  const execTimes: number[] = []
  await pool(FANOUT_N, clones, async (clone, i) => {
    const { result, ms: execMs } = await timed(() => clone.files.read('/home/sandbox/shared.txt'))
    execTimes.push(execMs)
    assertEq(result, 'from-the-source', `clone #${i} must see the snapshot's file`)
  })
  ctx.log(statLine('clone first read', execTimes))

  // Isolation: each clone writes its own file; no clone sees another's write.
  await pool(FANOUT_N, clones, async (clone, i) => {
    await clone.files.write(`/home/sandbox/only-${i}.txt`, `clone-${i}`)
  })
  await pool(FANOUT_N, clones, async (clone, i) => {
    assertEq(
      await clone.files.read(`/home/sandbox/only-${i}.txt`),
      `clone-${i}`,
      `clone #${i} must see its own write`
    )
    const neighbor = (i + 1) % FANOUT_N
    let sawNeighbor = true
    try {
      await clone.files.read(`/home/sandbox/only-${neighbor}.txt`)
    } catch {
      sawNeighbor = false
    }
    assert(!sawNeighbor, `clone #${i} must NOT see clone #${neighbor}'s write`)
  })

  await pool(FANOUT_N, clones, async (clone) => {
    await clone.kill()
    ctx.untrack(clone)
  })
})

suite.test('snapshot housekeeping: list contains ours, delete removes it', async (ctx) => {
  const opts = hostOpts()
  const sbx = await createOnHost(ctx)
  const snap = await sbx.snapshot()
  await sbx.kill()
  ctx.untrack(sbx)

  const listed = await Sandbox.listSnapshots(opts)
  assert(
    listed.some((s) => s.snapshotId === snap.snapshotId),
    'fresh snapshot must appear in listSnapshots'
  )

  await Sandbox.deleteSnapshot(snap.snapshotId, opts)
  const after = await Sandbox.listSnapshots(opts)
  assert(
    after.every((s) => s.snapshotId !== snap.snapshotId),
    'deleted snapshot must disappear from listSnapshots'
  )
})

suite.test('cleanup: delete snapshots created by this suite', async (ctx) => {
  const opts = hostOpts()
  let deleted = 0
  for (const id of createdSnapshots) {
    try {
      await Sandbox.deleteSnapshot(id, opts)
      deleted++
    } catch {
      // Already deleted by its own test — fine.
    }
  }
  ctx.log(`deleted ${deleted} leftover snapshot(s)`)
})
