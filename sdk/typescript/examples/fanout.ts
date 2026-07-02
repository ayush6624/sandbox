/**
 * Snapshots + fan-out: prepare state ONCE (files on disk, a process running
 * in memory), snapshot it, then clone it N ways in seconds. Every clone
 * resumes exactly where the snapshot was taken — the background process is
 * still running — but each has its own network identity and copy-on-write
 * disk, so their writes never bleed into each other.
 *
 * The classic use: `pnpm install` / warm a dev server once, then hand an
 * identical live environment to N test shards or N agents.
 *
 * NOTE: point SANDBOX_API_URL at a HOST (e.g. http://host:8080), not a
 * gateway — /snapshots/* routes are host-local (see docs/http-api.md).
 *
 * Run with: npm run example:fanout
 */
import { NotFoundError } from '../src/errors.js'
import { Sandbox } from '../src/index.js'
import { ensureCreds, runExample, step } from './shared.js'

const CLONES = 4
const TTL_MS = 300_000 // safety net: everything auto-destroys in 5 min

async function main(): Promise<void> {
  ensureCreds()

  // --- Prepare state once ------------------------------------------------
  step('Creating the source sandbox and preparing state...')
  const source = await Sandbox.create({ timeoutMs: TTL_MS })

  // Disk state: a "dataset" and a worker script that sums one shard of it.
  await source.files.write(
    '/home/sandbox/app/dataset.json',
    JSON.stringify(Array.from({ length: 1000 }, (_, i) => i + 1))
  )
  await source.files.write(
    '/home/sandbox/app/worker.js',
    `
    const data = require('/home/sandbox/app/dataset.json')
    const [shard, of] = process.argv.slice(2).map(Number)
    const mine = data.filter((_, i) => i % of === shard)
    const sum = mine.reduce((a, b) => a + b, 0)
    console.log(JSON.stringify({ shard, count: mine.length, sum }))
    `
  )

  // Memory state: a live process ticking a counter. Snapshots capture memory,
  // so every clone resumes with this process RUNNING mid-loop.
  await source.commands.run(
    `nohup node -e 'let n = 0; setInterval(() => require("fs").writeFileSync("/tmp/ticks", String(++n)), 100)' >/dev/null 2>&1 &
     until [ -f /tmp/ticks ]; do sleep 0.1; done`
  )
  const ticksAtSnapshot = Number(await source.files.read('/tmp/ticks'))
  console.log(`  dataset + worker written; background counter at tick ${ticksAtSnapshot}`)

  // --- Snapshot and release the source ------------------------------------
  step('Snapshotting (pauses ~1s, then the source keeps running)...')
  const snap = await source.snapshot()
  console.log(`  snapshot ${snap.snapshotId.slice(0, 8)} of ${snap.sourceId.slice(0, 8)}`)

  step('Killing the source (a snapshot outlives its sandbox)...')
  await source.kill()

  let clones: Sandbox[] = []
  try {
    // --- Fan out -----------------------------------------------------------
    step(`Fanning out ${CLONES} clones...`)
    const t0 = performance.now()
    clones = await Sandbox.fanout(snap.snapshotId, CLONES, { timeoutMs: TTL_MS })
    console.log(
      `  ${clones.length}/${CLONES} live in ${Math.round(performance.now() - t0)} ms: ` +
        clones.map((c) => c.sandboxId.slice(0, 8)).join(', ')
    )

    // --- Shared state: every clone has the prepared disk AND the live process
    step('Each clone works its own shard of the shared dataset:')
    const results = await Promise.all(
      clones.map((c, i) => c.commands.run(`node /home/sandbox/app/worker.js ${i} ${CLONES}`))
    )
    let total = 0
    results.forEach((r, i) => {
      const out = JSON.parse(r.stdout)
      total += out.sum
      console.log(`  clone ${i}: shard sum = ${out.sum} (${out.count} items)`)
    })
    console.log(`  shards add up: ${total} === 500500 → ${total === 500500}`)

    step('The in-memory process survived cloning (counter kept ticking):')
    for (const [i, c] of clones.entries()) {
      const ticks = Number(await c.files.read('/tmp/ticks'))
      console.log(`  clone ${i}: tick ${ticks} (was ${ticksAtSnapshot} at snapshot) → alive: ${ticks > ticksAtSnapshot}`)
    }

    // --- Isolated writes -----------------------------------------------------
    step('Writes are isolated (copy-on-write disks):')
    const [first, second] = clones
    if (!first || !second) throw new Error(`need at least 2 clones, got ${clones.length}`)
    await first.files.write('/home/sandbox/app/only-in-clone-0.txt', 'hi')
    try {
      await second.files.read('/home/sandbox/app/only-in-clone-0.txt')
      console.log('  ERROR: clone 1 saw clone 0’s write!')
    } catch (err) {
      if (!(err instanceof NotFoundError)) throw err
      console.log('  clone 0 wrote a file; clone 1 correctly cannot see it')
    }
  } finally {
    // --- Cleanup -------------------------------------------------------------
    step('Killing clones and deleting the snapshot...')
    await Promise.allSettled(clones.map((c) => c.kill()))
    await Sandbox.deleteSnapshot(snap.snapshotId)
  }
}

runExample(main)
