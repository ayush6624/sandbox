import { SandboxClient } from '../src/index.js'

const client = new SandboxClient()
const prepared = await client.sandboxes.create({
  name: 'prepared-source',
  ttlMs: 10 * 60_000,
  idleTimeoutMs: 5 * 60_000,
})

try {
  await prepared.commands.run('mkdir -p /home/sandbox/app && echo ready > /home/sandbox/app/state.txt')
  const snapshot = await prepared.createSnapshot({ name: 'prepared-state', retentionMs: 60 * 60_000 })
  const operation = await client.sandboxes.createMany({
    count: 8,
    source: { snapshotId: snapshot.id },
    maxParallelism: 4,
    metadata: { purpose: 'example' },
  })
  const completed = await operation.wait()
  console.log(`${completed.succeeded}/${completed.requested} sandboxes created`)
  for (const result of completed.results) {
    if (result.value) await result.value.terminate()
    else console.error(`item ${result.index}: ${result.error?.code}`)
  }
  await client.snapshots.delete(snapshot.id)
} finally {
  await prepared.terminate().catch(() => undefined)
}
