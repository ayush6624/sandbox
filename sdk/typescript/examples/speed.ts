/**
 * Hot creates: every Sandbox.create() is served by cloning a pre-booted
 * golden snapshot on the server, so a fresh microVM is ready in a few
 * hundred milliseconds — fast enough to create one per request.
 *
 * This example times sequential creates, a concurrent burst, and the
 * first command round-trip. Works against a single host or a fleet
 * gateway (the burst spreads across hosts in fleet mode).
 *
 * Run with: npm run example:speed
 */
import { Sandbox } from '../src/index.js'
import { ensureCreds, runExample, step } from './shared.js'

const TTL_MS = 300_000 // safety net: everything auto-destroys in 5 min

async function timed<T>(fn: () => Promise<T>): Promise<[T, number]> {
  const t0 = performance.now()
  const v = await fn()
  return [v, Math.round(performance.now() - t0)]
}

async function main(): Promise<void> {
  ensureCreds()
  const cleanup: Sandbox[] = []

  try {
    step('3 sequential creates (each waits until the sandbox is usable):')
    for (let i = 1; i <= 3; i++) {
      const [sbx, ms] = await timed(() => Sandbox.create({ timeoutMs: TTL_MS }))
      cleanup.push(sbx)
      console.log(`  #${i} ${sbx.sandboxId.slice(0, 8)}  ready in ${ms} ms`)
    }

    step('A burst of 5 concurrent creates:')
    const [burst, burstMs] = await timed(() =>
      Promise.all(Array.from({ length: 5 }, () => Sandbox.create({ timeoutMs: TTL_MS })))
    )
    cleanup.push(...burst)
    console.log(`  5 sandboxes in ${burstMs} ms total (${Math.round(burstMs / 5)} ms amortized)`)

    step('First command in a fresh sandbox (no warm-up needed):')
    const probe = burst[0]
    if (!probe) throw new Error('burst produced no sandboxes')
    const [res, execMs] = await timed(() => probe.commands.run('node --version && uname -r'))
    console.log(`  ${res.stdout.trim().replace('\n', ' / ')}  (round-trip ${execMs} ms)`)
  } finally {
    step(`Killing ${cleanup.length} sandboxes...`)
    await Promise.allSettled(cleanup.map((s) => s.kill()))
  }
}

runExample(main)
