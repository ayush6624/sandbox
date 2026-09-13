import { readFileSync, statSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { parseArgs } from 'node:util'
import { SandboxClient } from '../src/index.js'
import { driveWorkspaceAttempts } from './workspace/attempts.js'
import { canonicalTarget, readAttemptJournal, runDiagnostic, summarizeRun, withAttemptJournal, type RunState } from './workspace/attempts-journal.js'

function positiveInteger(value: string, name: string, maximum = Number.MAX_SAFE_INTEGER): number {
  const parsed = Number(value)
  if (!Number.isSafeInteger(parsed) || parsed <= 0 || parsed > maximum) throw new Error(`${name} must be an integer between 1 and ${maximum}`)
  return parsed
}

function printState(directory: string, state: RunState): void {
  const summary = summarizeRun(state)
  const attempts = state.attempts.map((attempt) => {
    if (attempt.kind !== 'ready') return attempt
    const command = attempt.command.kind === 'completed'
      ? { kind: attempt.command.kind, exitCode: attempt.command.result.exitCode,
        durationMs: attempt.command.result.durationMs,
        stdout: join(directory, `attempt-${attempt.index}.stdout`),
        stderr: join(directory, `attempt-${attempt.index}.stderr`),
        stdoutTruncated: attempt.command.result.stdout.truncated,
        stderrTruncated: attempt.command.result.stderr.truncated }
      : attempt.command
    return { index: attempt.index, kind: attempt.kind, sandboxId: attempt.sandboxId,
      readyAt: attempt.readyAt, resources: attempt.resources, command, cleanup: attempt.cleanup, usage: attempt.usage }
  })
  process.stdout.write(`${JSON.stringify({ ...summary, run: directory, operation: state.operation, attempts, observations: state.observations })}\n`)
  process.exitCode = summary.passed ? 0 : state.phase === 'complete' ? 1 : 2
}

async function main() {
  const { values, positionals } = parseArgs({ allowPositionals: true, options: {
    run: { type: 'string' }, snapshot: { type: 'string' }, count: { type: 'string' },
    'command-file': { type: 'string' }, 'command-timeout-ms': { type: 'string' },
    'observation-timeout-ms': { type: 'string' }, help: { type: 'boolean', short: 'h' },
  } })
  if (values.help) {
    process.stdout.write('Usage: workspace-attempts <start|resume|status|cleanup> --run DIR\n'
      + 'Start: --snapshot ID [--count 2] [--command-file PATH] [--command-timeout-ms 120000]\n'
      + 'Observation: [--observation-timeout-ms 300000]\n')
    return
  }
  const [action] = positionals
  if (positionals.length !== 1 || !['start', 'resume', 'status', 'cleanup'].includes(action ?? '')) {
    throw new Error('Choose start, resume, status, or cleanup')
  }
  if (!values.run) throw new Error('--run DIR is required')
  const directory = resolve(values.run)
  if (action !== 'start' && (values.snapshot !== undefined || values.count !== undefined
    || values['command-file'] !== undefined || values['command-timeout-ms'] !== undefined)) {
    throw new Error('Snapshot, count, and command options are immutable; use them only with start')
  }
  if (action === 'status') {
    printState(directory, readAttemptJournal(directory))
    return
  }
  const targetValue = process.env.SANDBOX_API_URL
  const apiKey = process.env.SANDBOX_API_KEY
  if (!targetValue || !apiKey) throw new Error('Set SANDBOX_API_URL and SANDBOX_API_KEY')
  const target = canonicalTarget(targetValue)
  const observationTimeoutMs = positiveInteger(values['observation-timeout-ms'] ?? '300000', '--observation-timeout-ms', 2_147_483_647)
  const client = new SandboxClient({ baseUrl: target, apiKey, maxRetries: 0, requestTimeoutMs: observationTimeoutMs })
  if (action === 'start') {
    if (!values.snapshot) throw new Error('--snapshot ID is required for start')
    const count = positiveInteger(values.count ?? '2', '--count', 32)
    if (count < 2) throw new Error('--count must be between 2 and 32')
    const timeoutMs = positiveInteger(values['command-timeout-ms'] ?? '120000', '--command-timeout-ms', 2_147_468_647)
    let script = 'python3 /home/sandbox/workspace/check.py'
    if (values['command-file'] !== undefined) {
      if (statSync(values['command-file']).size > 65_536) throw new Error('Command file exceeds 64 KiB')
      script = readFileSync(values['command-file'], 'utf8')
    }
    if (!script || Buffer.byteLength(script) > 65_536) throw new Error('Command must contain between 1 byte and 64 KiB')
    const snapshot = await client.snapshots.get(values.snapshot, AbortSignal.timeout(observationTimeoutMs))
    if (snapshot.state !== 'durable') throw new Error(`Snapshot ${snapshot.id} is ${snapshot.state}; start requires a durable saved snapshot`)
    const state = await withAttemptJournal({ directory, start: {
      target, snapshotId: values.snapshot, count, maxParallelism: Math.min(count, 8), script,
      timeoutMs, ttlMs: 30 * 60_000,
    } }, (journal) => driveWorkspaceAttempts(journal, client, { observationTimeoutMs }))
    printState(directory, state)
    return
  }
  const state = await withAttemptJournal({ directory, target }, (journal) =>
    driveWorkspaceAttempts(journal, client, { cleanup: action === 'cleanup', observationTimeoutMs }))
  printState(directory, state)
}

main().catch((error: unknown) => {
  process.stderr.write(`${JSON.stringify({ error: runDiagnostic(error) })}\n`)
  process.exitCode = 2
})
