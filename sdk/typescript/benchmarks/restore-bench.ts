/**
 * Snapshot-restore latency benchmark.
 *
 * Measures how long it takes to get a *usable* sandbox two ways:
 *   1. cold boot   — `Sandbox.create()` (rootfs copy → kernel boot → init →
 *                     sandboxd startup → agent ready)
 *   2. restore     — `Sandbox.restore(snapshotId)` (rootfs copy → load memory +
 *                     device state → resume → agent ready)
 *
 * Both calls block until the in-guest agent answers, so the measured wall time
 * is "time until you can exec/write in the box" — the number that matters to an
 * SDK user. Restore skips kernel boot, init, and agent startup, which is where
 * the speedup comes from.
 *
 * The harness first creates one source sandbox, snapshots it, and kills it
 * (freeing the guest IP + tap the snapshot baked in, so restores can reuse
 * them). It then times N cold boots and N restores and prints a comparison.
 *
 * Usage:
 *   SANDBOX_API_URL=http://<host>:8080 SANDBOX_API_KEY=<key> \
 *     tsx benchmarks/restore-bench.ts [--iterations N] [--output file.json] [--keep-snapshot]
 *
 *   npm run bench:restore -- --iterations 10
 *
 * Point SANDBOX_API_URL at a single host's TCP API (not the gateway): snapshots
 * are host-local, and restore must land on the host that owns the snapshot.
 */
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { mkdirSync, writeFileSync } from 'node:fs'
import { Sandbox } from '../src/index.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const RESULTS_DIR = join(HERE, 'results')

interface Args {
  iterations: number
  output?: string
  keepSnapshot: boolean
}

function parseArgs(argv: string[]): Args {
  const args: Args = { iterations: 10, keepSnapshot: false }
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i]
    if (a === '--iterations') {
      const v = Number(argv[++i])
      if (!Number.isInteger(v) || v < 1) throw new Error('--iterations must be a positive integer')
      args.iterations = v
    } else if (a === '--output') {
      args.output = argv[++i]
    } else if (a === '--keep-snapshot') {
      args.keepSnapshot = true
    } else if (a === '--help' || a === '-h') {
      console.log(
        'Usage: tsx benchmarks/restore-bench.ts [--iterations N] [--output file.json] [--keep-snapshot]'
      )
      process.exit(0)
    } else {
      throw new Error(`unknown argument: ${a}`)
    }
  }
  return args
}

interface Stats {
  mean: number
  p50: number
  p90: number
  min: number
  max: number
  samples: number[]
}

function stats(samples: number[]): Stats {
  const sorted = [...samples].sort((a, b) => a - b)
  const pct = (p: number) => sorted[Math.min(sorted.length - 1, Math.floor((p / 100) * sorted.length))]!
  const mean = samples.reduce((s, v) => s + v, 0) / samples.length
  return {
    mean,
    p50: pct(50),
    p90: pct(90),
    min: sorted[0]!,
    max: sorted[sorted.length - 1]!,
    samples,
  }
}

const ms = (n: number) => `${Math.round(n)}ms`

/** Confirms a sandbox is actually usable by running a trivial command. */
async function verify(sbx: Sandbox): Promise<void> {
  const r = await sbx.commands.run('echo ok', { timeoutMs: 15_000 })
  if (r.stdout.trim() !== 'ok') throw new Error(`verify failed: got ${JSON.stringify(r.stdout)}`)
}

/** Times one cold boot: create → ready, returns ms. Kills the sandbox after. */
async function timeColdBoot(verifyFirst: boolean): Promise<number> {
  const start = Date.now()
  const sbx = await Sandbox.create({ timeoutMs: 5 * 60_000 })
  const elapsed = Date.now() - start
  try {
    if (verifyFirst) await verify(sbx)
  } finally {
    await sbx.kill().catch(() => {})
  }
  return elapsed
}

