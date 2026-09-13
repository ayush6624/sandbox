import { mkdirSync, writeFileSync } from 'node:fs'
import { dirname } from 'node:path'
import { SandboxClient, SandboxError, type ClientSandbox, type SandboxSource } from '../src/index.js'
import { benchmarkMetadata, benchmarkResourceMetadata, observeBuilds } from './metadata.js'
import { cleanupRunSandboxes, deleteSnapshotWithRetry } from './snapshot-batch-bench.js'
import { observeBatch } from './observe-batch.js'

const apiUrl = process.env.SANDBOX_API_URL
const apiKey = process.env.SANDBOX_API_KEY
if (!apiUrl || !apiKey) throw new Error('SANDBOX_API_URL and SANDBOX_API_KEY are required')
const output = process.env.BENCH_OUTPUT ?? 'benchmarks/results/create-paths.json'
const iterations = 5
const counts = [4, 8, 16]
const client = new SandboxClient({ baseUrl: apiUrl, apiKey, requestTimeoutMs: 120_000 })
const metadata = benchmarkMetadata('create-paths', {
  iterations, counts, readiness_probe: 'printf benchmark-ready',
  sources: 'default warm pool; disposable durable snapshot without a warm target',
  timing: 'client wall time including transport and operation polling; no latency SLO asserted',
  classification: 'aggregate counter deltas per request or batch, never per-child batch miss counts',
})
const labels = benchmarkResourceMetadata(metadata)
const tracked = new Map<string, ClientSandbox>()
const createdIds = new Set<string>()
const failures: string[] = []
const cleanupErrors: string[] = []
const samples: Array<{ phase: string; index: number; sandbox_id: string; api_ms: number; command_ready_ms: number }> = []
const batches: Array<{ count: number; accepted_ms: number; operation_ms: number; status: string; results: unknown; items: typeof samples }> = []
const observations: Array<{ phase: string; captured_at: string; scrape_ms: number; metrics: string }> = []
let snapshotId: string | undefined

function message(error: unknown): string { return error instanceof Error ? error.message : String(error) }

async function observe(phase: string): Promise<string> {
  const started = performance.now()
  const response = await fetch(`${apiUrl}/metrics/hosts`, {
    headers: { Authorization: `Bearer ${apiKey}` }, signal: AbortSignal.timeout(15_000),
  })
  if (!response.ok) throw new Error(`metrics HTTP ${response.status}`)
  const metrics = (await response.text()).split('\n').filter(line =>
    /^sandbox_(build_info|host_scrape_ok|warm|template_warm|running|starting|capacity|free_slots)/.test(line),
  ).join('\n')
  if (/^sandbox_host_scrape_ok\{[^}]+\}\s+0\s*$/m.test(metrics)) throw new Error('worker scrape incomplete')
  observations.push({ phase, captured_at: new Date().toISOString(), scrape_ms: performance.now() - started, metrics })
  return metrics
}

function counter(metrics: string, name: string): number {
  return metrics.split('\n').reduce((sum, line) => {
    if (!line.startsWith(`${name}{`) && !line.startsWith(`${name} `)) return sum
    const value = Number(line.slice(line.lastIndexOf(' ') + 1))
    if (!Number.isFinite(value)) throw new Error(`invalid ${name}`)
    return sum + value
  }, 0)
}

function checkPath(before: string, after: string, expected: 'warm' | 'miss'): void {
  const claims = counter(after, 'sandbox_warm_claims_total') - counter(before, 'sandbox_warm_claims_total')
  const misses = counter(after, 'sandbox_warm_misses_total') - counter(before, 'sandbox_warm_misses_total')
  if (expected === 'warm' ? claims !== 1 || misses !== 0 : claims !== 0 || misses < 1) {
    throw new Error(`ambiguous ${expected} classification: claims=${claims}, misses=${misses}`)
  }
}

function track(sandbox: ClientSandbox): void {
  tracked.set(sandbox.id, sandbox)
  createdIds.add(sandbox.id)
}

async function verify(sandbox: ClientSandbox): Promise<void> {
  const result = await sandbox.commands.run('printf benchmark-ready', { timeoutMs: 30_000 })
  if (result.exitCode !== 0 || result.stdout !== 'benchmark-ready') throw new Error(`command probe failed for ${sandbox.id}`)
}

async function terminate(sandbox: ClientSandbox): Promise<void> {
  await sandbox.terminate({ timeoutMs: 120_000 })
  tracked.delete(sandbox.id)
}

