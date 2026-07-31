/**
 * Post-rollout smoke test: proves a freshly rolled fleet can actually serve a
 * sandbox, not merely that its processes started.
 *
 * Covers the REST lifecycle AND the interactive WebSocket path, because the two
 * authenticate and proxy differently — REST sends `Authorization`, the shell
 * sends its credential in the Sec-WebSocket-Protocol list and is upgraded
 * through a different code path on every hop. A REST-only smoke passed happily
 * while `pty.create()` was failing its handshake fleet-wide.
 *
 * Run by rollout.sh; standalone:
 *   SANDBOX_API_URL=... SANDBOX_API_KEY=... npx tsx rollout-smoke.mts
 */
import {
  NotFoundError,
  Sandbox,
} from '../../sdk/typescript/src/index.js'

const apiUrl = process.env.SANDBOX_API_URL
const apiKey = process.env.SANDBOX_API_KEY
if (!apiUrl || !apiKey) {
  console.error('smoke: set SANDBOX_API_URL and SANDBOX_API_KEY')
  process.exit(2)
}

const MARKER = `SMOKE_${Math.floor(Date.now() / 1000)}`
let failures = 0
const ok = (label: string, detail = '') =>
  console.log(`    ✓ ${label}${detail ? ` — ${detail}` : ''}`)
const bad = (label: string, detail: string) => {
  failures++
  console.log(`    ✗ ${label} — ${detail}`)
}

let sbx: Sandbox | undefined
try {
  const t0 = Date.now()
  sbx = await Sandbox.create({ apiUrl, apiKey })
  ok('create', `${sbx.sandboxId.slice(0, 8)} in ${Date.now() - t0}ms`)

  // 1. REST exec.
  const res = await sbx.commands.run(`echo ${MARKER}`)
  res.stdout.includes(MARKER)
    ? ok('exec', `exit ${res.exitCode}`)
    : bad('exec', `no marker in stdout: ${JSON.stringify(res.stdout)}`)

  // 2. Interactive PTY: connect, run, exit cleanly.
  let out = ''
  const tPty = Date.now()
  const pty = await sbx.pty.create({
    cols: 100,
    rows: 30,
    onData: (d) => {
      out += new TextDecoder().decode(d)
    },
  })
  ok('pty connect', `${Date.now() - tPty}ms`)

  pty.sendInput(`echo ${MARKER}_PTY\n`)
  const deadline = Date.now() + 20_000
  while (!out.includes(`${MARKER}_PTY`) && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 200))
  }
  if (out.includes(`${MARKER}_PTY`)) {
    ok('pty shell', 'command echoed')
  } else {
    bad('pty shell', `no marker after 20s; got ${JSON.stringify(out.slice(-200))}`)
  }
  pty.resize({ cols: 140, rows: 45 })
  pty.sendInput('exit\n')
  try {
    ok('pty exit', `code ${await pty.exited}`)
  } catch (err) {
    bad('pty exit', (err as Error).message)
  }
} catch (err) {
  bad('lifecycle', `${(err as Error).constructor.name}: ${(err as Error).message}`)
} finally {
  if (sbx) {
    try {
      await sbx.kill()
      ok('destroy', sbx.sandboxId.slice(0, 8))
    } catch (err) {
      bad('destroy', (err as Error).message)
    }
  }
}

// 3. WebSocket errors must arrive as 4xxx close frames, not an opaque 1006.
// This is a real regression class: wsutil.Reject silently lost the ability to
// hijack behind the request-instrumentation middleware, and every WS error on
// the fleet degraded to 1006 with no signal anywhere else.
//
// Skipped in fast mode: it has to wait out a heartbeat interval for the gateway
// to drop the dead id, which alone is ~8s — most of a fast rollout's budget. The
// checks above already exercise the same auth and upgrade path end to end.
if (sbx && process.env.SMOKE_MODE !== 'fast') {
  await new Promise((r) => setTimeout(r, 8000)) // let routing drop the dead id
  const classify = (err: unknown) => {
    const e = err as Error
    if (e instanceof NotFoundError) return ok('ws error channel', 'deleted id -> 4404')
    if (/1006/.test(e.message)) return bad('ws error channel', 'degraded to an opaque 1006')
    return ok('ws error channel', `typed: ${e.constructor.name}`)
  }
  try {
    const p = await sbx.pty.create({ onData: () => {}, connectTimeoutMs: 15_000 })
    // create() resolves after a 100ms grace window, so a slower close frame
    // lands on `exited` instead of rejecting the create.
    try {
      await p.exited
      bad('ws error channel', 'no error delivered for a deleted sandbox')
    } catch (err) {
      classify(err)
    }
  } catch (err) {
    classify(err)
  }
}

if (failures > 0) {
  console.log(`\n    smoke: ${failures} check(s) failed`)
  process.exit(1)
}
console.log('\n    smoke: all checks passed')
