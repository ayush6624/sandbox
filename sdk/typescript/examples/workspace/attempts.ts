import { setTimeout as sleep } from 'node:timers/promises'
import { SandboxClient, SandboxError, TimeoutError, type ClientSandbox, type Operation } from '../../src/index.js'
import { executeReceiptedCommand } from './command-receipt.js'
import { isJournalError, runDiagnostic, type AttemptJournal, type ReadyAttempt, type RunState } from './attempts-journal.js'

function updateReady(journal: AttemptJournal, index: number, update: (attempt: ReadyAttempt) => void) {
  journal.commit((draft) => {
    const attempt = draft.attempts[index]
    if (!attempt || attempt.kind !== 'ready') throw new Error(`Attempt ${index} is not ready`)
    update(attempt)
  })
}

function observeError(journal: AttemptJournal, error: unknown) {
  if (isJournalError(error)) throw error
  journal.commit((draft) => { draft.observations.push(runDiagnostic(error)) })
}

function isMissing(error: unknown): boolean {
  return error instanceof SandboxError && error.status === 404
}

function checkOwnership(state: RunState, sandbox: ClientSandbox) {
  const { intent } = state
  if (sandbox.metadata.benchmark_run_id !== intent.runId
    || sandbox.metadata.purpose !== 'workspace-attempts'
    || sandbox.metadata.workspace_snapshot_id !== intent.snapshotId
    || sandbox.source.type !== 'snapshot' || sandbox.source.id !== intent.snapshotId) {
    throw new Error(`Sandbox ${sandbox.id} does not belong to this workspace run`)
  }
}

function mergeOperation(journal: AttemptJournal, operation: Operation<ClientSandbox>) {
  const state = operation.state
  if (state.requested !== journal.state.intent.count) throw new Error('Operation member count differs from run intent')
  const seen = new Set<number>()
  journal.commit((draft) => {
    if (draft.operation.kind === 'accepted' && draft.operation.id !== operation.id) throw new Error('Operation identity changed')
    draft.operation = { kind: 'accepted', id: operation.id, done: operation.done }
    for (const result of state.results) {
      if (!Number.isInteger(result.index) || result.index < 0 || result.index >= draft.intent.count || seen.has(result.index)) {
        throw new Error('Operation returned invalid or duplicate member index')
      }
      seen.add(result.index)
      const existing = draft.attempts[result.index]
      if (!existing) throw new Error('Operation member is outside the run')
      if (result.value && result.error) throw new Error('Operation member has both a sandbox and an error')
      if (result.value) {
        if (existing.kind === 'create-failed' || existing.kind === 'ready' && existing.sandboxId !== result.value.id) {
          throw new Error(`Operation changed identity for attempt ${result.index}`)
        }
        if (existing.kind === 'pending') {
          draft.attempts[result.index] = {
            index: result.index, kind: 'ready', sandboxId: result.value.id,
            readyAt: new Date().toISOString(), resources: result.value.resources,
            command: { kind: 'waiting' }, cleanup: 'retained', usage: { kind: 'unobserved' },
          }
        }
      } else if (result.error) {
        if (existing.kind === 'ready') throw new Error(`Operation replaced sandbox with error for attempt ${result.index}`)
        if (existing.kind === 'pending') {
          const problem = result.error
          draft.attempts[result.index] = { index: result.index, kind: 'create-failed',
            error: runDiagnostic(new SandboxError(problem.detail ?? problem.title, problem.status, { problem })) }
        }
      }
    }
    const sandboxIds = draft.attempts.flatMap((attempt) => attempt.kind === 'ready' ? [attempt.sandboxId] : [])
    if (new Set(sandboxIds).size !== sandboxIds.length) throw new Error('Operation assigned a sandbox to multiple members')
    if (operation.done && draft.attempts.some((attempt) => attempt.kind === 'pending')) {
      throw new Error('Completed operation omitted member outcomes')
    }
  })
}

