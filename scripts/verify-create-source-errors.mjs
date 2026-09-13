#!/usr/bin/env node
import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { mkdirSync, writeFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { SandboxClient, SandboxError } from '../sdk/typescript/src/index.ts'
import { cleanupRunSandboxes } from '../sdk/typescript/benchmarks/snapshot-batch-bench.ts'

const output = resolve(process.argv[2] ?? 'create-source-errors.json')
const client = new SandboxClient()
const tags = { benchmark_run_id: randomUUID(), purpose: 'verify-create-source-errors' }
const report = { passed: false, run_id: tags.benchmark_run_id, singles: [], batches: [], cleanup_errors: [] }
const tracked = new Map()
try {
  for (const type of ['snapshot', 'template']) {
    const source = { type, id: randomUUID() }
    const idempotencyKey = randomUUID()
    for (let replay = 0; replay < 2; replay++) {
      try {
        const sandbox = await client.sandboxes.create({ source, metadata: tags, ttlMs: 60_000, idempotencyKey })
        tracked.set(sandbox.id, sandbox)
        throw new Error(`Missing ${type} unexpectedly created ${sandbox.id}`)
      } catch (error) {
        if (!(error instanceof SandboxError)) throw error
        report.singles.push({ type, replay, status: error.status, code: error.code,
          request_id: error.requestId, message: error.message })
      }
    }
    const batchKey = randomUUID()
    const options = { source, count: 2, metadata: tags, ttlMs: 60_000, idempotencyKey: batchKey }
    const operation = await client.sandboxes.createMany(options)
    const state = await operation.wait({ timeoutMs: 120_000, signal: AbortSignal.timeout(120_000) })
    for (const result of state.results) if (result.value) tracked.set(result.value.id, result.value)
    const replay = await client.sandboxes.createMany(options)
    assert.equal(replay.id, operation.id)
    report.batches.push({ type, operation_id: operation.id, status: state.status,
      requested: state.requested, succeeded: state.succeeded, failed: state.failed,
      results: state.results.map((result) => ({ index: result.index, error: result.error })) })
  }
  for (const single of report.singles) {
    assert.equal(single.status, 404)
    assert.equal(single.code, 'source_not_found')
    assert.ok(single.request_id)
  }
  for (const batch of report.batches) {
    assert.equal(batch.status, 'failed')
    assert.equal(batch.succeeded, 0)
    assert.equal(batch.failed, 2)
    assert.deepEqual(batch.results.map((result) => result.index).sort(), [0, 1])
    for (const result of batch.results) {
      assert.equal(result.error?.status, 404)
      assert.equal(result.error?.code, 'source_not_found')
      assert.ok(result.error?.request_id)
    }
  }
  report.passed = true
} catch (error) {
  report.error = error instanceof Error ? error.message : String(error)
} finally {
  report.cleanup_errors = await cleanupRunSandboxes(client, tracked, tags)
  report.passed = report.passed && report.cleanup_errors.length === 0
  mkdirSync(dirname(output), { recursive: true })
  writeFileSync(output, `${JSON.stringify(report, null, 2)}\n`)
}
console.log(JSON.stringify({ passed: report.passed, output }))
if (!report.passed) process.exitCode = 1