/** Times one restore: restore → ready, returns ms. Kills the sandbox after. */
async function timeRestore(snapshotId: string, verifyFirst: boolean): Promise<number> {
  const start = Date.now()
  const sbx = await Sandbox.restore(snapshotId, { timeoutMs: 5 * 60_000 })
  const elapsed = Date.now() - start
  try {
    if (verifyFirst) await verify(sbx)
  } finally {
    await sbx.kill().catch(() => {})
  }
  return elapsed
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2))

  console.log('='.repeat(64))
  console.log('  SNAPSHOT RESTORE BENCHMARK')
  console.log('='.repeat(64))
  console.log(`  iterations: ${args.iterations} per mode\n`)

  // --- Setup: create a source sandbox, snapshot it, then kill it. ---
  console.log('  [setup] creating source sandbox (cold boot)...')
  const setupStart = Date.now()
  const source = await Sandbox.create({ timeoutMs: 10 * 60_000 })
  console.log(`  [setup] source ${source.sandboxId} ready in ${ms(Date.now() - setupStart)}`)

  console.log('  [setup] taking snapshot...')
  const snapStart = Date.now()
  const snap = await source.snapshot()
  console.log(`  [setup] snapshot ${snap.snapshotId} captured in ${ms(Date.now() - snapStart)}`)

  // Free the snapshot's guest IP + tap so restores can reuse them.
  console.log('  [setup] killing source so restores can reuse its IP/tap...')
  await source.kill()
  // Brief settle for tap teardown + DNAT removal.
  await new Promise((r) => setTimeout(r, 1000))

  let coldStats: Stats | undefined
  let restoreStats: Stats | undefined
  try {
    // --- Cold boot runs ---
    console.log(`\n  Cold boot x${args.iterations}:`)
    const cold: number[] = []
    for (let i = 0; i < args.iterations; i++) {
      const t = await timeColdBoot(i === 0)
      cold.push(t)
      process.stdout.write(`    ${String(i + 1).padStart(2)}. ${ms(t).padStart(7)}\n`)
    }
    coldStats = stats(cold)

    // --- Restore runs ---
    console.log(`\n  Restore x${args.iterations}:`)
    const restore: number[] = []
    for (let i = 0; i < args.iterations; i++) {
      const t = await timeRestore(snap.snapshotId, i === 0)
      restore.push(t)
      process.stdout.write(`    ${String(i + 1).padStart(2)}. ${ms(t).padStart(7)}\n`)
    }
    restoreStats = stats(restore)
  } finally {
    if (!args.keepSnapshot) {
      console.log('\n  [cleanup] deleting snapshot...')
      await Sandbox.deleteSnapshot(snap.snapshotId).catch((e) =>
        console.error(`  [cleanup] deleteSnapshot failed: ${e}`)
      )
    } else {
      console.log(`\n  [cleanup] keeping snapshot ${snap.snapshotId} (--keep-snapshot)`)
    }
  }

  // --- Report ---
  console.log(`\n${'='.repeat(64)}`)
  console.log('  RESULTS')
  console.log('='.repeat(64))
  const row = (label: string, s: Stats) =>
    `  ${label.padEnd(12)} ${ms(s.mean).padStart(9)} ${ms(s.p50).padStart(9)} ${ms(s.p90).padStart(9)} ${ms(s.min).padStart(9)} ${ms(s.max).padStart(9)}`
  console.log(
    `  ${'mode'.padEnd(12)} ${'mean'.padStart(9)} ${'p50'.padStart(9)} ${'p90'.padStart(9)} ${'min'.padStart(9)} ${'max'.padStart(9)}`
  )
  console.log(`  ${'-'.repeat(12)} ${'-'.repeat(9)} ${'-'.repeat(9)} ${'-'.repeat(9)} ${'-'.repeat(9)} ${'-'.repeat(9)}`)
  console.log(row('cold boot', coldStats!))
  console.log(row('restore', restoreStats!))

  const speedup = coldStats!.p50 / restoreStats!.p50
  console.log(`\n  Restore is ${speedup.toFixed(2)}x faster than cold boot (p50: ${ms(coldStats!.p50)} → ${ms(restoreStats!.p50)})`)

  mkdirSync(RESULTS_DIR, { recursive: true })
  const ts = new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '').replace('T', '_')
  const outPath = args.output ?? join(RESULTS_DIR, `restore_${ts}.json`)
  writeFileSync(
    outPath,
    JSON.stringify(
      {
        iterations: args.iterations,
        snapshot_id: snap.snapshotId,
        cold_boot: coldStats,
        restore: restoreStats,
        speedup_p50: speedup,
      },
      null,
      2
    )
  )
  console.log(`\n  Results saved to ${outPath}`)
}

main().catch((err) => {
  console.error(err instanceof Error ? (err.stack ?? err.message) : err)
  process.exit(1)
})
