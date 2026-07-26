/**
 * Snapshot-source create latency benchmark.
 *
 * Measures how long it takes to get a *usable* sandbox two ways:
 *   1. default source — `client.sandboxes.create()` (rootfs copy → kernel boot
 *                     → init → sandboxd startup → agent ready)
 *   2. snapshot source — `client.sandboxes.create({ source: { snapshotId } })`
 *                     (rootfs copy → load memory + device state → resume →
 *                     agent ready)
 *
 * Both calls block until the in-guest agent answers, so the measured wall time
 * is "time until you can exec/write in the box" — the number that matters to an
 * SDK user. A snapshot source skips kernel boot, init, and agent startup.
 *
 * The harness first creates one source sandbox, snapshots it, and terminates it.
 * It then times N default-source creates and N snapshot-source creates.
 *
 * Usage:
 *   SANDBOX_API_URL=http://<host>:8080 SANDBOX_API_KEY=<key> \
 *     tsx benchmarks/snapshot-source-bench.ts [--iterations N] [--output file.json] [--keep-snapshot]
 *
 *   npm run bench:snapshot-source -- --iterations 10
 *
 * Point SANDBOX_API_URL at a single host's TCP API (not the gateway): snapshots
 * are host-local, and snapshot-source creation must land on the owner host.
 */
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { mkdirSync, writeFileSync } from 'node:fs'
import { SandboxClient, type ClientSandbox } from '../src/index.js'
import {
  benchmarkMetadata,
  benchmarkResourceMetadata,
  type BenchmarkMetadata,
} from './metadata.js'

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
        'Usage: tsx benchmarks/snapshot-source-bench.ts [--iterations N] [--output file.json] [--keep-snapshot]'
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
async function verify(sbx: ClientSandbox): Promise<void> {
  const r = await sbx.commands.run('echo ok', { timeoutMs: 15_000 })
  if (r.stdout.trim() !== 'ok') throw new Error(`verify failed: got ${JSON.stringify(r.stdout)}`)
}

/** Times one default-source create through readiness, then terminates it. */
async function timeDefaultSourceCreate(
  client: SandboxClient,
  metadata: BenchmarkMetadata,
  verifyFirst: boolean,
): Promise<number> {
  const start = Date.now()
  const sbx = await client.sandboxes.create({
    requestTimeoutMs: 5 * 60_000,
    metadata: benchmarkResourceMetadata(metadata),
  })
  const elapsed = Date.now() - start
  try {
    if (verifyFirst) await verify(sbx)
  } finally {
    await sbx.terminate().catch(() => {})
  }
  return elapsed
}

/** Times one snapshot-source create through readiness, then terminates it. */
async function timeSnapshotSourceCreate(
  client: SandboxClient,
  metadata: BenchmarkMetadata,
  snapshotId: string,
  verifyFirst: boolean,
): Promise<number> {
  const start = Date.now()
  const sbx = await client.sandboxes.create({
    source: { snapshotId },
    requestTimeoutMs: 5 * 60_000,
    metadata: benchmarkResourceMetadata(metadata),
  })
  const elapsed = Date.now() - start
  try {
    if (verifyFirst) await verify(sbx)
  } finally {
    await sbx.terminate().catch(() => {})
  }
  return elapsed
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2))
  const client = new SandboxClient()
  const metadata = benchmarkMetadata('snapshot-source-create', {
    iterations: args.iterations,
    source_type: 'snapshot',
  })

  console.log('='.repeat(64))
  console.log('  SNAPSHOT-SOURCE CREATE BENCHMARK')
  console.log('='.repeat(64))
  console.log(`  iterations: ${args.iterations} per mode\n`)

  // --- Setup: create a source sandbox, snapshot it, then terminate it. ---
  console.log('  [setup] creating default-source sandbox...')
  const setupStart = Date.now()
  const source = await client.sandboxes.create({
    requestTimeoutMs: 10 * 60_000,
    metadata: benchmarkResourceMetadata(metadata),
  })
  console.log(`  [setup] source ${source.sandboxId} ready in ${ms(Date.now() - setupStart)}`)

  console.log('  [setup] taking snapshot...')
  const snapStart = Date.now()
  const snap = await source.createSnapshot()
  console.log(`  [setup] snapshot ${snap.id} captured in ${ms(Date.now() - snapStart)}`)

  console.log('  [setup] terminating source...')
  await source.terminate()
  // Brief settle for tap teardown + DNAT removal.
  await new Promise((r) => setTimeout(r, 1000))

  let coldStats: Stats | undefined
  let snapshotStats: Stats | undefined
  try {
    // --- Default-source runs ---
    console.log(`\n  Default source x${args.iterations}:`)
    const cold: number[] = []
    for (let i = 0; i < args.iterations; i++) {
      const t = await timeDefaultSourceCreate(client, metadata, i === 0)
      cold.push(t)
      process.stdout.write(`    ${String(i + 1).padStart(2)}. ${ms(t).padStart(7)}\n`)
    }
    coldStats = stats(cold)

    // --- Snapshot-source runs ---
    console.log(`\n  Snapshot source x${args.iterations}:`)
    const snapshot: number[] = []
    for (let i = 0; i < args.iterations; i++) {
      const t = await timeSnapshotSourceCreate(client, metadata, snap.id, i === 0)
      snapshot.push(t)
      process.stdout.write(`    ${String(i + 1).padStart(2)}. ${ms(t).padStart(7)}\n`)
    }
    snapshotStats = stats(snapshot)
  } finally {
    if (!args.keepSnapshot) {
      console.log('\n  [cleanup] deleting snapshot...')
      await client.snapshots.delete(snap.id).catch((e) =>
        console.error(`  [cleanup] snapshot delete failed: ${e}`)
      )
    } else {
      console.log(`\n  [cleanup] keeping snapshot ${snap.id} (--keep-snapshot)`)
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
  console.log(row('snapshot', snapshotStats!))

  const speedup = coldStats!.p50 / snapshotStats!.p50
  console.log(`\n  Snapshot-source create is ${speedup.toFixed(2)}x faster than default-source create (p50: ${ms(coldStats!.p50)} → ${ms(snapshotStats!.p50)})`)

  mkdirSync(RESULTS_DIR, { recursive: true })
  const ts = new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '').replace('T', '_')
  const outPath = args.output ?? join(RESULTS_DIR, `snapshot_source_${ts}.json`)
  writeFileSync(
    outPath,
    JSON.stringify(
      {
        metadata,
        iterations: args.iterations,
        snapshot_id: snap.id,
        default_source_create: coldStats,
        snapshot_source_create: snapshotStats,
        speedup_p50: speedup,
        // Deprecated compatibility keys preserve historic result consumers.
        cold_boot: coldStats,
        restore: snapshotStats,
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
