/**
 * Exec semantics: exit codes, output capture, envs/cwd, timeouts and
 * process-group kills, the 2 MiB output cap, and streaming (NDJSON) parity
 * with the buffered path. One sandbox is shared across all tests — that also
 * exercises many sequential execs against a single agent.
 */

import type { Sandbox } from '../../sdk/typescript/src/index.js'
import { SuiteDef, assert, assertEq, assertThrows, statLine } from '../harness.js'
import type { Ctx } from '../harness.js'

export const suite = new SuiteDef('exec')

const MAX_OUTPUT = 2 * 1024 * 1024 // agentapi.MaxOutputBytes, per stream

let shared: Sandbox | undefined
async function sandbox(ctx: Ctx): Promise<Sandbox> {
  if (!shared) shared = await ctx.createTracked()
  return shared
}

suite.test('captures stdout, stderr, and exit code 0', async (ctx) => {
  const sbx = await sandbox(ctx)
  const res = await sbx.commands.run('echo out; echo err >&2')
  assertEq(res.stdout.trim(), 'out', 'stdout')
  assertEq(res.stderr.trim(), 'err', 'stderr')
  assertEq(res.exitCode, 0, 'exit code')
  assert(res.durationMs >= 0, 'durationMs must be reported')
})

suite.test('non-zero exit throws CommandExitError with the full result', async (ctx) => {
  const sbx = await sandbox(ctx)
  const err = (await assertThrows(
    () => sbx.commands.run('echo partial; echo oops >&2; exit 42'),
    'CommandExitError',
    'non-zero exit'
  )) as import('../../sdk/typescript/src/index.js').CommandExitError
  assertEq(err.exitCode, 42, 'exit code carried on the error')
  assertEq(err.stdout.trim(), 'partial', 'stdout carried on the error')
  assertEq(err.stderr.trim(), 'oops', 'stderr carried on the error')
})

suite.test('envs are passed through', async (ctx) => {
  const sbx = await sandbox(ctx)
  const res = await sbx.commands.run('printf "%s|%s" "$FOO" "$EMPTY_CHECK"', {
    envs: { FOO: 'bar baz  qux', EMPTY_CHECK: '' },
  })
  assertEq(res.stdout, 'bar baz  qux|', 'env values incl. spaces must round-trip')
})

suite.test('cwd defaults to /home/sandbox/app and can be overridden', async (ctx) => {
  const sbx = await sandbox(ctx)
  const def = await sbx.commands.run('pwd')
  assertEq(def.stdout.trim(), '/home/sandbox/app', 'default cwd')
  const tmp = await sbx.commands.run('pwd', { cwd: '/tmp' })
  assertEq(tmp.stdout.trim(), '/tmp', 'cwd override')
})

suite.test('unicode and multi-line output round-trips', async (ctx) => {
  const sbx = await sandbox(ctx)
  const res = await sbx.commands.run(`printf 'line1\\nline2\\n\\u00e9\\u00fc \\u4f60\\u597d 🚀\\n'`)
  assertEq(res.stdout, 'line1\nline2\néü 你好 🚀\n', 'multi-line unicode output')
})

suite.test('guest toolchain present: node 22, python 3, git, pnpm, tsc', async (ctx) => {
  const sbx = await sandbox(ctx)
  const res = await sbx.commands.run(
    'node --version && python3 --version && git --version && pnpm --version && tsc --version'
  )
  assert(/^v22\./m.test(res.stdout), `expected node v22, got:\n${res.stdout}`)
  assert(/Python 3\./.test(res.stdout), 'python3 present')
  assert(/git version/.test(res.stdout), 'git present')
  assert(/Version \d/.test(res.stdout), 'tsc present')
})

suite.test('timeout kills the command and throws TimeoutError quickly', async (ctx) => {
  const sbx = await sandbox(ctx)
  const start = performance.now()
  await assertThrows(
    () => sbx.commands.run('sleep 333', { timeoutMs: 3_000 }),
    'TimeoutError',
    'command exceeding its budget'
  )
  const elapsed = performance.now() - start
  assert(elapsed < 15_000, `timeout must return promptly, took ${Math.round(elapsed)}ms`)
})

suite.test('timeout kills the whole process group, not just the shell', async (ctx) => {
  const sbx = await sandbox(ctx)
  await assertThrows(
    () => sbx.commands.run('sleep 444 & sleep 444 & wait', { timeoutMs: 2_000 }),
    'TimeoutError',
    'process-group command exceeding its budget'
  )
  // Bracket pattern so pgrep doesn't match the shell running this very check.
  const check = await sbx.commands.run('pgrep -f "[s]leep 444" | wc -l')
  assertEq(check.stdout.trim(), '0', 'background children must not survive the timeout kill')
})

