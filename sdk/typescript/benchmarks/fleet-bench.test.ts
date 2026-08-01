import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'
import { AuthenticationError } from '../src/index.js'
import { fleetClient } from './fleet-bench.js'

const SCRIPT = fileURLToPath(new URL('./fleet-bench.ts', import.meta.url))

function run(args: string[]) {
  return spawnSync(process.execPath, ['--import', 'tsx', SCRIPT, ...args], {
    cwd: fileURLToPath(new URL('..', import.meta.url)),
    encoding: 'utf8',
  })
}

test('fleet benchmark documents safe arguments without requiring credentials', () => {
  const result = run(['--help'])
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stdout, /--request-timeout-ms 120000/)
  assert.match(result.stdout, /--cleanup-timeout-ms 10000/)
})

// Host inventory is operator-gated (worker-control credential), so the benchmark
// samples it through the SDK's operator client — never with the tenant API key,
// which the gateway answers 401 for.
test('fleet inventory demands the operator credential, not the tenant key', () => {
  const previousControl = process.env.SANDBOX_CONTROL_KEY
  delete process.env.SANDBOX_CONTROL_KEY
  process.env.SANDBOX_API_URL = 'https://gateway.example'
  process.env.SANDBOX_API_KEY = 'tenant-key'
  try {
    assert.throws(() => fleetClient(), (err: unknown) => {
      assert.ok(err instanceof AuthenticationError)
      assert.match(String((err as Error).message), /SANDBOX_CONTROL_KEY/)
      return true
    })
    process.env.SANDBOX_CONTROL_KEY = 'control-key'
    assert.doesNotThrow(() => fleetClient())
  } finally {
    delete process.env.SANDBOX_API_URL
    delete process.env.SANDBOX_API_KEY
    if (previousControl === undefined) delete process.env.SANDBOX_CONTROL_KEY
    else process.env.SANDBOX_CONTROL_KEY = previousControl
  }
})

for (const [name, args, expected] of [
  ['mode', ['--mode', 'fast'], /default\|fsync\|large/],
  ['count', ['--count', '0'], /--count must be a positive integer/],
  ['iterations', ['--iterations', '1.5'], /--iterations must be a positive integer/],
  ['create concurrency', ['--create-concurrency', '-1'], /--create-concurrency must be a positive integer/],
  ['request timeout', ['--request-timeout-ms', '0'], /--request-timeout-ms must be a positive integer/],
  ['cleanup timeout', ['--cleanup-timeout-ms', 'NaN'], /--cleanup-timeout-ms must be a positive integer/],
] as const) {
  test(`fleet benchmark rejects invalid ${name}`, () => {
    const result = run([...args])
    assert.notEqual(result.status, 0)
    assert.match(result.stderr, expected)
  })
}
