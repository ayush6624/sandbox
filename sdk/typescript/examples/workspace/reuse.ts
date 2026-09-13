import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { SandboxClient, type ClientSandbox } from '../../src/index.js'
import { cleanupRunSandboxes } from '../../benchmarks/snapshot-batch-bench.js'

const snapshotId = process.argv[2]
assert.ok(snapshotId, 'Usage: npm run example:workspace:reuse -- <snapshot-id>')
const client = new SandboxClient()
const tags = { benchmark_run_id: randomUUID(), purpose: 'prepared-workspace-reuse' }
const tracked = new Map<string, ClientSandbox>()
try {
  const attempt = await client.sandboxes.create({ source: { snapshotId },
    ttlMs: 10 * 60_000, idleTimeoutMs: 10 * 60_000, metadata: tags,
    idempotencyKey: tags.benchmark_run_id })
  tracked.set(attempt.id, attempt)
  const result = await attempt.commands.run('python3 /home/sandbox/workspace/check.py', { timeoutMs: 120_000 })
  console.log(result.stdout)
} finally {
  const errors = await cleanupRunSandboxes(client, tracked, tags)
  if (errors.length) throw new Error(`Cleanup failed: ${errors.join('; ')}`)
}
