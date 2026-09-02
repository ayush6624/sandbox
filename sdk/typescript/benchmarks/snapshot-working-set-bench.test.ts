import assert from 'node:assert/strict'
import { test } from 'node:test'
import { countOrder, parseArgs } from './snapshot-working-set-bench.js'

test('working-set benchmark defaults exercise the global restore budget', () => {
  const args = parseArgs([])
  assert.deepEqual(args.counts, [1, 4, 8, 16, 24, 32])
  assert.equal(args.maxParallelism, 32)
  assert.equal(args.guestMemoryMiB, undefined)
  assert.equal(args.memoryMiB, 256)
  assert.equal(args.diskMiB, 384)
})

test('parseArgs accepts an explicit guest memory size', () => {
  const args = parseArgs(['--guest-memory-mib', '2048'])
  assert.equal(args.guestMemoryMiB, 2048)
})

test('working-set benchmark alternates count order to reduce cache bias', () => {
  const counts = [1, 4, 8, 16]
  assert.deepEqual(countOrder(counts, 1), [1, 4, 8, 16])
  assert.deepEqual(countOrder(counts, 2), [16, 8, 4, 1])
  assert.deepEqual(counts, [1, 4, 8, 16])
})

test('working-set benchmark validates count and API parallelism limits', () => {
  assert.throws(() => parseArgs(['--counts', '1,0']), /--counts/)
  assert.throws(() => parseArgs(['--max-parallelism', '33']), /--max-parallelism/)
  assert.throws(() => parseArgs(['--rounds', '0']), /--rounds/)
})
