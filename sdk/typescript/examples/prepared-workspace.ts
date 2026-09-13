import assert from 'node:assert/strict'
import { mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { parseArgs } from 'node:util'
import { SandboxClient, SandboxError, CommandExitError, type ClientSandbox } from '../src/index.js'
import { benchmarkMetadata, benchmarkResourceMetadata, observeBuilds } from '../benchmarks/metadata.js'
import { observeBatch } from '../benchmarks/observe-batch.js'
import { cleanupRunSandboxes } from '../benchmarks/snapshot-batch-bench.js'

const { values } = parseArgs({ options: {
  output: { type: 'string', default: 'benchmarks/results/prepared-workspace.json' },
  count: { type: 'string', default: '2' },
  'keep-snapshot': { type: 'boolean', default: false },
} })
const count = Number(values.count)
assert.ok(Number.isInteger(count) && count >= 2 && count <= 32, '--count must be between 2 and 32')
const baseUrl = process.env.SANDBOX_API_URL
const apiKey = process.env.SANDBOX_API_KEY
assert.ok(baseUrl && apiKey, 'Set SANDBOX_API_URL and SANDBOX_API_KEY')
const client = new SandboxClient({ baseUrl, apiKey, requestTimeoutMs: 120_000 })
const metadata = benchmarkMetadata('prepared-workspace', {
  repository: 'https://github.com/pallets/itsdangerous',
  revision: '096c8d42545d3b68ea21a4f890fb2b2d8979c0bd', count,
}, baseUrl)
const tags = benchmarkResourceMetadata(metadata)
const tracked = new Map<string, ClientSandbox>()
const output = resolve(values.output)
mkdirSync(dirname(output), { recursive: true })

function diagnostic(error: unknown) {
  return {
    message: error instanceof Error ? error.message : String(error),
    ...(error instanceof SandboxError ? { code: error.code, request_id: error.requestId, status: error.status } : {}),
    ...(error instanceof CommandExitError ? { command: error.result } : {}),
  }
}

interface CheckedTests {
  revision: string
  tests: number
  failures: number
  errors: number
  skipped: number
  files_sha256: string
  dependencies_sha256: string
  workspace_allocated_bytes: number
}

async function check(sandbox: ClientSandbox): Promise<CheckedTests> {
  const result = await sandbox.commands.run('python3 /home/sandbox/workspace/check.py', { timeoutMs: 120_000 })
  const value: unknown = JSON.parse(result.stdout)
  assert.ok(value && typeof value === 'object')
  assert.ok('revision' in value && typeof value.revision === 'string' && value.revision === metadata.workload.revision)
  assert.ok('tests' in value && value.tests === 297)
  assert.ok('failures' in value && value.failures === 0)
  assert.ok('errors' in value && value.errors === 0)
  assert.ok('skipped' in value && value.skipped === 0)
  assert.ok('files_sha256' in value && typeof value.files_sha256 === 'string')
  assert.ok('dependencies_sha256' in value && typeof value.dependencies_sha256 === 'string')
  assert.ok('workspace_allocated_bytes' in value && typeof value.workspace_allocated_bytes === 'number')
  return { revision: value.revision, tests: value.tests, failures: value.failures,
    errors: value.errors, skipped: value.skipped, files_sha256: value.files_sha256,
    dependencies_sha256: value.dependencies_sha256, workspace_allocated_bytes: value.workspace_allocated_bytes }
}

type Attempt = { index: number; sandbox_id: string; first_command_ms: number; tests_complete_ms: number; checked: CheckedTests }
const report: {
  metadata: typeof metadata; passed: boolean; stage: string
  source_id: string; snapshot_id: string; operation_id: string; snapshot_retained: boolean
  timings_ms: Record<string, number>; attempts: Attempt[]
  baseline?: CheckedTests
  isolation?: { changed_sandbox_id: string; changed_files_sha256: string; unchanged_sibling_ids: string[];
    source_unchanged: true; snapshot_probe_id: string; snapshot_files_sha256: string }
  failures: Array<{ stage: string; error: ReturnType<typeof diagnostic> }>; cleanup_errors: string[]
} = {
  metadata, passed: false, stage: 'starting',
  source_id: '', snapshot_id: '', operation_id: '', snapshot_retained: false,
  timings_ms: {}, attempts: [], failures: [], cleanup_errors: [],
}
function save() {
  writeFileSync(`${output}.tmp`, `${JSON.stringify(report, null, 2)}\n`)
  renameSync(`${output}.tmp`, output)
}
function stage(name: string) { report.stage = name; save() }
let workflowPassed = false
try {
  save()
  await observeBuilds(metadata, baseUrl, apiKey)
  stage('fresh-create')
  let tick = performance.now()
  const freshStarted = tick
  const source = await client.sandboxes.create({ name: 'workspace-source', ttlMs: 30 * 60_000,
    idleTimeoutMs: 30 * 60_000, metadata: tags, idempotencyKey: `${metadata.run_id}-source` })
  tracked.set(source.id, source)
  report.source_id = source.id
  report.timings_ms.fresh_create = performance.now() - tick
  const ready = await source.commands.run('python3 --version && git --version')
  assert.ok(ready.stdout.includes('Python 3.') && ready.stdout.includes('git version'))
  report.timings_ms.fresh_first_command = performance.now() - tick
  stage('prepare')
  tick = performance.now()
  const root = '/home/sandbox/workspace'
  for (const file of ['prepare.sh', 'check.py']) {
    await source.files.write(`${root}/${file}`, readFileSync(new URL(`./workspace/${file}`, import.meta.url)))
  }
  await source.commands.run(`bash ${root}/prepare.sh`, { timeoutMs: 300_000 })
  report.timings_ms.prepare = performance.now() - tick
  stage('source-tests')
  tick = performance.now()
  const baseline = await check(source)
  report.baseline = baseline
  report.timings_ms.source_tests = performance.now() - tick
  report.timings_ms.fresh_through_tests = performance.now() - freshStarted
  stage('snapshot-capture')
  tick = performance.now()
  const snapshot = await source.createSnapshot({ name: `workspace-${metadata.run_id}`, retentionMs: 60 * 60_000,
    idempotencyKey: `${metadata.run_id}-snapshot` })
  report.snapshot_id = snapshot.id
  report.timings_ms.snapshot_capture = performance.now() - tick
  stage('snapshot-durability')
  tick = performance.now()
  await client.snapshots.waitForDurable(snapshot.id, { timeoutMs: 300_000 })
  report.timings_ms.snapshot_durability = performance.now() - tick
  stage('snapshot-attempts')
  const batchStarted = performance.now()
  const operation = await client.sandboxes.createMany({ count, maxParallelism: Math.min(count, 8),
    source: { snapshotId: snapshot.id }, ttlMs: 30 * 60_000, idleTimeoutMs: 30 * 60_000,
    metadata: tags, idempotencyKey: `${metadata.run_id}-attempts` })
  report.operation_id = operation.id
  save()
  await observeBatch({ operation, timeoutMs: 300_000, onSandbox: async (sandbox, index) => {
    tracked.set(sandbox.id, sandbox)
    try {
      const ready = await sandbox.commands.run(`test -x ${root}/.venv/bin/python && cat ${root}/dependencies.txt`)
      assert.ok(ready.stdout.includes('pytest==8.1.1'))
      const firstCommandMs = performance.now() - batchStarted
      const checked = await check(sandbox)
      assert.equal(checked.tests, baseline.tests)
      assert.equal(checked.files_sha256, baseline.files_sha256)
      assert.equal(checked.dependencies_sha256, baseline.dependencies_sha256)
      report.attempts.push({ index, sandbox_id: sandbox.id, first_command_ms: firstCommandMs,
        tests_complete_ms: performance.now() - batchStarted, checked })
      save()
    } catch (error) {
      report.failures.push({ stage: `attempt-${index}`, error: diagnostic(error) }); save()
    }
  } })
  report.timings_ms.all_attempts_tests = performance.now() - batchStarted
  for (const item of operation.state.results) {
    if (item.error) report.failures.push({ stage: `create-${item.index}`, error: { message: JSON.stringify(item.error) } })
  }
  assert.equal(report.attempts.length, count, 'some attempts did not pass; inspect failures and operation_id')
  assert.equal(new Set(report.attempts.map((attempt) => attempt.sandbox_id)).size, count)
  stage('isolation')
  const first = report.attempts[0]
  assert.ok(first)
  const changed = tracked.get(first.sandbox_id)
  assert.ok(changed)
  const modulePath = `${root}/repo/src/itsdangerous/__init__.py`
  const original = await changed.files.read(modulePath)
  await changed.files.write(modulePath, `${original}\nWORKSPACE_ATTEMPT = 'changed'\n`)
  const mutated = await check(changed)
  assert.notEqual(mutated.files_sha256, baseline.files_sha256)
  for (const attempt of report.attempts.slice(1)) {
    const sibling = tracked.get(attempt.sandbox_id)
    assert.ok(sibling)
    assert.equal(await sibling.files.read(modulePath), original, `sibling ${attempt.index} changed`)
  }
  assert.equal(await source.files.read(modulePath), original, 'source changed')
  const pristine = await client.sandboxes.create({ source: { snapshotId: snapshot.id },
    metadata: tags, ttlMs: 30 * 60_000, idleTimeoutMs: 30 * 60_000, idempotencyKey: `${metadata.run_id}-pristine` })
  tracked.set(pristine.id, pristine)
  assert.equal((await check(pristine)).files_sha256, baseline.files_sha256, 'saved snapshot changed')
  report.isolation = { changed_sandbox_id: changed.id, changed_files_sha256: mutated.files_sha256,
    unchanged_sibling_ids: report.attempts.slice(1).map((attempt) => attempt.sandbox_id),
    source_unchanged: true, snapshot_probe_id: pristine.id, snapshot_files_sha256: baseline.files_sha256 }
  workflowPassed = true
} catch (error) {
  report.failures.push({ stage: report.stage, error: diagnostic(error) })
} finally {
  stage('cleanup')
  report.cleanup_errors.push(...await cleanupRunSandboxes(client, tracked, tags))
  if (report.snapshot_id) {
    if (workflowPassed && values['keep-snapshot']) report.snapshot_retained = true
    else try { await client.snapshots.delete(report.snapshot_id) }
    catch (error) { report.cleanup_errors.push(`snapshot ${report.snapshot_id}: ${diagnostic(error).message}`) }
  }
  await observeBuilds(metadata, baseUrl, apiKey)
  report.passed = workflowPassed && report.failures.length === 0 && report.cleanup_errors.length === 0
  report.stage = report.passed ? 'complete' : 'failed'
  save()
}
console.log(JSON.stringify({ passed: report.passed, report: output, snapshot_id: report.snapshot_retained ? report.snapshot_id : undefined }))
if (!report.passed) process.exitCode = 1
