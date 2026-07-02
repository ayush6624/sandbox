/**
 * Minimal test harness for the end-to-end suites. No framework — each suite
 * registers named tests, the runner executes them sequentially, and every
 * sandbox created through the harness is tracked and killed when the suite
 * ends (pass or fail), so a crashed run doesn't leak VMs on the fleet.
 */

import { Sandbox } from '../sdk/typescript/src/index.js'
import type { SandboxCreateOpts } from '../sdk/typescript/src/index.js'

// ---------------------------------------------------------------------------
// Assertions

export class AssertionError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'AssertionError'
  }
}

export function assert(cond: unknown, msg: string): asserts cond {
  if (!cond) throw new AssertionError(msg)
}

export function assertEq<T>(actual: T, expected: T, msg: string): void {
  if (actual !== expected) {
    throw new AssertionError(`${msg}\n  expected: ${JSON.stringify(expected)}\n  actual:   ${JSON.stringify(actual)}`)
  }
}

/** Asserts that `fn` rejects with an error whose `name` matches. */
export async function assertThrows(
  fn: () => Promise<unknown>,
  errorName: string,
  msg: string
): Promise<Error> {
  try {
    await fn()
  } catch (err) {
    const e = err as Error
    if (e.name === errorName) return e
    throw new AssertionError(`${msg}\n  expected error ${errorName}, got ${e.name}: ${e.message}`)
  }
  throw new AssertionError(`${msg}\n  expected error ${errorName}, but nothing was thrown`)
}

// ---------------------------------------------------------------------------
// Timing / stats

export function percentile(values: number[], p: number): number {
  if (values.length === 0) return NaN
  const sorted = [...values].sort((a, b) => a - b)
  const idx = Math.min(sorted.length - 1, Math.ceil((p / 100) * sorted.length) - 1)
  return sorted[Math.max(0, idx)]
}

export function statLine(label: string, values: number[]): string {
  const ms = (n: number) => `${Math.round(n)}ms`
  return (
    `${label}: n=${values.length} ` +
    `p50=${ms(percentile(values, 50))} p95=${ms(percentile(values, 95))} ` +
    `min=${ms(Math.min(...values))} max=${ms(Math.max(...values))}`
  )
}

export async function timed<T>(fn: () => Promise<T>): Promise<{ result: T; ms: number }> {
  const start = performance.now()
  const result = await fn()
  return { result, ms: performance.now() - start }
}

export function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms))
}

/** Polls `fn` until it returns truthy or the deadline passes. */
export async function eventually<T>(
  fn: () => Promise<T | undefined | false>,
  opts: { timeoutMs: number; intervalMs?: number; what: string }
): Promise<T> {
  const deadline = Date.now() + opts.timeoutMs
  let lastErr: unknown
  while (Date.now() < deadline) {
    try {
      const v = await fn()
      if (v) return v
    } catch (err) {
      lastErr = err
    }
    await sleep(opts.intervalMs ?? 500)
  }
  throw new AssertionError(
    `timed out after ${opts.timeoutMs}ms waiting for: ${opts.what}` +
      (lastErr ? `\n  last error: ${(lastErr as Error).message}` : '')
  )
}

/** Runs `fn` over `items` with at most `limit` in flight. Rejections propagate. */
export async function pool<T, R>(
  limit: number,
  items: readonly T[],
  fn: (item: T, index: number) => Promise<R>
): Promise<R[]> {
  const results: R[] = new Array(items.length)
  let next = 0
  const workers = Array.from({ length: Math.min(limit, items.length) }, async () => {
    while (next < items.length) {
      const i = next++
      results[i] = await fn(items[i], i)
    }
  })
  await Promise.all(workers)
  return results
}

// ---------------------------------------------------------------------------
// Suite definition + context

export interface TestResult {
  suite: string
  name: string
  ok: boolean
  skipped?: boolean
  ms: number
  error?: string
}

