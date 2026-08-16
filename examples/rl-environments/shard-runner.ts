/**
 * Shard an agent benchmark across N sandboxes cloned from one prepared snapshot.
 *
 * This is the cheap half of the plan: benchmarks whose environment is a Python
 * process rather than a machine (τ³-bench, Zapier's AutomationBench) don't
 * *need* a microVM for isolation, but they do need hundreds of independent,
 * reproducible, parallel workers — and one prepared snapshot fanned out N ways
 * gives that in ~764 ms per sandbox instead of a per-worker `pip install`.
 *
 * The pattern:
 *   1. create one sandbox, install the benchmark, snapshot it  (once, minutes)
 *   2. clone the snapshot N ways                               (~764 ms each)
 *   3. run shard i in clone i, streaming output to a local log
 *   4. pull each shard's results JSON back and merge
 *   5. destroy everything, then read the billing ledger for what it cost
 *
 * Usage (from the control VM — never a laptop tunnel; transport RTT reads as
 * sandbox latency):
 *
 *   export SANDBOX_API_URL=http://10.160.0.100:9090
 *   export SANDBOX_API_KEY=...
 *   export ANTHROPIC_API_KEY=...          # forwarded into each shard
 *   npx tsx examples/rl-environments/shard-runner.ts tau3 --shards 8 --limit 32
 *
 * Add `--dry-run` to run the whole harness with a scripted no-LLM policy: it
 * exercises create/fanout/exec/collect/teardown and spends zero tokens.
 */

import { mkdir, writeFile } from 'node:fs/promises'
import path from 'node:path'

import { SandboxClient, CapacityError } from '../../sdk/typescript/src/index.js'
import type { ClientSandbox } from '../../sdk/typescript/src/index.js'

// ---------------------------------------------------------------------------
// Benchmark definitions
//
// `prepare` runs ONCE in the sandbox that becomes the snapshot. `shardCommand`
// runs in each clone and must write JSON to `resultPath`.

interface Benchmark {
  readonly name: string
  /** Setup script. Everything expensive belongs here — it is paid once. */
  readonly prepare: string
  /** Per-shard command. Receives shard index/count and the model to drive. */
  shardCommand(shard: number, shards: number, options: RunOptions): string
  readonly resultPath: string
  /** Env forwarded into the guest. Keys absent from the host are skipped. */
  readonly forwardEnv: readonly string[]
}

const WORKDIR = '/home/sandbox/app'
const BENCH_RESULT = `${WORKDIR}/bench/results.json`

const TAU3: Benchmark = {
  name: 'tau3-bench',
  prepare: `
set -euo pipefail
cd ${WORKDIR}
git clone --depth 1 https://github.com/sierra-research/tau2-bench.git bench
cd bench
python3 -m venv .venv
.venv/bin/pip install --quiet --upgrade pip
.venv/bin/pip install --quiet -e .
# Prove the install before it becomes a snapshot: a broken snapshot fans out
# into N broken shards, and the failure surfaces N times, much later.
.venv/bin/python -c "import tau2; print('tau2 ok')"
`,
  shardCommand: (shard, shards, options) => `
set -euo pipefail
cd ${WORKDIR}/bench
.venv/bin/python -m tau2 run \\
  --domain ${options.domain ?? 'retail'} \\
  --agent-llm ${options.model} \\
  --user-llm ${options.model} \\
  --num-trials 1 \\
  --shard ${shard} --num-shards ${shards} \\
  ${options.limit ? `--max-tasks ${options.limit}` : ''} \\
  --output ${BENCH_RESULT}
`,
  resultPath: BENCH_RESULT,
  forwardEnv: ['ANTHROPIC_API_KEY', 'OPENAI_API_KEY'],
}

const AUTOMATION_BENCH: Benchmark = {
  name: 'automationbench',
  prepare: `
set -euo pipefail
cd ${WORKDIR}
git clone --depth 1 https://github.com/zapier/AutomationBench.git bench
cd bench
# uv is the project's own toolchain; installing it here keeps the snapshot
# self-contained so no clone needs the network at run time.
curl -LsSf https://astral.sh/uv/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
uv sync --quiet
uv run auto-bench --help > /dev/null
`,
  shardCommand: (shard, shards, options) => `
set -euo pipefail
cd ${WORKDIR}/bench
export PATH="$HOME/.local/bin:$PATH"
uv run auto-bench \\
  --model ${options.model} \\
  --shard-index ${shard} --shard-count ${shards} \\
  ${options.limit ? `--max-tasks ${options.limit}` : ''} \\
  --output results.json
`,
  resultPath: BENCH_RESULT,
  forwardEnv: ['ANTHROPIC_API_KEY', 'OPENAI_API_KEY'],
}