async function captureUsage(journal: AttemptJournal, index: number, sandbox: ClientSandbox, signal: AbortSignal) {
  let usage: ReadyAttempt['usage']
  try {
    const report = await sandbox.usage({ signal })
    usage = { kind: 'reported', totals: report.totals, coverage: report.coverage }
  } catch (error) {
    if (isJournalError(error)) throw error
    usage = { kind: 'unavailable', error: runDiagnostic(error) }
  }
  updateReady(journal, index, (attempt) => { attempt.usage = usage })
}

async function runAttempt(journal: AttemptJournal, client: SandboxClient, attempt: ReadyAttempt, signal: AbortSignal) {
  let sandbox: ClientSandbox
  try {
    sandbox = await client.sandboxes.get(attempt.sandboxId, signal)
    checkOwnership(journal.state, sandbox)
  } catch (error) {
    if (isJournalError(error)) throw error
    updateReady(journal, attempt.index, (draft) => {
      draft.command = isMissing(error)
        ? { kind: 'unavailable', error: runDiagnostic(error) }
        : { kind: 'uncertain', error: runDiagnostic(error) }
    })
    return
  }
  updateReady(journal, attempt.index, (draft) => { draft.command = { kind: 'issued' } })
  try {
    const { intent } = journal.state
    const result = await executeReceiptedCommand(sandbox, {
      runId: intent.runId, index: attempt.index, script: intent.script, timeoutMs: intent.timeoutMs,
    })
    updateReady(journal, attempt.index, (draft) => {
      draft.command = result.kind === 'completed'
        ? { kind: 'completed', result: result.result }
        : { kind: 'uncertain', error: runDiagnostic(new Error(result.reason)) }
    })
  } catch (error) {
    if (isJournalError(error)) throw error
    updateReady(journal, attempt.index, (draft) => { draft.command = { kind: 'uncertain', error: runDiagnostic(error) } })
  }
  await captureUsage(journal, attempt.index, sandbox, signal)
}

async function deleteAttempt(journal: AttemptJournal, client: SandboxClient, attempt: ReadyAttempt, signal: AbortSignal) {
  if (attempt.cleanup === 'deleted') return
  let sandbox: ClientSandbox
  try {
    sandbox = await client.sandboxes.get(attempt.sandboxId, signal)
  } catch (error) {
    if (!isMissing(error)) throw error
    updateReady(journal, attempt.index, (draft) => { draft.cleanup = 'deleted' })
    return
  }
  checkOwnership(journal.state, sandbox)
  if (attempt.usage.kind === 'unobserved') await captureUsage(journal, attempt.index, sandbox, signal)
  let deleteError: unknown
  try {
    await sandbox.terminate({ signal, idempotencyKey: `${journal.state.intent.runId}-delete-${attempt.index}` })
  } catch (error) {
    if (isJournalError(error)) throw error
    deleteError = error
  }
  try {
    await client.sandboxes.get(attempt.sandboxId, signal)
  } catch (error) {
    if (!isMissing(error)) throw error
    updateReady(journal, attempt.index, (draft) => { draft.cleanup = 'deleted' })
    return
  }
  throw deleteError ?? new Error(`Sandbox ${attempt.sandboxId} is still present after deletion`)
}

function canCleanAutomatically(state: RunState) {
  return state.operation.kind === 'accepted' && state.operation.done && state.attempts.every((attempt) =>
    attempt.kind === 'create-failed' || attempt.kind === 'ready'
    && (attempt.command.kind === 'completed' || attempt.command.kind === 'unavailable'))
}

function enterCleanup(journal: AttemptJournal) {
  journal.commit((draft) => {
    draft.phase = 'cleaning'
    for (const attempt of draft.attempts) {
      if (attempt.kind !== 'ready') continue
      if (attempt.command.kind === 'waiting') {
        attempt.command = { kind: 'unavailable', error: runDiagnostic(new Error('Command was not run before cleanup')) }
      } else if (attempt.command.kind === 'issued') {
        attempt.command = { kind: 'uncertain', error: runDiagnostic(new Error('Command outcome was not observed before cleanup')) }
      }
    }
  })
}

