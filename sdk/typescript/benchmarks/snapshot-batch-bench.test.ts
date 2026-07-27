import assert from 'node:assert/strict'
import { test } from 'node:test'
import type { ClientSandbox, SandboxClient } from '../src/index.js'
import {
  cleanupRunSandboxes,
  deleteSnapshotWithRetry,
} from './snapshot-batch-bench.js'

const resourceMetadata = {
  benchmark: 'snapshot-source-batch-create',
  benchmark_run_id: 'run-under-test',
  benchmark_release: 'release-under-test',
}

function sandbox(
  id: string,
  metadata: Record<string, string>,
  terminate: () => Promise<void>,
): ClientSandbox {
  return { id, metadata, terminate } as unknown as ClientSandbox
}

test('metadata cleanup discovers lost results, retries listing, and ignores unrelated resources', async () => {
  let listCalls = 0
  let ownedTerminations = 0
  let unrelatedTerminations = 0
  const owned = sandbox('owned', resourceMetadata, async () => { ownedTerminations++ })
  const unrelated = sandbox(
    'unrelated',
    { ...resourceMetadata, benchmark_run_id: 'some-other-run' },
    async () => { unrelatedTerminations++ },
  )
  const client = {
    sandboxes: {
      list: () => (async function* () {
        listCalls++
        if (listCalls === 1) throw new Error('transient list failure')
        if (listCalls === 2) {
          yield owned
          // Deliberately emulate an incorrectly broad server-side filter.
          yield unrelated
        }
      })(),
    },
  }

  const errors = await cleanupRunSandboxes(
    client,
    new Map(),
    resourceMetadata,
    { attempts: 4, delayMs: 1 },
  )

  assert.deepEqual(errors, [])
  assert.equal(listCalls, 3)
  assert.equal(ownedTerminations, 1)
  assert.equal(unrelatedTerminations, 0)
})

test('metadata cleanup refuses discovery without a unique run id', async () => {
  let listed = false
  const client = {
    sandboxes: {
      list: () => {
        listed = true
        return (async function* () {})()
      },
    },
  }

  const errors = await cleanupRunSandboxes(
    client,
    new Map(),
    { benchmark: 'snapshot-source-batch-create' },
    { attempts: 1, delayMs: 1 },
  )

  assert.deepEqual(errors, ['refusing metadata cleanup without benchmark_run_id'])
  assert.equal(listed, false)
})

test('snapshot cleanup retries transient failures boundedly', async () => {
  let attempts = 0
  const client = {
    snapshots: {
      delete: async () => {
        attempts++
        if (attempts < 3) throw new Error('temporary network failure')
      },
    },
  } as unknown as Pick<SandboxClient, 'snapshots'>

  const error = await deleteSnapshotWithRetry(
    client,
    'snapshot-under-test',
    { attempts: 3, delayMs: 1 },
  )

  assert.equal(error, undefined)
  assert.equal(attempts, 3)
})

test('snapshot cleanup treats already-deleted snapshots as success', async () => {
  const client = {
    snapshots: {
      delete: async () => {
        throw Object.assign(new Error('not found'), { status: 404 })
      },
    },
  } as unknown as Pick<SandboxClient, 'snapshots'>

  assert.equal(
    await deleteSnapshotWithRetry(client, 'gone', { attempts: 3, delayMs: 1 }),
    undefined,
  )
})