/** Zero-token control: exercises the harness without calling any model. */
const DRY_RUN: Benchmark = {
  name: 'dry-run',
  prepare: `set -euo pipefail; mkdir -p ${WORKDIR}/bench; python3 --version; node --version`,
  shardCommand: (shard, shards) => `
set -euo pipefail
cd ${WORKDIR}/bench
python3 - <<'PY'
import json, os, platform, socket, time
start = time.time()
# Stand in for a rollout: touch the filesystem, burn a little CPU, report.
open("scratch.txt", "w").write("x" * 4096)
total = sum(i * i for i in range(2_000_000))
json.dump({
  "shard": ${shard}, "shards": ${shards}, "host": socket.gethostname(),
  "kernel": platform.release(), "checksum": total,
  "duration_s": round(time.time() - start, 3),
  "tasks": [{"id": f"synthetic-{${shard}}-{i}", "reward": 1.0} for i in range(4)],
}, open("results.json", "w"))
PY
`,
  resultPath: BENCH_RESULT,
  forwardEnv: [],
}

const BENCHMARKS: Record<string, Benchmark> = {
  tau3: TAU3,
  automationbench: AUTOMATION_BENCH,
  'dry-run': DRY_RUN,
}

// ---------------------------------------------------------------------------

interface RunOptions {
  benchmark: Benchmark
  shards: number
  model: string
  limit?: number
  domain?: string
  outDir: string
  /** Reuse an existing prepared snapshot instead of building one. */
  snapshotId?: string
  keepSnapshot: boolean
}

function parseArgs(argv: string[]): RunOptions {
  const positional = argv.filter((a) => !a.startsWith('--'))
  const flag = (name: string): string | undefined => {
    const index = argv.indexOf(`--${name}`)
    return index === -1 ? undefined : argv[index + 1]
  }
  const key = positional[0] ?? 'dry-run'
  const benchmark = argv.includes('--dry-run') ? DRY_RUN : BENCHMARKS[key]
  if (!benchmark) {
    throw new Error(`unknown benchmark "${key}" (have: ${Object.keys(BENCHMARKS).join(', ')})`)
  }
  return {
    benchmark,
    shards: Number(flag('shards') ?? 8),
    // Haiku 4.5 is the cheap policy default: $1/$5 per MTok vs Sonnet 5's
    // $3/$15. Override with --model for a capability run.
    model: flag('model') ?? 'claude-haiku-4-5',
    limit: flag('limit') ? Number(flag('limit')) : undefined,
    domain: flag('domain'),
    outDir: flag('out') ?? `bench-results/${benchmark.name}-${Date.now()}`,
    snapshotId: flag('snapshot'),
    keepSnapshot: argv.includes('--keep-snapshot'),
  }
}

function guestEnv(benchmark: Benchmark): Record<string, string> {
  const env: Record<string, string> = {}
  for (const key of benchmark.forwardEnv) {
    const value = process.env[key]
    if (value) env[key] = value
  }
  return env
}

/** Build the prepared snapshot: install once, freeze, destroy the source. */
async function buildSnapshot(client: SandboxClient, options: RunOptions): Promise<string> {
  console.log(`[prepare] creating a sandbox to install ${options.benchmark.name}`)
  const sandbox = await client.sandboxes.create({
    name: `prepare-${options.benchmark.name}`.slice(0, 64),
    // Never auto-pause mid-install, and give the install a generous TTL.
    idleTimeoutMs: -1,
    ttlMs: 60 * 60_000,
    metadata: { role: 'prepare', benchmark: options.benchmark.name },
  })
  try {
    const started = Date.now()
    await sandbox.commands.run(options.benchmark.prepare, {
      timeoutMs: 30 * 60_000,
      envs: guestEnv(options.benchmark),
      onStdout: (chunk) => process.stdout.write(chunk),
      onStderr: (chunk) => process.stderr.write(chunk),
    })
    console.log(`[prepare] install finished in ${((Date.now() - started) / 1000).toFixed(1)}s`)
    const snapshot = await sandbox.createSnapshot({ name: `${options.benchmark.name}-prepared` })
    console.log(`[prepare] snapshot ${snapshot.id}`)
    return snapshot.id
  } finally {
    // The snapshot is the artifact; the source VM has served its purpose.
    await sandbox.terminate().catch(() => {})
  }
}

interface ShardOutcome {
  shard: number
  sandboxId: string
  ok: boolean
  durationMs: number
  error?: string
  results?: unknown
}