suite.test('backgrounded processes let exec return immediately', async (ctx) => {
  const sbx = await sandbox(ctx)
  const res = await sbx.commands.run('nohup sleep 555 >/dev/null 2>&1 & echo started')
  assertEq(res.stdout.trim(), 'started', 'shell must exit without waiting for the daemon')
  assert(res.durationMs < 5_000, `exec must return when the shell exits, took ${res.durationMs}ms`)
  const check = await sbx.commands.run('pgrep -f "[s]leep 555" | wc -l')
  assertEq(check.stdout.trim(), '1', 'the backgrounded process must still be running')
  await sbx.commands.run('pkill -f "[s]leep 555" || true')
})

suite.test('stdout is capped at 2 MiB per stream', async (ctx) => {
  const sbx = await sandbox(ctx)
  const res = await sbx.commands.run(
    `node -e 'process.stdout.write("x".repeat(3 * 1024 * 1024))'`,
    { timeoutMs: 60_000 }
  )
  // sandboxd keeps exactly MaxOutputBytes and appends a "\n... [N bytes
  // truncated]" marker, so the payload is the cap plus a short suffix.
  assert(
    res.stdout.length <= MAX_OUTPUT + 64,
    `stdout must be capped near 2MiB, got ${res.stdout.length}`
  )
  assert(/\[\d+ bytes truncated\]$/.test(res.stdout), 'truncation marker must be present')
  const payload = res.stdout.slice(0, MAX_OUTPUT)
  assert(/^x+$/.test(payload.slice(0, 1024)) && payload.length === MAX_OUTPUT,
    'the first 2MiB of real output must be retained')
})

suite.test('streaming: chunks arrive incrementally and match the final result', async (ctx) => {
  const sbx = await sandbox(ctx)
  const chunks: string[] = []
  let stderrSeen = ''
  const res = await sbx.commands.run(
    `node -e '
      let i = 0
      const t = setInterval(() => {
        process.stdout.write("chunk-" + i + "\\n")
        if (i === 2) process.stderr.write("mid-stream-err\\n")
        if (++i >= 5) { clearInterval(t) }
      }, 250)
    '`,
    {
      timeoutMs: 30_000,
      onStdout: (d) => chunks.push(d),
      onStderr: (d) => (stderrSeen += d),
    }
  )
  assert(chunks.length >= 2, `expected multiple stdout chunks, got ${chunks.length}`)
  assertEq(chunks.join(''), res.stdout, 'accumulated chunks must equal result.stdout')
  assertEq(res.stdout, 'chunk-0\nchunk-1\nchunk-2\nchunk-3\nchunk-4\n', 'full streamed output')
  assertEq(stderrSeen, 'mid-stream-err\n', 'stderr must stream on its own channel')
  assertEq(res.exitCode, 0, 'exit code')
  ctx.log(`stdout arrived in ${chunks.length} chunk(s)`)
})

suite.test('streaming: non-zero exit still throws CommandExitError', async (ctx) => {
  const sbx = await sandbox(ctx)
  let streamed = ''
  const err = (await assertThrows(
    () =>
      sbx.commands.run('echo before-failure; exit 7', {
        onStdout: (d) => (streamed += d),
      }),
    'CommandExitError',
    'streaming non-zero exit'
  )) as import('../../sdk/typescript/src/index.js').CommandExitError
  assertEq(err.exitCode, 7, 'exit code via streaming path')
  assertEq(streamed.trim(), 'before-failure', 'output streamed before the failure')
  assertEq(err.stdout.trim(), 'before-failure', 'error carries accumulated stdout')
})

suite.test('streaming: timeout mid-stream throws TimeoutError', async (ctx) => {
  const sbx = await sandbox(ctx)
  let sawOutput = false
  await assertThrows(
    () =>
      sbx.commands.run('echo tick; sleep 666', {
        timeoutMs: 3_000,
        onStdout: () => (sawOutput = true),
      }),
    'TimeoutError',
    'streaming command exceeding its budget'
  )
  assert(sawOutput, 'output produced before the timeout must have streamed')
  const check = await sbx.commands.run('pgrep -f "[s]leep 666" | wc -l')
  assertEq(check.stdout.trim(), '0', 'timed-out streaming command must be killed')
})

suite.test('50 sequential execs on one sandbox stay fast and correct', async (ctx) => {
  const sbx = await sandbox(ctx)
  const times: number[] = []
  for (let i = 0; i < 50; i++) {
    const start = performance.now()
    const res = await sbx.commands.run(`echo ${i}`)
    times.push(performance.now() - start)
    assertEq(res.stdout.trim(), String(i), `exec #${i} output`)
  }
  ctx.log(statLine('sequential exec', times))
})
