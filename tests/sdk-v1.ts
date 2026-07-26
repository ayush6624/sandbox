/** Destructive-but-self-cleaning GCP probe for the TypeScript SDK v1 layer. */
import assert from 'node:assert/strict'

import { NotFoundError, SandboxClient } from '../sdk/typescript/src/index.js'

const baseUrl = process.env.SANDBOX_API_URL
const apiKey = process.env.SANDBOX_API_KEY
if (!baseUrl || !apiKey) throw new Error('SANDBOX_API_URL and SANDBOX_API_KEY are required')

const client = new SandboxClient({ baseUrl, apiKey, maxRetries: 2 })
const runId = `sdk-v1-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`
const cleanup = new Map<string, { terminate(): Promise<void> }>()
let snapshotId = ''

async function main(): Promise<void> {
  const source = await client.sandboxes.create({
    name: 'sdk-v1-source',
    source: { templateId: 'default' },
    ttlMs: 15 * 60_000,
    idleTimeoutMs: 10 * 60_000,
    metadata: { probe: 'sdk-v1', run_id: runId, role: 'source' },
  })
  cleanup.set(source.id, source)
  assert.equal(source.source.type, 'template')
  assert.equal(source.metadata.run_id, runId)
  assert.equal((await source.commands.run('printf sdk-v1-ready')).stdout, 'sdk-v1-ready')
  await source.commands.run('printf prepared > /home/sandbox/app/sdk-v1-state')

  const port = await source.createPortForward(3000)
  assert.equal(port.sandboxId, source.id)
  assert.equal((await source.listPortForwards())[0]?.guestPort, 3000)

  await source.pause()
  assert.equal(source.status, 'paused')
  await source.resume()
  assert.equal(source.status, 'running')

  const snapshot = await source.createSnapshot({ name: 'sdk-v1-snapshot', retentionMs: 60 * 60_000 })
  snapshotId = snapshot.id
  assert.equal(snapshot.sourceSandboxId, source.id)

  const operation = await client.sandboxes.createMany({
    count: 3,
    source: { snapshotId },
    maxParallelism: 2,
    ttlMs: 15 * 60_000,
    idleTimeoutMs: 10 * 60_000,
    metadata: { probe: 'sdk-v1', run_id: runId, role: 'batch' },
  })
  const completed = await operation.wait({ pollIntervalMs: 250, timeoutMs: 120_000 })
  assert.equal(completed.requested, 3)
  assert.equal(completed.failed, 0, JSON.stringify(completed.results.map((item) => item.error)))
  assert.equal(completed.results.length, 3)
  for (const item of completed.results) {
    assert.ok(item.value, `batch item ${item.index} has no sandbox`)
    cleanup.set(item.value!.id, item.value!)
  }
  const first = completed.results[0]!.value!
  assert.equal((await first.commands.run('cat /home/sandbox/app/sdk-v1-state')).stdout, 'prepared')

  const listed: string[] = []
  for await (const sandbox of client.sandboxes.list({ metadata: { run_id: runId }, pageSize: 1 })) {
    listed.push(sandbox.id)
  }
  assert.equal(listed.length, 4)

  await assert.rejects(
    client.sandboxes.get(`missing-${runId}`),
    (error: unknown) => error instanceof NotFoundError && error.code === 'not_found' && Boolean(error.requestId),
  )

  for (const sandbox of [...cleanup.values()].reverse()) await sandbox.terminate()
  cleanup.clear()
  await client.snapshots.delete(snapshotId)
  snapshotId = ''
  console.log('SDK v1 fleet probe passed')
}

main().catch(async (error) => {
  for (const sandbox of [...cleanup.values()].reverse()) {
    await sandbox.terminate().catch(() => undefined)
  }
  if (snapshotId) await client.snapshots.delete(snapshotId).catch(() => undefined)
  console.error(error)
  process.exitCode = 1
})
