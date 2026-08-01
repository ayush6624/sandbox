/**
 * PTY / WebSocket stress against a live fleet.
 *
 * The host→guest shell hop is an httputil.ReverseProxy, and it now shares the
 * REST path's sandbox-keyed connection pool. This drives the two shapes that
 * pool has to survive:
 *
 *   1. many concurrent shells (fan-out across sandboxes and within one), and
 *   2. shells opened on sandboxes created AFTER a churn round, so their guest
 *      IPs are recycled addresses whose previous owners are dead — the exact
 *      condition that made exec/file writes fail with "connection reset by
 *      peer" when the pool was keyed on the IP alone.
 *
 *   SANDBOX_API_URL=... SANDBOX_API_KEY=... npx tsx pty-stress.ts
 */
import { Sandbox } from '../sdk/typescript/src/index.js'

const ROUNDS = Number(process.env.PTY_ROUNDS ?? 3)
const PER_ROUND = Number(process.env.PTY_PER_ROUND ?? 8)
const SHELLS_PER_SANDBOX = Number(process.env.PTY_SHELLS_PER_SANDBOX ?? 2)

interface Failure {
  where: string
  error: string
}

const failures: Failure[] = []
const openMs: number[] = []

function pct(xs: number[], p: number): number {
  if (xs.length === 0) return 0
  const s = [...xs].sort((a, b) => a - b)
  return s[Math.min(s.length - 1, Math.floor((p / 100) * s.length))]
}

/** Opens a shell, runs a marker command, and asserts the marker echoes back. */
async function shellRoundTrip(sbx: Sandbox, tag: string): Promise<void> {
  const marker = `PTY_OK_${Math.random().toString(36).slice(2, 10)}`
  let seen = ''
  const t0 = Date.now()
  const pty = await sbx.pty.create({
    cols: 100,
    rows: 30,
    onData: (data) => {
      seen += Buffer.from(data).toString('utf8')
    },
  })
  openMs.push(Date.now() - t0)
  try {
    pty.resize({ cols: 120, rows: 40 })
    pty.sendInput(`echo ${marker}\n`)
    const deadline = Date.now() + 20_000
    while (!seen.includes(marker) && Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 50))
    }
    if (!seen.includes(marker)) {
      throw new Error(`marker never echoed (got ${seen.length} bytes)`)
    }
    pty.sendInput('exit\n')
    const code = await Promise.race([
      pty.exited,
      new Promise<number>((_, rej) => setTimeout(() => rej(new Error('exit timed out')), 15_000)),
    ])
    if (code !== 0) throw new Error(`shell exited ${code}, want 0`)
  } finally {
    try {
      pty.kill()
    } catch {
      /* already closed */
    }
  }
  void tag
}

async function main(): Promise<void> {
  console.log(
    `PTY stress: ${ROUNDS} rounds x ${PER_ROUND} sandboxes x ${SHELLS_PER_SANDBOX} shells ` +
      `(${ROUNDS * PER_ROUND * SHELLS_PER_SANDBOX} shells total)`
  )
  for (let round = 1; round <= ROUNDS; round++) {
    const t0 = Date.now()
    const boxes = await Promise.all(Array.from({ length: PER_ROUND }, () => Sandbox.create()))
    try {
      // Concurrent shells, several per sandbox.
      const work: Promise<void>[] = []
      for (const [i, sbx] of boxes.entries()) {
        for (let s = 0; s < SHELLS_PER_SANDBOX; s++) {
          work.push(
            shellRoundTrip(sbx, `r${round}/b${i}/s${s}`).catch((e: unknown) => {
              failures.push({
                where: `round ${round} box ${i} shell ${s}`,
                error: e instanceof Error ? e.message : String(e),
              })
            })
          )
        }
      }
      await Promise.all(work)

      // Interleave exec + files against the same sandboxes: they share the
      // pool with the shells, and this is what regressed before.
      await Promise.all(
        boxes.map(async (sbx, i) => {
          try {
            const r = await sbx.commands.run('echo exec_ok')
            if (!r.stdout.includes('exec_ok')) throw new Error(`bad stdout: ${r.stdout}`)
            await sbx.files.write('/home/sandbox/pty-stress.txt', 'x'.repeat(4096))
            const back = await sbx.files.read('/home/sandbox/pty-stress.txt')
            if (back.length !== 4096) throw new Error(`readback ${back.length} != 4096`)
          } catch (e: unknown) {
            failures.push({
              where: `round ${round} box ${i} exec/files`,
              error: e instanceof Error ? e.message : String(e),
            })
          }
        })
      )
    } finally {
      // Kill them all, so the NEXT round's sandboxes inherit these guest IPs.
      await Promise.all(
        boxes.map((s) =>
          s.kill().catch((e: unknown) => {
            failures.push({
              where: `round ${round} kill`,
              error: e instanceof Error ? e.message : String(e),
            })
          })
        )
      )
    }
    console.log(
      `  round ${round}/${ROUNDS}: ${PER_ROUND} sandboxes, ` +
        `${PER_ROUND * SHELLS_PER_SANDBOX} shells in ${Date.now() - t0}ms ` +
        `(failures so far: ${failures.length})`
    )
  }

  console.log(
    `\nshell open latency: p50=${pct(openMs, 50)}ms p95=${pct(openMs, 95)}ms ` +
      `max=${Math.max(...openMs)}ms n=${openMs.length}`
  )
  if (failures.length > 0) {
    console.log(`\nFAILURES (${failures.length}):`)
    for (const f of failures.slice(0, 20)) console.log(`  ${f.where}: ${f.error}`)
    process.exit(1)
  }
  console.log('\nPTY stress passed: no failures')
}

main().catch((e: unknown) => {
  console.error('fatal:', e)
  process.exit(1)
})
