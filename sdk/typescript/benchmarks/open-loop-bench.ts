import { mkdirSync, writeFileSync } from 'node:fs'
import { dirname } from 'node:path'
import { setTimeout as sleep } from 'node:timers/promises'
import { SandboxClient, SandboxError, type ClientSandbox } from '../src/index.js'
import { benchmarkMetadata, benchmarkResourceMetadata, observeBuilds } from './metadata.js'
import { cleanupRunSandboxes } from './snapshot-batch-bench.js'
import { runArrivals } from './open-loop-arrivals.js'

function positive(name: string, fallback: number): number {
  const value = Number(process.env[name] ?? fallback)
  if (!Number.isFinite(value) || value <= 0) throw new Error(`${name} must be positive and finite`)
  return value
}
const count = positive('BENCH_COUNT', 60)
if (!Number.isInteger(count)) throw new Error('BENCH_COUNT must be an integer')
const rate = positive('BENCH_RATE', 5)
const holdMs = positive('BENCH_HOLD_MS', 2000)
const apiUrl = process.env.SANDBOX_API_URL
const apiKey = process.env.SANDBOX_API_KEY
if (!apiUrl || !apiKey) throw new Error('SANDBOX_API_URL and SANDBOX_API_KEY are required')
const output = process.env.BENCH_OUTPUT ?? 'benchmarks/results/open-loop.json'
const client = new SandboxClient({ baseUrl: apiUrl, apiKey, requestTimeoutMs: 120_000, maxRetries: 0 })
const metadata = benchmarkMetadata('open-loop-create', {
  count, arrivals_per_second: rate, hold_ms: holdMs, retries: 0,
  command: 'printf benchmark-ready', source: 'default',
  timing: 'schedule delay and client elapsed times; transport included; per-request server queue time unavailable',
})
const labels = benchmarkResourceMetadata(metadata)
const tracked = new Map<string, ClientSandbox>()
const ids = new Set<string>()
const records: Array<{
  index: number; scheduled_ms: number; dispatched_ms: number; schedule_delay_ms: number;
  sandbox_id?: string; create_ms?: number; command_ready_ms?: number; total_ms?: number;
  error?: { stage: string; message: string; status?: number; code?: string; request_id?: string }
}> = []
const metrics: Array<{ elapsed_ms: number; endpoint: string; text?: string; error?: string }> = []
const failures: string[] = []
const cleanupErrors: string[] = []
let inFlight = 0
let peakInFlight = 0
let stopMetrics = false
const started = performance.now()
const message = (error: unknown) => error instanceof Error ? error.message : String(error)

async function scrape(endpoint: string): Promise<void> {
  try {
    const response = await fetch(`${apiUrl}${endpoint}`, {
      headers: { Authorization: `Bearer ${apiKey}` }, signal: AbortSignal.timeout(5000),
    })
    if (!response.ok) throw new Error(`HTTP ${response.status}`)
    const text = (await response.text()).split('\n').filter(line =>
      /^sandbox_(build_info|host_scrape_ok|create_|placement_|warm|running|starting|stopping|slots_|pool_|committed_mem|mem_budget|hosts_)/.test(line),
    ).join('\n')
    if (/^sandbox_host_scrape_ok\{[^}]+\}\s+0\s*$/m.test(text)) throw new Error('incomplete worker scrape')
    metrics.push({ elapsed_ms: performance.now() - started, endpoint, text })
  } catch (error) { metrics.push({ elapsed_ms: performance.now() - started, endpoint, error: message(error) }) }
}

await observeBuilds(metadata, apiUrl, apiKey)
await Promise.all([scrape('/metrics'), scrape('/metrics/hosts')])
const monitor = (async () => {
  while (!stopMetrics) {
    await sleep(500)
    if (!stopMetrics) await Promise.all([scrape('/metrics'), scrape('/metrics/hosts')])
  }
})()
try {
  const outcomes = await runArrivals({ count, rate, task: async ({ index, scheduledMs, dispatchedMs }) => {
    const rec: typeof records[number] = {
      index, scheduled_ms: scheduledMs, dispatched_ms: dispatchedMs, schedule_delay_ms: dispatchedMs - scheduledMs,
    }
    records.push(rec)
    const began = performance.now()
    inFlight++
    peakInFlight = Math.max(peakInFlight, inFlight)
    let sandbox: ClientSandbox | undefined
    let stage = 'create'
    try {
      sandbox = await client.sandboxes.create({ metadata: labels, ttlMs: 600_000, idleTimeoutMs: 0 })
      rec.sandbox_id = sandbox.id
      ids.add(sandbox.id)
      tracked.set(sandbox.id, sandbox)
      rec.create_ms = performance.now() - began
      stage = 'command'
      const result = await sandbox.commands.run('printf benchmark-ready', { timeoutMs: 30_000 })
      if (result.exitCode !== 0 || result.stdout !== 'benchmark-ready') throw new Error('command output mismatch')
      rec.command_ready_ms = performance.now() - began
      await sleep(holdMs)
      stage = 'terminate'
      await sandbox.terminate({ timeoutMs: 30_000 })
      tracked.delete(sandbox.id)
    } catch (error) {
      rec.error = { stage, message: message(error), ...(error instanceof SandboxError
        ? { status: error.status, code: error.code, request_id: error.requestId } : {}) }
    } finally {
      rec.total_ms = performance.now() - began
      inFlight--
    }
  } })
  for (const result of outcomes) if (result.status === 'rejected') failures.push(message(result.reason))
} catch (error) { failures.push(message(error)) }
finally {
  stopMetrics = true
  await monitor
  cleanupErrors.push(...await cleanupRunSandboxes(client, tracked, labels))
  for (const id of ids) {
    try {
      await client.sandboxes.get(id)
      cleanupErrors.push(`${id}: still exists`)
    } catch (error) {
      if (!(error instanceof SandboxError) || error.status !== 404) cleanupErrors.push(`${id}: ${message(error)}`)
    }
  }
  await observeBuilds(metadata, apiUrl, apiKey)
  await Promise.all([scrape('/metrics'), scrape('/metrics/hosts')])
  const succeeded = records.filter(rec => rec.command_ready_ms !== undefined && !rec.error).length
  const passed = succeeded === count && !failures.length && !cleanupErrors.length && metrics.every(row => !row.error)
  mkdirSync(dirname(output), { recursive: true })
  writeFileSync(output, JSON.stringify({ metadata, passed, succeeded, peak_in_flight: peakInFlight, records, metrics, failures,
    cleanupErrors, cleanup: { sandbox_ids_checked_absent: [...ids] } }, null, 2))
  console.log(JSON.stringify({ output, passed, succeeded, requested: count, peakInFlight, failures, cleanupErrors,
    errors: records.filter(rec => rec.error).map(rec => rec.error) }))
  if (!passed) process.exitCode = 1
}
