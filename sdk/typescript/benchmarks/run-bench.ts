/**
 * sandbox benchmark orchestrator (host side).
 *
 * Our own version of tensorlakeai/sandbox-sqlite-bench's `run_benchmarks.py`,
 * driving a sandbox microVM through this SDK. Unlike the upstream runner
 * (which shells out to each provider's CLI and runs a Python workload), this is
 * pure TypeScript end to end: it ships `benchmark.ts` into the guest and runs it
 * on the guest's own Node 22 via type-stripping — no Python, no extra installs.
 *
 * Lifecycle per run: create → detect specs → copy `benchmark.ts` into the guest
 * → `node benchmark.ts` → parse the JSON it prints → tear the sandbox down.
 *
 * Usage:
 *   SANDBOX_API_URL=http://<host>:8080 SANDBOX_API_KEY=<key> \
 *     tsx benchmarks/run-bench.ts [--mode default|fsync|large] [--iterations N] [--output file.json]
 *
 *   npm run bench -- --mode large --iterations 3
 */
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { readFileSync, mkdirSync, writeFileSync } from 'node:fs'
import { Sandbox } from '../src/index.js'

const HERE = dirname(fileURLToPath(import.meta.url))
const WORKLOAD_SCRIPT = join(HERE, 'benchmark.ts')
const RESULTS_DIR = join(HERE, 'results')
const GUEST_SCRIPT_PATH = '/tmp/benchmark.ts'

type Mode = 'default' | 'fsync' | 'large'

interface Args {
  mode: Mode
  iterations: number
  output?: string
}

/** Per-run record, shaped like one entry of the upstream `results/*.json`. */
interface ProviderResult {
  provider: string
  sandbox_id: string
  specs: Record<string, unknown>
  sandbox_creation_time: number
  mode: Mode
  iterations: number
  results: Record<string, unknown>
  wall_time: number
}

function parseArgs(argv: string[]): Args {
  const args: Args = { mode: 'default', iterations: 3 }
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i]
    if (a === '--mode') {
      const v = argv[++i]
      if (v !== 'default' && v !== 'fsync' && v !== 'large') {
        throw new Error(`--mode must be one of default|fsync|large, got: ${v}`)
      }
      args.mode = v
    } else if (a === '--iterations') {
      const v = Number(argv[++i])
      if (!Number.isInteger(v) || v < 1) throw new Error(`--iterations must be a positive integer`)
      args.iterations = v
    } else if (a === '--output') {
      args.output = argv[++i]
    } else if (a === '--help' || a === '-h') {
      console.log(
        'Usage: tsx benchmarks/run-bench.ts [--mode default|fsync|large] [--iterations N] [--output file.json]'
      )
      process.exit(0)
    } else {
      throw new Error(`unknown argument: ${a}`)
    }
  }
  return args
}

/**
 * Detects the guest's actual resources, the same way the upstream
 * `detect_specs()` does — nproc, cgroup cpu.max, MemTotal — but reports the Node
 * version (this workload runs on Node, not Python).
 */
async function detectSpecs(sbx: Sandbox): Promise<Record<string, unknown>> {
  const specs: Record<string, unknown> = {}

  const tryRun = async (cmd: string): Promise<string | undefined> => {
    try {
      const r = await sbx.commands.run(cmd, { timeoutMs: 30_000 })
      return r.stdout.trim().split('\n').at(-1)?.trim()
    } catch {
      return undefined
    }
  }

  const nproc = await tryRun('nproc')
  specs.actual_cpus = nproc ? Number(nproc) : 'unknown'

  const cgroup = await tryRun('cat /sys/fs/cgroup/cpu.max')
  if (cgroup) {
    const parts = cgroup.split(/\s+/)
    if (parts.length === 2 && parts[0] !== 'max') {
      specs.cgroup_cpus = Math.round((Number(parts[0]) / Number(parts[1])) * 10) / 10
    }
  }

  const meminfo = await tryRun('grep MemTotal /proc/meminfo')
  const kb = meminfo?.match(/(\d+)/)?.[1]
  specs.actual_memory_mb = kb ? Math.round(Number(kb) / 1024) : 'unknown'

  const nodever = await tryRun('node --version')
  specs.node_version = nodever ?? 'unknown'

  return specs
}

/** Extracts the JSON object the benchmark prints after the `--- JSON ---` marker. */
function parseBenchmarkJson(output: string): Record<string, unknown> {
  const idx = output.indexOf('--- JSON ---')
  if (idx === -1) return { error: 'Could not parse JSON from output' }
  const tail = output.slice(idx + '--- JSON ---'.length).trim()
  try {
    return JSON.parse(tail) as Record<string, unknown>
  } catch (e) {
    return { error: `Could not parse JSON from output: ${e instanceof Error ? e.message : String(e)}` }
  }
}