async function sample(phase: 'default-warm' | 'snapshot-miss', index: number, source: SandboxSource): Promise<void> {
  const before = await observe(`${phase}-${index}-before`)
  const started = performance.now()
  const sandbox = await client.sandboxes.create({ source, metadata: labels, ttlMs: 600_000, idleTimeoutMs: 0 })
  track(sandbox)
  const apiMs = performance.now() - started
  await verify(sandbox)
  samples.push({ phase, index, sandbox_id: sandbox.id, api_ms: apiMs, command_ready_ms: performance.now() - started })
  const after = await observe(`${phase}-${index}-after`)
  checkPath(before, after, phase === 'default-warm' ? 'warm' : 'miss')
  await terminate(sandbox)
  console.log(`${phase} ${index}: ${Math.round(apiMs)} ms API, ${Math.round(samples.at(-1)?.command_ready_ms ?? 0)} ms command`)
}

try {
  await observeBuilds(metadata, apiUrl, apiKey)
  await observe('initial')
  for (let index = 1; index <= iterations; index++) await sample('default-warm', index, { type: 'default' })
  const source = await client.sandboxes.create({ metadata: labels, ttlMs: 600_000, idleTimeoutMs: 0 })
  track(source)
  await verify(source)
  const snapshot = await source.createSnapshot({ name: `create-paths-${metadata.run_id}`, retentionMs: 3_600_000 })
  snapshotId = snapshot.id
  await client.snapshots.waitForDurable(snapshotId, { timeoutMs: 180_000 })
  await terminate(source)
  for (let index = 1; index <= iterations; index++) await sample('snapshot-miss', index, { snapshotId })
  for (const count of counts) {
    const before = await observe(`batch-${count}-before`)
    const started = performance.now()
    const operation = await client.sandboxes.createMany({
      count, maxParallelism: count, source: { snapshotId }, metadata: labels, ttlMs: 600_000, idleTimeoutMs: 0,
    })
    const acceptedMs = performance.now() - started
    const items: typeof samples = []
    const operationMs = acceptedMs + await observeBatch({
      operation, timeoutMs: 180_000,
      onSandbox: async (sandbox, index) => {
        track(sandbox)
        const apiMs = performance.now() - started
        await verify(sandbox)
        items.push({ phase: `batch-${count}`, index, sandbox_id: sandbox.id, api_ms: apiMs, command_ready_ms: performance.now() - started })
      },
    })
    batches.push({ count, accepted_ms: acceptedMs, operation_ms: operationMs, status: operation.state.status,
      results: operation.state.results.map(item => ({ index: item.index, error: item.error, sandbox_id: item.value?.id })), items })
    if (items.length !== count) throw new Error(`batch ${count}: ${items.length} commands succeeded`)
    const after = await observe(`batch-${count}-after`)
    checkPath(before, after, 'miss')
    for (const sandbox of [...tracked.values()]) await terminate(sandbox)
    console.log(`batch ${count}: ${Math.round(operationMs)} ms operation, ${Math.round(Math.max(...items.map(item => item.command_ready_ms)))} ms all commands`)
  }
} catch (error) {
  failures.push(message(error))
} finally {
  cleanupErrors.push(...await cleanupRunSandboxes(client, tracked, labels))
  for (const id of createdIds) {
    try {
      await client.sandboxes.get(id)
      cleanupErrors.push(`${id}: still exists after cleanup`)
    } catch (error) {
      if (!(error instanceof SandboxError) || error.status !== 404) cleanupErrors.push(`${id}: ${message(error)}`)
    }
  }
  if (snapshotId) {
    const error = await deleteSnapshotWithRetry(client, snapshotId)
    if (error) cleanupErrors.push(error)
    try {
      await client.snapshots.get(snapshotId)
      cleanupErrors.push(`${snapshotId}: snapshot still exists`)
    } catch (error) {
      if (!(error instanceof SandboxError) || error.status !== 404) cleanupErrors.push(`${snapshotId}: ${message(error)}`)
    }
  }
  await observeBuilds(metadata, apiUrl, apiKey)
  try { await observe('final') } catch (error) { failures.push(message(error)) }
  const passed = failures.length === 0 && cleanupErrors.length === 0 && samples.length === iterations * 2 && batches.length === counts.length
  mkdirSync(dirname(output), { recursive: true })
  writeFileSync(output, JSON.stringify({ metadata, passed, snapshot_id: snapshotId, samples, batches, observations, failures, cleanupErrors,
    cleanup: { sandbox_ids_checked_absent: [...createdIds], snapshot_checked_absent: snapshotId } }, null, 2))
  console.log(JSON.stringify({ output, passed, failures, cleanupErrors }))
  if (!passed) process.exitCode = 1
}
