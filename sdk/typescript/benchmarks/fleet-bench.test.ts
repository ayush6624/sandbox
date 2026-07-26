import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

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
  assert.match(result.stdout, /--cleanup-timeout-ms 10000/)
})

for (const [name, args, expected] of [
  ['mode', ['--mode', 'fast'], /default\|fsync\|large/],
  ['count', ['--count', '0'], /--count must be a positive integer/],
  ['iterations', ['--iterations', '1.5'], /--iterations must be a positive integer/],
  ['create concurrency', ['--create-concurrency', '-1'], /--create-concurrency must be a positive integer/],
  ['cleanup timeout', ['--cleanup-timeout-ms', 'NaN'], /--cleanup-timeout-ms must be a positive integer/],
] as const) {
  test(`fleet benchmark rejects invalid ${name}`, () => {
    const result = run([...args])
    assert.notEqual(result.status, 0)
    assert.match(result.stderr, expected)
  })
}
