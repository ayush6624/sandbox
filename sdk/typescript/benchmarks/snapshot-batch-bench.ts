/**
 * Snapshot-source batch-create benchmark.
 *
 * Measures how fast the v1 batch operation creates N sandboxes from one
 * immutable snapshot source, versus N default-source creates.
 *
 * For each N it reports operation wall-clock and amortized per-sandbox time,
 * then — for the largest N — a default-source baseline
 * (N concurrent default-source creates) so the speedup is explicit.
 *
 * Usage:
 *   SANDBOX_API_URL=http://<host>:8080 SANDBOX_API_KEY=<key> \
 *     tsx benchmarks/snapshot-batch-bench.ts [--counts 1,4,16,32] [--baseline] [--output file.json]
 *
 * Point SANDBOX_API_URL at a single host's API: snapshots are host-local.
 */
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { mkdirSync, writeFileSync } from 'node:fs'
import { SandboxClient, type ClientSandbox } from '../src/index.js'
import { benchmarkMetadata, benchmarkResourceMetadata } from './metadata.js'

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
  const client = new SandboxClient()
  const metadata = benchmarkMetadata('snapshot-source-batch-create', {
    counts: args.counts,
    baseline: args.baseline,
  })
  console.log(`\nSnapshot batch-create benchmark: counts=[${args.counts.join(', ')}] baseline=${args.baseline}`)
  console.log(`Host: ${process.env.SANDBOX_API_URL}`)

  // --- Setup: one source sandbox -> snapshot -> terminate ---
  console.log('\n[setup] creating source sandbox...')
  const src = await client.sandboxes.create({
    requestTimeoutMs: 60 * 60_000,
    metadata: benchmarkResourceMetadata(metadata),
  })
  console.log(`[setup] source ${src.id} ready`)
  console.log('[setup] snapshotting...')
  const snap = await src.createSnapshot()
  console.log(`[setup] snapshot ${snap.id}`)
  await src.terminate()
  console.log('[setup] source terminated\n')

  const rows: Array<{
    n: number
    wallMs: number
    perSandboxMs: number
    ok: number
    failed: number
    operationId: string
    operationStatus: string
    /** @deprecated Historic alias for perSandboxMs. */
    perCloneMs: number
  }> = []
  try {
    for (const n of args.counts) {
      const t = Date.now()
      const operation = await client.sandboxes.createMany({
        count: n,
        maxParallelism: n,
        source: { snapshotId: snap.id },
        requestTimeoutMs: 30 * 60_000,
        metadata: benchmarkResourceMetadata(metadata),
      })
      const state = await operation.wait({ timeoutMs: 30 * 60_000 })
      const clones = state.results
        .map((result) => result.value)
        .filter((sandbox): sandbox is ClientSandbox => sandbox !== undefined)
      const wallMs = Date.now() - t
      // Confirm each clone is actually usable (exec a trivial command).
      const oks = await mapLimit(clones, 32, async (c) => {
        try { const r = await c.commands.run('echo ok'); return r.stdout.trim() === 'ok' } catch { return false }
      })
      const ok = oks.filter(Boolean).length
      rows.push({
        n,
        wallMs,
        perSandboxMs: wallMs / n,
        ok,
        failed: state.failed,
        operationId: operation.id,
        operationStatus: state.status,
        perCloneMs: wallMs / n,
      })
      console.log(`  N=${String(n).padStart(3)}  batch ${fmt(wallMs)}ms  per-sandbox ${fmt(wallMs / n)}ms  usable ${ok}/${n}`)
      await mapLimit(clones, 16, async (c) => { try { await c.terminate() } catch { /* best effort */ } })
    }

    // --- Baseline: N concurrent cold boots, for the largest N ---
    let baseline: {
      n: number
      wallMs: number
      perSandboxMs: number
      /** @deprecated Historic alias for perSandboxMs. */
      perCloneMs: number
    } | undefined
    if (args.baseline) {
      const n = Math.max(...args.counts)
      console.log(`\n[baseline] ${n} concurrent default-source creates...`)
      const t = Date.now()
      const boots = await mapLimit(Array.from({ length: n }), 8, async () => {
        try {
          return await client.sandboxes.create({
            requestTimeoutMs: 30 * 60_000,
            metadata: benchmarkResourceMetadata(metadata),
          })
        } catch {
          return null
        }
      })
      const wallMs = Date.now() - t
      baseline = { n, wallMs, perSandboxMs: wallMs / n, perCloneMs: wallMs / n }
      console.log(`  cold boot N=${n}  batch ${fmt(wallMs)}ms  per-boot ${fmt(wallMs / n)}ms`)
      await mapLimit(boots.filter(Boolean), 16, async (b) => { try { await b!.terminate() } catch { /* best effort */ } })
    }

    console.log(`\n${'='.repeat(56)}\n  SNAPSHOT BATCH-CREATE RESULTS\n${'='.repeat(56)}`)
    console.log('   N   batch(ms)  per-sandbox(ms)  usable  operation')
    for (const r of rows) {
      console.log(`  ${String(r.n).padStart(3)}   ${fmt(r.wallMs).padStart(8)}   ${fmt(r.perSandboxMs).padStart(12)}   ${r.ok}/${r.n}  ${r.operationStatus}`)
    }
    if (baseline) {
      console.log(`\n  default-source baseline N=${baseline.n}: ${fmt(baseline.wallMs)}ms batch, ${fmt(baseline.perSandboxMs)}ms/sandbox`)
      const biggest = rows.find((r) => r.n === baseline!.n)
      if (biggest) console.log(`  snapshot-source speedup at N=${baseline.n}: ${fmt(baseline.wallMs / biggest.wallMs)}x`)
    }

    mkdirSync(RESULTS_DIR, { recursive: true })
    const ts = new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '').replace('T', '_')
    const outPath = args.output ?? join(RESULTS_DIR, `snapshot_batch_${ts}.json`)
    writeFileSync(outPath, JSON.stringify({
      metadata,
      rows,
      baseline,
      // Deprecated compatibility alias for historic fan-out result readers.
      fanout: rows,
    }, null, 2))
    console.log(`\nSaved ${outPath}`)
  } finally {
    try { await client.snapshots.delete(snap.id) } catch { /* best effort */ }
  }
}

main().catch((e) => { console.error(e instanceof Error ? (e.stack ?? e.message) : e); process.exit(1) })
