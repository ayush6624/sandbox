import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { createHash } from 'node:crypto'
import { mkdtempSync, mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test, type TestContext } from 'node:test'
import { fileURLToPath } from 'node:url'
import {
  isJournalError, readAttemptJournal, summarizeRun, withAttemptJournal,
  type AttemptJournal, type ReadyAttempt, type RunState, type StartAttempts,
} from './attempts-journal.js'
import type { CommandOutput, CompletedCommandResult } from './command-receipt.js'

const start: StartAttempts = {
  target: 'http://127.0.0.1:8123', snapshotId: 'saved-workspace', count: 2,
  maxParallelism: 2, script: 'printf result', timeoutMs: 10_000, ttlMs: 600_000,
}

function directory(t: TestContext): string {
  const parent = mkdtempSync(join(tmpdir(), 'workspace-journal-'))
  t.after(() => rmSync(parent, { recursive: true, force: true }))
  return join(parent, 'run')
}

function output(value: string): CommandOutput {
  const data = Buffer.from(value)
  return { base64: data.toString('base64'), bytes: data.length,
    sha256: createHash('sha256').update(data).digest('hex'), truncated: false }
}

function completed(): CompletedCommandResult {
  return { exitCode: 0, durationMs: 12, stdout: output('saved result\n'), stderr: output('diagnostic\n') }
}

function ready(index: number): ReadyAttempt {
  return { index, kind: 'ready', sandboxId: `sandbox-${index}`, readyAt: new Date().toISOString(),
    resources: { vcpus: 1, memoryMib: 256 }, command: { kind: 'completed', result: completed() },
    cleanup: 'retained', usage: { kind: 'unobserved' } }
}

function recordCompletion(journal: AttemptJournal): void {
  journal.commit((draft) => {
    draft.operation = { kind: 'accepted', id: 'batch-operation', done: true }
    draft.attempts = [ready(0), { index: 1, kind: 'create-failed', error: { message: 'capacity exhausted' } }]
  })
}