async function runBenchmark(args: Args): Promise<ProviderResult> {
  console.log(`\n${'='.repeat(60)}`)
  console.log('  SANDBOX')
  console.log('='.repeat(60))

  const createStart = Date.now()
  console.log('  Creating sandbox microVM...')
  // Generous TTL so a long --mode large run can't be reaped mid-benchmark;
  // killed explicitly in the finally block regardless.
  const sbx = await Sandbox.create({ timeoutMs: 30 * 60_000 })
  const createTime = Math.round((Date.now() - createStart) / 10) / 100
  console.log(`  Sandbox ID: ${sbx.sandboxId}`)
  console.log(`  Creation time: ${createTime}s`)

  try {
    console.log('  Detecting specs...')
    const specs = await detectSpecs(sbx)
    console.log(`  Specs: ${JSON.stringify(specs, null, 4)}`)

    console.log('  Copying benchmark workload...')
    const script = readFileSync(WORKLOAD_SCRIPT, 'utf8')
    await sbx.files.write(GUEST_SCRIPT_PATH, script)

    // Node 22 runs the .ts directly via type-stripping; --no-warnings hides the
    // node:sqlite ExperimentalWarning so it never pollutes stderr.
    const benchCmd = `node --no-warnings ${GUEST_SCRIPT_PATH} --mode ${args.mode} --iterations ${args.iterations}`
    console.log(`  Running: ${benchCmd}`)
    const start = Date.now()
    const res = await sbx.commands.run(benchCmd, { timeoutMs: 600_000 })
    const wallTime = Math.round((Date.now() - start) / 10) / 100
    process.stdout.write(res.stdout)
    if (res.stderr.trim()) process.stderr.write(res.stderr)

    const benchResults = parseBenchmarkJson(res.stdout)

    return {
      provider: 'sandbox',
      sandbox_id: sbx.sandboxId,
      specs,
      sandbox_creation_time: createTime,
      mode: args.mode,
      iterations: args.iterations,
      results: benchResults,
      wall_time: wallTime,
    }
  } finally {
    console.log('  Cleaning up sandbox sandbox...')
    try {
      await sbx.kill()
    } catch {
      // best-effort cleanup
    }
  }
}

/**
 * Pulls a numeric metric out of a results object, handling both the
 * single-iteration shape (`number`) and the multi-iteration summary shape
 * (`{ mean, stddev, min, max }`). Mirrors upstream `get_result_value`.
 */
function getResultValue(results: Record<string, unknown>, key: string): number | undefined {
  const val = results[key]
  if (typeof val === 'number') return val
  if (val && typeof val === 'object' && 'mean' in val) {
    const mean = (val as { mean: unknown }).mean
    return typeof mean === 'number' ? mean : undefined
  }
  return undefined
}

const BENCHMARK_KEYS = [
  'sequential_inserts',
  'batch_inserts',
  'select_count',
  'range_queries',
  'like_queries',
  'updates',
  'deletes',
  'transaction_inserts',
  'aggregates',
  'join_query',
  'concurrent_reads_wall',
  'fs_write_many',
  'fs_read_many',
  'fs_large_write',
  'fs_large_read',
  'total_time',
]

/** Prints the per-metric table and ranking, mirroring upstream `print_comparison`. */
function printComparison(all: ProviderResult[]): void {
  console.log(`\n${'='.repeat(80)}`)
  console.log('  COMPARISON')
  console.log(`${'='.repeat(80)}\n`)

  console.log('Resource Configuration:')
  console.log(
    `  ${'Provider'.padEnd(15)} ${'vCPUs'.padStart(8)} ${'Memory (MB)'.padStart(12)} ${'Node'.padStart(12)} ${'Create (s)'.padStart(12)}`
  )
  console.log(`  ${'-'.repeat(15)} ${'-'.repeat(8)} ${'-'.repeat(12)} ${'-'.repeat(12)} ${'-'.repeat(12)}`)
  for (const r of all) {
    const cpus = String(r.specs.cgroup_cpus ?? r.specs.actual_cpus ?? '?')
    const mem = String(r.specs.actual_memory_mb ?? '?')
    const node = String(r.specs.node_version ?? '?')
    const create = String(r.sandbox_creation_time ?? '?')
    console.log(`  ${r.provider.padEnd(15)} ${cpus.padStart(8)} ${mem.padStart(12)} ${node.padStart(12)} ${create.padStart(12)}`)
  }
  console.log()

  let header = `  ${'Benchmark'.padEnd(28)}`
  for (const r of all) header += ` ${r.provider.padStart(14)}`
  console.log(header)
  console.log(`  ${'-'.repeat(28)}` + ` ${'-'.repeat(14)}`.repeat(all.length))

  for (const bench of BENCHMARK_KEYS) {
    let row = `  ${bench.padEnd(28)}`
    for (const r of all) {
      const val = getResultValue(r.results, bench)
      row += val !== undefined ? ` ${(val.toFixed(4) + 's').padStart(14)}` : ` ${'n/a'.padStart(14)}`
    }
    console.log(row)
  }

  const ranked = all
    .filter((r) => getResultValue(r.results, 'total_time') !== undefined)
    .sort((a, b) => getResultValue(a.results, 'total_time')! - getResultValue(b.results, 'total_time')!)
  if (ranked.length) {
    console.log('\nRanking (fastest to slowest):')
    const baseline = getResultValue(ranked[0]!.results, 'total_time')!
    ranked.forEach((r, i) => {
      const t = getResultValue(r.results, 'total_time')!
      const ratio = t / baseline
      const bar = '#'.repeat(Math.floor((30 * baseline) / t))
      console.log(`  ${i + 1}. ${r.provider.padEnd(14)} ${t.toFixed(4)}s  (${ratio.toFixed(2)}x)  ${bar}`)
    })
  }
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2))

  const result = await runBenchmark(args)
  printComparison([result])

  mkdirSync(RESULTS_DIR, { recursive: true })
  const ts = new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '').replace('T', '_')
  const outPath = args.output ?? join(RESULTS_DIR, `${args.mode}_${ts}.json`)
  writeFileSync(outPath, JSON.stringify([result], null, 2))
  console.log(`\nResults saved to ${outPath}`)
}

main().catch((err) => {
  console.error(err instanceof Error ? (err.stack ?? err.message) : err)
  process.exit(1)
})