async function runShard(
  sandbox: ClientSandbox,
  shard: number,
  options: RunOptions,
): Promise<ShardOutcome> {
  const started = Date.now()
  const logPath = path.join(options.outDir, `shard-${shard}.log`)
  const chunks: string[] = []
  try {
    const command = options.benchmark.shardCommand(shard, options.shards, options)
    // Streaming, not buffered: a long agent run would blow past the 2 MiB
    // per-stream cap on buffered exec, and a live log is what makes a 40-minute
    // shard debuggable while it runs.
    await sandbox.commands.run(command, {
      timeoutMs: 4 * 60 * 60_000,
      envs: guestEnv(options.benchmark),
      onStdout: (chunk) => {
        chunks.push(chunk)
        process.stdout.write(`[${shard}] ${chunk}`)
      },
      onStderr: (chunk) => chunks.push(chunk),
    })
    const raw = await sandbox.files.read(options.benchmark.resultPath)
    await writeFile(logPath, chunks.join(''))
    return {
      shard,
      sandboxId: sandbox.id,
      ok: true,
      durationMs: Date.now() - started,
      results: JSON.parse(raw),
    }
  } catch (err) {
    await writeFile(logPath, chunks.join('')).catch(() => {})
    return {
      shard,
      sandboxId: sandbox.id,
      ok: false,
      durationMs: Date.now() - started,
      error: err instanceof Error ? err.message : String(err),
    }
  }
}

async function main(): Promise<void> {
  const options = parseArgs(process.argv.slice(2))
  await mkdir(options.outDir, { recursive: true })
  const client = new SandboxClient({
    baseUrl: process.env.SANDBOX_API_URL,
    apiKey: process.env.SANDBOX_API_KEY,
  })

  console.log(
    `[run] ${options.benchmark.name}: ${options.shards} shards, model=${options.model}` +
      (options.limit ? `, limit=${options.limit}` : ''),
  )

  const snapshotId = options.snapshotId ?? (await buildSnapshot(client, options))

  console.log(`[fanout] cloning ${options.shards} sandboxes from ${snapshotId}`)
  const fanoutStarted = Date.now()
  let sandboxes: ClientSandbox[]
  try {
    // Batch create rather than N parallel creates: the server bounds
    // parallelism and the gateway queues past free capacity, so this is
    // back-pressure-aware. A CapacityError here means the fleet is genuinely
    // full — fewer shards, or wait for the autoscaler.
    const operation = await client.sandboxes.createMany({
      count: options.shards,
      source: { snapshotId },
      maxParallelism: 8,
      idleTimeoutMs: -1,
      ttlMs: 6 * 60 * 60_000,
      metadata: { role: 'shard', benchmark: options.benchmark.name },
    })
    const result = await operation.wait()
    sandboxes = result.results.flatMap((item) => (item.value ? [item.value] : []))
    const failed = result.results.filter((item) => !item.value)
    if (failed.length) {
      console.warn(
        `[fanout] ${failed.length}/${options.shards} clones failed: ` +
          failed.map((item) => `${item.index}:${item.error?.code}`).join(', '),
      )
    }
  } catch (err) {
    if (err instanceof CapacityError) {
      console.error('[fanout] fleet is at capacity — retry with fewer --shards')
    }
    throw err
  }
  console.log(
    `[fanout] ${sandboxes.length} sandboxes in ${((Date.now() - fanoutStarted) / 1000).toFixed(1)}s ` +
      `(${Math.round((Date.now() - fanoutStarted) / Math.max(sandboxes.length, 1))} ms/sandbox)`,
  )

  let outcomes: ShardOutcome[] = []
  try {
    // Every shard runs concurrently: the whole point of one VM per shard is
    // that they cannot interfere, so there is nothing to serialize.
    outcomes = await Promise.all(sandboxes.map((sandbox, index) => runShard(sandbox, index, options)))
  } finally {
    console.log('[cleanup] destroying shard sandboxes')
    await Promise.all(sandboxes.map((sandbox) => sandbox.terminate().catch(() => {})))
    if (!options.keepSnapshot && !options.snapshotId) {
      await client.snapshots.delete(snapshotId).catch(() => {})
    }
  }

  const ok = outcomes.filter((outcome) => outcome.ok)
  const summaryPath = path.join(options.outDir, 'summary.json')
  await writeFile(
    summaryPath,
    JSON.stringify({ options: { ...options, benchmark: options.benchmark.name }, outcomes }, null, 2),
  )

  console.log(`\n[done] ${ok.length}/${outcomes.length} shards succeeded`)
  for (const outcome of outcomes) {
    const status = outcome.ok ? 'ok  ' : 'FAIL'
    console.log(
      `  ${status} shard ${outcome.shard} ${(outcome.durationMs / 1000).toFixed(1)}s` +
        (outcome.error ? ` — ${outcome.error}` : ''),
    )
  }
  console.log(`[done] results in ${summaryPath}`)

  // What the run actually cost in sandbox time. Terminated sandboxes still have
  // usage — the ledger outlives the VM — so this works after teardown.
  try {
    const usage = await client.usage.report({})
    console.log(
      `[usage] ${usage.totals.intervals} intervals, ` +
        `${usage.totals.durationSeconds.toFixed(0)}s VM time, ` +
        `${usage.totals.vcpuSeconds.toFixed(0)} vcpu-seconds`,
    )
  } catch {
    console.log('[usage] fleet usage unavailable (a host may be unreachable — it fails closed)')
  }

  if (ok.length !== outcomes.length) process.exitCode = 1
}

main().catch((err) => {
  console.error(err)
  process.exit(1)
})
