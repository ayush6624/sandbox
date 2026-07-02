/**
 * Sustained load: a fleet of sandboxes each running a real CPU + filesystem
 * workload concurrently, verified for correctness (every workload's output is
 * checked, not just "it didn't crash"). Sized by STRESS_LOAD_N (default 16).
 */

import {
  SuiteDef,
  assert,
  assertEq,
  envInt,
  pool,
  statLine,
  timed,
} from '../harness.js'

export const suite = new SuiteDef('load')

const N = envInt('STRESS_LOAD_N', 16)

/**
 * In-guest workload (plain JS so `node workload.js` needs no build step):
 * - CPU: count primes below 300k with trial division
 * - Disk: write 100 x 32KiB files with per-file content hashes, read them
 *   back, and verify every hash
 * Prints a single JSON line the host asserts on.
 */
const WORKLOAD_JS = `
const fs = require('fs')
const crypto = require('crypto')

function countPrimes(limit) {
  let count = 0
  for (let n = 2; n < limit; n++) {
    let prime = true
    for (let d = 2; d * d <= n; d++) if (n % d === 0) { prime = false; break }
    if (prime) count++
  }
  return count
}

const t0 = Date.now()
const primes = countPrimes(300000)
const cpuMs = Date.now() - t0

const dir = '/home/sandbox/loadwork'
fs.mkdirSync(dir, { recursive: true })
const hashes = []
const t1 = Date.now()
for (let i = 0; i < 100; i++) {
  const buf = crypto.randomBytes(32 * 1024)
  hashes.push(crypto.createHash('sha256').update(buf).digest('hex'))
  fs.writeFileSync(dir + '/f-' + i + '.bin', buf)
}
let ok = 0
for (let i = 0; i < 100; i++) {
  const back = fs.readFileSync(dir + '/f-' + i + '.bin')
  const h = crypto.createHash('sha256').update(back).digest('hex')
  if (h === hashes[i]) ok++
}
const diskMs = Date.now() - t1

console.log(JSON.stringify({ primes, verified: ok, cpuMs, diskMs }))
`

const EXPECTED_PRIMES = 25_997 // π(300000)

suite.test(`${N} sandboxes run a CPU+disk workload concurrently`, async (ctx) => {
  const createTimes: number[] = []
  const sandboxes = await pool(N, Array.from({ length: N }, (_, i) => i), async () => {
    const { result, ms } = await timed(() => ctx.createTracked())
    createTimes.push(ms)
    return result
  })
  ctx.log(statLine('create', createTimes))

  const cpuTimes: number[] = []
  const diskTimes: number[] = []
  const wallTimes: number[] = []
  await pool(N, sandboxes, async (sbx, i) => {
    await sbx.files.write('/home/sandbox/workload.js', WORKLOAD_JS)
    const { result: res, ms } = await timed(() =>
      sbx.commands.run('node /home/sandbox/workload.js', { timeoutMs: 120_000 })
    )
    wallTimes.push(ms)
    const out = JSON.parse(res.stdout.trim()) as {
      primes: number
      verified: number
      cpuMs: number
      diskMs: number
    }
    assertEq(out.primes, EXPECTED_PRIMES, `sandbox #${i} CPU result must be correct`)
    assertEq(out.verified, 100, `sandbox #${i} must verify all 100 file hashes`)
    cpuTimes.push(out.cpuMs)
    diskTimes.push(out.diskMs)
  })

  ctx.log(statLine('workload wall', wallTimes))
  ctx.log(statLine('guest cpu phase', cpuTimes))
  ctx.log(statLine('guest disk phase', diskTimes))

  // All sandboxes must still be healthy after the load.
  await pool(12, sandboxes, async (sbx, i) => {
    const res = await sbx.commands.run('echo still-alive')
    assertEq(res.stdout.trim(), 'still-alive', `sandbox #${i} healthy after load`)
  })
})

suite.test('one sandbox survives a memory-hungry workload', async (ctx) => {
  const sbx = await ctx.createTracked()
  // Allocate ~256MB in the guest, touch it, release, and keep working.
  const res = await sbx.commands.run(
    `node -e '
      const bufs = []
      for (let i = 0; i < 8; i++) { bufs.push(Buffer.alloc(32 * 1024 * 1024, i)) }
      let sum = 0
      for (const b of bufs) sum += b[b.length - 1]
      console.log("touched", sum)
    '`,
    { timeoutMs: 60_000 }
  )
  assert(res.stdout.startsWith('touched'), 'memory workload must complete')
  const after = await sbx.commands.run('echo ok')
  assertEq(after.stdout.trim(), 'ok', 'sandbox must remain responsive after memory pressure')
})

suite.test('disk churn: untar-like many-small-files workload', async (ctx) => {
  const sbx = await ctx.createTracked()
  const res = await sbx.commands.run(
    'mkdir -p /home/sandbox/many && cd /home/sandbox/many && ' +
      'for i in $(seq 1 500); do echo "content-$i" > "f$i.txt"; done && ' +
      'ls | wc -l && cat f250.txt',
    { timeoutMs: 60_000 }
  )
  const [count, sample] = res.stdout.trim().split('\n')
  assertEq(count.trim(), '500', 'all 500 files must be created')
  assertEq(sample.trim(), 'content-250', 'file contents must be intact')
})