/** Follow one durable batch, preserving completed results and draining admitted work. */
export async function driveWorkspaceAttempts(journal: AttemptJournal, client: SandboxClient, options: {
  cleanup?: boolean; observationTimeoutMs?: number; pollIntervalMs?: number
} = {}): Promise<RunState> {
  if (journal.state.phase === 'complete') return journal.state
  const timeoutMs = options.observationTimeoutMs ?? 300_000
  const pollIntervalMs = options.pollIntervalMs ?? 250
  if (!Number.isFinite(timeoutMs) || timeoutMs <= 0 || !Number.isFinite(pollIntervalMs) || pollIntervalMs <= 0) {
    throw new Error('Observation timeout and poll interval must be positive')
  }
  const deadline = new AbortController()
  const timer = setTimeout(() => deadline.abort(new TimeoutError('Workspace observation timed out; resume this run to continue')), timeoutMs)
  const active = new Map<number, Promise<void>>()
  const scheduled = new Set<number>()
  let taskFailure: unknown
  let observationFailure: unknown
  try {
    if (options.cleanup) enterCleanup(journal)
    const { intent } = journal.state
    const operation = journal.state.operation.kind === 'accepted'
      ? await client.operations.get(journal.state.operation.id, deadline.signal)
      : await client.sandboxes.createMany({
        count: intent.count, maxParallelism: intent.maxParallelism, source: { snapshotId: intent.snapshotId },
        ttlMs: intent.ttlMs, metadata: { benchmark_run_id: intent.runId, purpose: 'workspace-attempts', workspace_snapshot_id: intent.snapshotId },
        idempotencyKey: intent.requestKey, requestTimeoutMs: timeoutMs, signal: deadline.signal,
      })
    for (;;) {
      deadline.signal.throwIfAborted()
      if (taskFailure) throw taskFailure
      mergeOperation(journal, operation)
      if (journal.state.phase === 'running') {
        for (const attempt of journal.state.attempts) {
          if (active.size >= intent.maxParallelism) break
          if (attempt.kind !== 'ready' || attempt.cleanup === 'deleted' || scheduled.has(attempt.index)
            || attempt.command.kind === 'completed' || attempt.command.kind === 'unavailable') continue
          scheduled.add(attempt.index)
          const task = runAttempt(journal, client, attempt, deadline.signal)
            .catch((error: unknown) => { taskFailure ??= error })
            .finally(() => { active.delete(attempt.index) })
          active.set(attempt.index, task)
        }
      }
      if (operation.done) {
        if (active.size === 0) break
        await Promise.race(active.values())
      } else {
        await sleep(pollIntervalMs, undefined, { signal: deadline.signal })
        await operation.refresh(deadline.signal)
      }
    }
  } catch (error) {
    observationFailure = error
  } finally {
    await Promise.allSettled(active.values())
  }
  try {
    if (isJournalError(taskFailure)) throw taskFailure
    if (isJournalError(observationFailure)) throw observationFailure
    if (taskFailure !== undefined) observeError(journal, taskFailure)
    if (observationFailure !== undefined) observeError(journal, observationFailure)
    if (observationFailure === undefined && taskFailure === undefined
      && journal.state.phase === 'running' && canCleanAutomatically(journal.state)) enterCleanup(journal)
    if (journal.state.phase === 'cleaning' && journal.state.operation.kind === 'accepted' && journal.state.operation.done) {
      enterCleanup(journal)
      for (const attempt of journal.state.attempts) {
        if (attempt.kind !== 'ready') continue
        try { await deleteAttempt(journal, client, attempt, deadline.signal) }
        catch (error) { observeError(journal, error) }
      }
      if (journal.state.attempts.every((attempt) => attempt.kind === 'create-failed' || attempt.kind === 'ready' && attempt.cleanup === 'deleted')) {
        journal.commit((draft) => { draft.phase = 'complete' })
      }
    }
    return journal.state
  } finally {
    clearTimeout(timer)
  }
}
