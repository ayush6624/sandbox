import assert from 'node:assert/strict'
import { afterEach, test } from 'node:test'

import { benchmarkMetadata, benchmarkResourceMetadata } from './metadata.js'

const KEYS = ['BENCH_RUN_ID', 'SANDBOX_RELEASE', 'BENCH_RELEASE', 'SANDBOX_API_URL'] as const
const original = Object.fromEntries(KEYS.map((key) => [key, process.env[key]]))

afterEach(() => {
  for (const key of KEYS) {
    const value = original[key]
    if (value === undefined) delete process.env[key]
    else process.env[key] = value
  }
})

test('benchmark metadata attributes the release and redacts the target path', () => {
  process.env.BENCH_RUN_ID = 'gcp-suite-42'
  process.env.SANDBOX_RELEASE = 'release-42'
  process.env.SANDBOX_API_URL = 'https://gateway.example.test:9090/private/path?token=secret'

  const metadata = benchmarkMetadata('snapshot-source-create', { iterations: 25 })

  assert.equal(metadata.schema_version, 2)
  assert.equal(metadata.api_version, 'v1')
  assert.equal(metadata.run_id, 'gcp-suite-42')
  assert.equal(metadata.release, 'release-42')
  assert.equal(metadata.target, 'https://gateway.example.test:9090')
  assert.deepEqual(metadata.workload, { iterations: 25 })
})

test('resource metadata provides stable cleanup labels', () => {
  process.env.BENCH_RUN_ID = 'cleanup-run'
  process.env.BENCH_RELEASE = 'candidate-7'

  const labels = benchmarkResourceMetadata(benchmarkMetadata('fleet-sqlite-filesystem', {}))

  assert.deepEqual(labels, {
    benchmark: 'fleet-sqlite-filesystem',
    benchmark_run_id: 'cleanup-run',
    benchmark_release: 'candidate-7',
  })
})