function childOwner(runDirectory: string, mode: 'hold' | 'probe') {
  const child = spawn(process.execPath, ['--import', 'tsx', '--input-type=module', '--eval', `
    import { withAttemptJournal } from ${JSON.stringify(new URL('./attempts-journal.ts', import.meta.url).href)};
    try {
      await withAttemptJournal({ directory: process.env.WORKSPACE_JOURNAL_DIRECTORY }, async () => {
        process.stdout.write('owned\\n');
        if (process.env.WORKSPACE_JOURNAL_MODE === 'hold') await new Promise(() => {});
      });
    } catch (error) {
      process.stderr.write(String(error) + '\\n');
      process.exitCode = 2;
    }
  `], {
    cwd: fileURLToPath(new URL('../../', import.meta.url)),
    env: { ...process.env, WORKSPACE_JOURNAL_DIRECTORY: runDirectory, WORKSPACE_JOURNAL_MODE: mode },
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  let stdout = '', stderr = ''
  let becameReady: () => void = () => {}
  let readinessFailed: (error: Error) => void = () => {}
  const ready = new Promise<void>((resolve, reject) => { becameReady = resolve; readinessFailed = reject })
  child.stdout.on('data', (data) => {
    stdout += String(data)
    if (stdout.includes('owned\n')) becameReady()
  })
  child.stderr.on('data', (data) => { stderr += String(data) })
  const done = new Promise<{ code: number | null; signal: NodeJS.Signals | null; stdout: string; stderr: string }>((resolve, reject) => {
    child.once('error', (error) => { readinessFailed(error); reject(error) })
    child.once('close', (code, signal) => {
      if (!stdout.includes('owned\n')) readinessFailed(new Error(`Owner never acquired lock: ${stderr}`))
      resolve({ code, signal, stdout, stderr })
    })
  })
  // Probes intentionally exit without ownership when busy.
  void ready.catch(() => {})
  return { child, ready, done }
}

test('run ownership excludes another process and survives until SIGKILL', { timeout: 20_000 }, async (t) => {
  const runDirectory = directory(t)
  await withAttemptJournal({ directory: runDirectory, start }, async () => {})
  const owner = childOwner(runDirectory, 'hold')
  t.after(async () => { owner.child.kill('SIGKILL'); await owner.done })
  await owner.ready
  const contender = childOwner(runDirectory, 'probe')
  t.after(async () => { contender.child.kill('SIGKILL'); await contender.done })
  const rejected = await contender.done
  assert.equal(rejected.code, 2)
  assert.match(rejected.stderr, /Workspace run is busy/)
  assert.equal(rejected.stdout, '')
  // Read-only status remains available while a different process owns mutation.
  assert.equal(readAttemptJournal(runDirectory).intent.count, 2)
  owner.child.kill('SIGKILL')
  assert.equal((await owner.done).signal, 'SIGKILL')
  const successor = childOwner(runDirectory, 'probe')
  t.after(async () => { successor.child.kill('SIGKILL'); await successor.done })
  const acquired = await successor.done
  assert.equal(acquired.code, 0, acquired.stderr)
  assert.equal(acquired.stdout, 'owned\n')
})

test('resume rejects a different API target before invoking work', async (t) => {
  const runDirectory = directory(t)
  await withAttemptJournal({ directory: runDirectory, start }, async () => {})
  await assert.rejects(withAttemptJournal({ directory: runDirectory, target: 'https://another.example' }, async () => {
    assert.fail('wrong-target callback ran')
  }), /target differs/)
  await withAttemptJournal({ directory: runDirectory, target: `${start.target}/` }, async (journal) => {
    assert.equal(journal.state.intent.target, start.target)
  })
})

test('status rejects malformed journals and conflicting indexes', async (t) => {
  const runDirectory = directory(t)
  await withAttemptJournal({ directory: runDirectory, start }, async () => {})
  const original = readAttemptJournal(runDirectory)
  const journalPath = join(runDirectory, 'journal.json')
  for (const corrupt of ['{', '', 'null']) {
    writeFileSync(journalPath, corrupt)
    assert.throws(() => readAttemptJournal(runDirectory))
  }
  for (const mutate of [
    (state: RunState) => { state.attempts = [{ index: 0, kind: 'pending' }, { index: 0, kind: 'pending' }] },
    (state: RunState) => { state.attempts.pop() },
    (state: RunState) => {
      state.operation = { kind: 'accepted', id: 'batch', done: true }
      state.attempts = [ready(0), { ...ready(1), sandboxId: 'sandbox-0' }]
    },
  ]) {
    const state = structuredClone(original)
    mutate(state)
    writeFileSync(journalPath, JSON.stringify(state))
    assert.throws(() => readAttemptJournal(runDirectory), /indexes|index/)
  }
})

test('a failed checkpoint stops subsequent commits and preserves the prior state', async (t) => {
  const runDirectory = directory(t)
  await withAttemptJournal({ directory: runDirectory, start }, async (journal) => {
    const before = structuredClone(journal.state)
    const journalPath = join(runDirectory, 'journal.json')
    const saved = join(runDirectory, 'saved-journal.json')
    renameSync(journalPath, saved)
    mkdirSync(journalPath)
    assert.throws(() => journal.commit((draft) => { draft.phase = 'cleaning' }), isJournalError)
    assert.deepEqual(journal.state, before)
    rmSync(journalPath, { recursive: true })
    renameSync(saved, journalPath)
    let invoked = false
    assert.throws(() => journal.commit(() => { invoked = true }), isJournalError)
    assert.equal(invoked, false)
    assert.deepEqual(readAttemptJournal(runDirectory), before)
  })
})

test('completed outputs survive reopen and cleanup without changing indexed results', async (t) => {
  const runDirectory = directory(t)
  await withAttemptJournal({ directory: runDirectory, start }, async (journal) => recordCompletion(journal))
  assert.equal(readFileSync(join(runDirectory, 'attempt-0.stdout'), 'utf8'), 'saved result\n')
  assert.equal(readFileSync(join(runDirectory, 'attempt-0.stderr'), 'utf8'), 'diagnostic\n')
  const before = readAttemptJournal(runDirectory).attempts[0]
  assert.ok(before?.kind === 'ready')
  await withAttemptJournal({ directory: runDirectory }, async (journal) => {
    journal.commit((draft) => {
      draft.phase = 'complete'
      const attempt = draft.attempts[0]
      assert.ok(attempt?.kind === 'ready')
      attempt.cleanup = 'deleted'
    })
  })
  const recovered = readAttemptJournal(runDirectory)
  const after = recovered.attempts[0]
  assert.ok(after?.kind === 'ready')
  assert.deepEqual(after.command, before.command)
  assert.equal(after.cleanup, 'deleted')
  assert.deepEqual(summarizeRun(recovered), {
    run_id: recovered.intent.runId, phase: 'complete', requested: 2, succeeded: 1,
    failed: 1, unresolved: 0, passed: false,
  })
  assert.equal(readFileSync(join(runDirectory, 'attempt-0.stdout'), 'utf8'), 'saved result\n')
})

test('terminal command results cannot be changed or downgraded', async (t) => {
  for (const mode of ['change', 'waiting', 'pending']) {
    await t.test(mode, async (t) => {
      const runDirectory = directory(t)
      await withAttemptJournal({ directory: runDirectory, start }, async (journal) => {
        recordCompletion(journal)
        const before = readAttemptJournal(runDirectory)
        assert.throws(() => journal.commit((draft) => {
          const attempt = draft.attempts[0]
          assert.ok(attempt?.kind === 'ready' && attempt.command.kind === 'completed')
          if (mode === 'change') attempt.command.result.exitCode = 1
          else if (mode === 'waiting') attempt.command = { kind: 'waiting' }
          else draft.attempts[0] = { index: 0, kind: 'pending' }
        }), isJournalError, mode)
        assert.deepEqual(readAttemptJournal(runDirectory), before)
      })
    })
  }
})

test('a retained journal handle cannot commit after ownership returns', async (t) => {
  const runDirectory = directory(t)
  const escaped = await withAttemptJournal({ directory: runDirectory, start }, async (journal) => journal)
  assert.throws(() => escaped.commit(() => assert.fail('closed callback ran')), isJournalError)
})

test('invalid initial count cannot create a journal or invoke work', async (t) => {
  for (const count of [-1, 0, 1, 33, 2.5, Number.NaN]) {
    const runDirectory = directory(t)
    await assert.rejects(withAttemptJournal({ directory: runDirectory, start: { ...start, count } }, async () => {
      assert.fail('invalid initial count invoked work')
    }))
    assert.throws(() => readFileSync(join(runDirectory, 'journal.json')), { code: 'ENOENT' })
  }
})