export interface Ctx {
  /** Creates a sandbox that the harness kills at the end of the suite. */
  createTracked(opts?: SandboxCreateOpts): Promise<Sandbox>
  /** Registers an externally created sandbox for end-of-suite cleanup. */
  track(sbx: Sandbox): Sandbox
  /** Unregisters a sandbox the test already killed itself. */
  untrack(sbx: Sandbox): void
  /** Prints an indented informational line (metrics, distributions, ...). */
  log(msg: string): void
}

interface TestDef {
  name: string
  fn: (ctx: Ctx) => Promise<void>
}

export class SuiteDef {
  readonly tests: TestDef[] = []
  constructor(readonly name: string) {}

  test(name: string, fn: (ctx: Ctx) => Promise<void>): void {
    this.tests.push({ name, fn })
  }
}

export class SkipSuite extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'SkipSuite'
  }
}

// ---------------------------------------------------------------------------
// Runner

const tracked = new Map<string, Sandbox>()

async function killTracked(): Promise<void> {
  const toKill = [...tracked.values()]
  tracked.clear()
  await pool(16, toKill, async (sbx) => {
    try {
      await sbx.kill()
    } catch {
      // Already gone (test killed it, or TTL reaped it) — that's fine.
    }
  })
}

// Best-effort sweep if the process is interrupted mid-run.
process.on('SIGINT', () => {
  console.error(`\nInterrupted — killing ${tracked.size} tracked sandbox(es)...`)
  void killTracked().finally(() => process.exit(130))
})

export async function runSuite(suite: SuiteDef): Promise<TestResult[]> {
  console.log(`\n== ${suite.name} ==`)
  const results: TestResult[] = []

  const ctx: Ctx = {
    async createTracked(opts?: SandboxCreateOpts) {
      const sbx = await Sandbox.create(opts)
      tracked.set(sbx.sandboxId, sbx)
      return sbx
    },
    track(sbx: Sandbox) {
      tracked.set(sbx.sandboxId, sbx)
      return sbx
    },
    untrack(sbx: Sandbox) {
      tracked.delete(sbx.sandboxId)
    },
    log(msg: string) {
      console.log(`      ${msg}`)
    },
  }

  for (const t of suite.tests) {
    const start = performance.now()
    try {
      await t.fn(ctx)
      const ms = performance.now() - start
      results.push({ suite: suite.name, name: t.name, ok: true, ms })
      console.log(`  PASS ${t.name} (${Math.round(ms)}ms)`)
    } catch (err) {
      const ms = performance.now() - start
      if (err instanceof SkipSuite) {
        results.push({ suite: suite.name, name: t.name, ok: true, skipped: true, ms })
        console.log(`  SKIP ${t.name} — ${err.message}`)
        continue
      }
      const e = err as Error
      results.push({ suite: suite.name, name: t.name, ok: false, ms, error: `${e.name}: ${e.message}` })
      console.log(`  FAIL ${t.name} (${Math.round(ms)}ms)`)
      console.log(`       ${e.name}: ${e.message.split('\n').join('\n       ')}`)
    }
  }

  await killTracked()
  return results
}

// ---------------------------------------------------------------------------
// Environment

export function requireEnv(name: string): string {
  const v = process.env[name]
  if (!v) {
    console.error(
      `Missing required environment variable ${name}.\n` +
        `  export SANDBOX_API_URL=http://<gateway-or-host>:<port>\n` +
        `  export SANDBOX_API_KEY=<token>`
    )
    process.exit(1)
  }
  return v
}

export function envInt(name: string, fallback: number): number {
  const v = process.env[name]
  if (!v) return fallback
  const n = Number.parseInt(v, 10)
  if (!Number.isInteger(n) || n < 1) {
    console.error(`${name} must be a positive integer, got ${JSON.stringify(v)}`)
    process.exit(1)
  }
  return n
}
