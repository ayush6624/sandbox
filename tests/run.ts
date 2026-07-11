/**
 * Test runner. Runs every suite (or just the ones named on the command line)
 * sequentially, prints a summary, writes a JSON report to results/, and exits
 * non-zero if anything failed.
 *
 *   npx tsx run.ts                 # everything
 *   npx tsx run.ts exec files      # a subset
 *
 * Required env: SANDBOX_API_URL, SANDBOX_API_KEY (gateway or host).
 * Optional env: SANDBOX_HOST_URL, SANDBOX_HOST_KEY (host-direct, enables the
 * snapshots suite), STRESS_BURST, STRESS_LOAD_N, STRESS_FANOUT_N,
 * STRESS_CHURN_CYCLES / _ROUNDS / _BATCH.
 */

import { mkdirSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { requireEnv, runSuite } from './harness.js'
import type { SuiteDef, TestResult } from './harness.js'

import { suite as lifecycle } from './suites/lifecycle.js'
import { suite as exec } from './suites/exec.js'
import { suite as files } from './suites/files.js'
import { suite as ports } from './suites/ports.js'
import { suite as concurrency } from './suites/concurrency.js'
import { suite as churn } from './suites/churn.js'
import { suite as load } from './suites/load.js'
import { suite as snapshots } from './suites/snapshots.js'
import { suite as hibernate } from './suites/hibernate.js'
import { suite as clock } from './suites/clock.js'

const ALL: SuiteDef[] = [lifecycle, exec, files, ports, concurrency, churn, load, snapshots, hibernate, clock]

async function main(): Promise<void> {
  requireEnv('SANDBOX_API_URL')
  requireEnv('SANDBOX_API_KEY')

  const wanted = process.argv.slice(2)
  const unknown = wanted.filter((w) => !ALL.some((s) => s.name === w))
  if (unknown.length > 0) {
    console.error(
      `Unknown suite(s): ${unknown.join(', ')}\nAvailable: ${ALL.map((s) => s.name).join(', ')}`
    )
    process.exit(1)
  }
  const suites = wanted.length > 0 ? ALL.filter((s) => wanted.includes(s.name)) : ALL

  console.log(`Target: ${process.env.SANDBOX_API_URL}`)
  console.log(`Suites: ${suites.map((s) => s.name).join(', ')}`)

  const started = Date.now()
  const results: TestResult[] = []
  for (const suite of suites) {
    results.push(...(await runSuite(suite)))
  }
  const totalMs = Date.now() - started

  const passed = results.filter((r) => r.ok && !r.skipped)
  const skipped = results.filter((r) => r.skipped)
  const failed = results.filter((r) => !r.ok)

  console.log(`\n${'='.repeat(60)}`)
  console.log(
    `${passed.length} passed, ${failed.length} failed, ${skipped.length} skipped ` +
      `in ${(totalMs / 1000).toFixed(1)}s`
  )
  for (const f of failed) {
    console.log(`  FAIL ${f.suite} :: ${f.name}`)
    if (f.error) console.log(`       ${f.error.split('\n')[0]}`)
  }

  const here = dirname(fileURLToPath(import.meta.url))
  const outDir = join(here, 'results')
  mkdirSync(outDir, { recursive: true })
  const stamp = new Date().toISOString().replace(/[:.]/g, '-')
  const outPath = join(outDir, `run_${stamp}.json`)
  writeFileSync(
    outPath,
    JSON.stringify(
      {
        target: process.env.SANDBOX_API_URL,
        startedAt: new Date(started).toISOString(),
        totalMs,
        summary: { passed: passed.length, failed: failed.length, skipped: skipped.length },
        results,
      },
      null,
      2
    )
  )
  console.log(`Report: ${outPath}`)

  process.exit(failed.length > 0 ? 1 : 0)
}

main().catch((err) => {
  console.error('Runner crashed:', err)
  process.exit(1)
})
