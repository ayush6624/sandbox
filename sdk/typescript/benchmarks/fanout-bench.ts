/**
 * Snapshot fan-out benchmark.
 *
 * Measures how fast we can bring up N sandboxes from ONE snapshot via
 * Sandbox.fanout(), versus N independent cold boots. Fan-out gives each clone a
 * fresh IP/tap/port and its own reflink CoW rootfs, and the clone reidentifies
 * from MMDS on resume — so N clones of one snapshot run side by side.
 *
 * For each N it reports batch wall-clock (one fanout call returns all N) and the
 * amortized per-clone time, then — for the largest N — a cold-boot baseline
 * (N concurrent Sandbox.create) so the speedup is explicit.
 *
 * Usage:
 *   SANDBOX_API_URL=http://<host>:8080 SANDBOX_API_KEY=<key> \
 *     tsx benchmarks/fanout-bench.ts [--counts 1,4,16,32] [--baseline] [--output file.json]
 *
 * Point SANDBOX_API_URL at a single host's TCP API (NOT the gateway): fan-out is
 * host-local — a snapshot's artifacts live on the host that took it.
 */
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { mkdirSync, writeFileSync } from 'node:fs'
import { Sandbox } from '../src/index.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const RESULTS_DIR = join(HERE, 'results')

interface Args {
  counts: number[]
  baseline: boolean
  output?: string
}

function parseArgs(argv: string[]): Args {
  const a: Args = { counts: [1, 4, 16, 32], baseline: false }
  for (let i = 0; i < argv.length; i++) {
    const k = argv[i]
    if (k === '--counts') a.counts = argv[++i]!.split(',').map((x) => Number(x.trim()))
    else if (k === '--baseline') a.baseline = true
    else if (k === '--output') a.output = argv[++i]
    else throw new Error(`unknown arg: ${k}`)
  }
  if (a.counts.some((n) => !Number.isInteger(n) || n < 1)) throw new Error('--counts must be positive integers')
  return a
}

async function mapLimit<T, R>(items: T[], limit: number, fn: (item: T, idx: number) => Promise<R>): Promise<R[]> {
  const results = new Array<R>(items.length)
  let next = 0
  const workers = Array.from({ length: Math.min(limit, items.length) }, async () => {
    while (true) {
      const i = next++
      if (i >= items.length) return
      results[i] = await fn(items[i]!, i)
    }
  })
  await Promise.all(workers)
  return results
}

const fmt = (x: number) => (Number.isFinite(x) ? x.toFixed(1) : 'n/a')

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2))
  console.log(`\nFan-out benchmark: counts=[${args.counts.join(', ')}] baseline=${args.baseline}`)
  console.log(`Host: ${process.env.SANDBOX_API_URL}`)

  // --- Setup: one source sandbox -> snapshot -> kill ---
  console.log('\n[setup] creating source sandbox...')
  const src = await Sandbox.create({ timeoutMs: 60 * 60_000 })
  console.log(`[setup] source ${src.sandboxId} ready`)
  console.log('[setup] snapshotting...')
  const snap = await src.snapshot()
  console.log(`[setup] snapshot ${snap.snapshotId}`)
  await src.kill()
  console.log('[setup] source killed\n')

  const rows: Array<{ n: number; wallMs: number; perCloneMs: number; ok: number }> = []
  try {
    for (const n of args.counts) {
      const t = Date.now()
      const clones = await Sandbox.fanout(snap.snapshotId, n, { timeoutMs: 30 * 60_000 })
      const wallMs = Date.now() - t
      // Confirm each clone is actually usable (exec a trivial command).
      const oks = await mapLimit(clones, 32, async (c) => {
        try { const r = await c.commands.run('echo ok'); return r.stdout.trim() === 'ok' } catch { return false }
      })
      const ok = oks.filter(Boolean).length
      rows.push({ n, wallMs, perCloneMs: wallMs / n, ok })
      console.log(`  N=${String(n).padStart(3)}  batch ${fmt(wallMs)}ms  per-clone ${fmt(wallMs / n)}ms  usable ${ok}/${n}`)
      await mapLimit(clones, 16, async (c) => { try { await c.kill() } catch { /* best effort */ } })
    }

    // --- Baseline: N concurrent cold boots, for the largest N ---
    let baseline: { n: number; wallMs: number; perCloneMs: number } | undefined
    if (args.baseline) {
      const n = Math.max(...args.counts)
      console.log(`\n[baseline] ${n} concurrent cold boots (Sandbox.create)...`)
      const t = Date.now()
      const boots = await mapLimit(Array.from({ length: n }), 8, async () => {
        try { return await Sandbox.create({ timeoutMs: 30 * 60_000 }) } catch { return null }
      })
      const wallMs = Date.now() - t
      baseline = { n, wallMs, perCloneMs: wallMs / n }
      console.log(`  cold boot N=${n}  batch ${fmt(wallMs)}ms  per-boot ${fmt(wallMs / n)}ms`)
      await mapLimit(boots.filter(Boolean), 16, async (b) => { try { await b!.kill() } catch { /* best effort */ } })
    }

    console.log(`\n${'='.repeat(56)}\n  FAN-OUT RESULTS\n${'='.repeat(56)}`)
    console.log('   N   batch(ms)  per-clone(ms)  usable')
    for (const r of rows) {
      console.log(`  ${String(r.n).padStart(3)}   ${fmt(r.wallMs).padStart(8)}   ${fmt(r.perCloneMs).padStart(10)}   ${r.ok}/${r.n}`)
    }
    if (baseline) {
      console.log(`\n  cold-boot baseline N=${baseline.n}: ${fmt(baseline.wallMs)}ms batch, ${fmt(baseline.perCloneMs)}ms/boot`)
      const biggest = rows.find((r) => r.n === baseline!.n)
      if (biggest) console.log(`  fan-out speedup at N=${baseline.n}: ${fmt(baseline.wallMs / biggest.wallMs)}x`)
    }

    mkdirSync(RESULTS_DIR, { recursive: true })
    const ts = new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '').replace('T', '_')
    const outPath = args.output ?? join(RESULTS_DIR, `fanout_${ts}.json`)
    writeFileSync(outPath, JSON.stringify({ rows, baseline }, null, 2))
    console.log(`\nSaved ${outPath}`)
  } finally {
    try { await Sandbox.deleteSnapshot(snap.snapshotId) } catch { /* best effort */ }
  }
}

main().catch((e) => { console.error(e instanceof Error ? (e.stack ?? e.message) : e); process.exit(1) })
