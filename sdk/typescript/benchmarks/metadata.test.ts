import assert from 'node:assert/strict'
import { afterEach, test } from 'node:test'
import { readFileSync } from 'node:fs'

import { benchmarkMetadata, benchmarkResourceMetadata, provenanceIssues } from './metadata.js'

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
  delete process.env.SANDBOX_RELEASE
  process.env.BENCH_RUN_ID = 'cleanup-run'
  process.env.BENCH_RELEASE = 'candidate-7'

  const labels = benchmarkResourceMetadata(benchmarkMetadata('fleet-sqlite-filesystem', {}))

  assert.deepEqual(labels, {
    benchmark: 'fleet-sqlite-filesystem',
    benchmark_run_id: 'cleanup-run',
    benchmark_release: 'candidate-7',
  })
})

test('direct invocation uses the actual SDK version and unique run identities', () => {
  delete process.env.BENCH_RUN_ID
  const first = benchmarkMetadata('test', {})
  const second = benchmarkMetadata('test', {})
  const manifest: unknown = JSON.parse(readFileSync(new URL('../package.json', import.meta.url), 'utf8'))
  assert.ok(manifest && typeof manifest === 'object' && 'version' in manifest)
  assert.equal(first.sdk.version, manifest.version)
  assert.notEqual(first.run_id, second.run_id)
})

test('explicit targets override ambient configuration and malformed URLs cannot leak credentials', () => {
  process.env.SANDBOX_API_URL = 'https://wrong.example.test'
  assert.equal(benchmarkMetadata('test', {}, 'http://user:secret@target.test:8080/path?key=secret').target, 'http://target.test:8080')
  assert.equal(benchmarkMetadata('test', {}, 'not-a-url?key=secret').target, 'unknown')
})

test('report promotion requires observed builds, complete declarations, and a passing run', () => {
  process.env.SANDBOX_RELEASE = 'release-42'
  const metadata = benchmarkMetadata('test', {}, 'https://gateway.test')
  assert.ok(provenanceIssues({ passed: true, metadata }).includes('missing observed worker release'))
  metadata.environment = { guest_image_sha256: 'a'.repeat(64), runner_region: 'fixture-region', cache_state: 'cold-destination' }
  metadata.builds = [{ target: metadata.target, endpoint: '/metrics', captured_at: new Date().toISOString(),
    releases: [{ component: 'worker', release: 'release-42' }] }]
  assert.deepEqual(provenanceIssues({ passed: true, metadata }), [])
  assert.ok(provenanceIssues({ passed: true, metadata: { ...metadata, sdk: undefined } }).includes('missing SDK version'))
  assert.ok(provenanceIssues({ passed: false, metadata }).includes('run did not pass'))
  metadata.builds[0]!.releases[0]!.release = 'different-release'
  assert.ok(provenanceIssues({ passed: true, metadata }).includes('observed release does not match declared release'))
  metadata.builds[0]!.error = 'one worker did not answer'
  assert.ok(provenanceIssues({ passed: true, metadata }).includes('incomplete build observation'))
})
