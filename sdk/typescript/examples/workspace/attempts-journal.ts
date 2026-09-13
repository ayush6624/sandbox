import { createHash, randomUUID } from 'node:crypto'
import { closeSync, fstatSync, fsyncSync, mkdirSync, openSync, readSync, renameSync, unlinkSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:net'
import { dirname, join, resolve } from 'node:path'
import { SandboxError, type SandboxResources, type UsageReport } from '../../src/index.js'
import { parseCompletedCommandResult, type CompletedCommandResult } from './command-receipt.js'

export interface RunDiagnostic { message: string; code?: string; requestId?: string; status?: number }
export interface RunIntent {
  runId: string
  target: string
  snapshotId: string
  count: number
  maxParallelism: number
  script: string
  timeoutMs: number
  ttlMs: number
  startedAt: string
  requestKey: string
}
export type CommandState =
  | { kind: 'waiting' }
  | { kind: 'issued' }
  | { kind: 'uncertain'; error: RunDiagnostic }
  | { kind: 'unavailable'; error: RunDiagnostic }
  | { kind: 'completed'; result: CompletedCommandResult }
export type UsageState =
  | { kind: 'unobserved' }
  | { kind: 'unavailable'; error: RunDiagnostic }
  | { kind: 'reported'; totals: UsageReport['totals']; coverage: UsageReport['coverage'] }
export interface ReadyAttempt {
  index: number
  kind: 'ready'
  sandboxId: string
  readyAt: string
  resources: SandboxResources
  command: CommandState
  cleanup: 'retained' | 'deleted'
  usage: UsageState
}
export type Attempt =
  | { index: number; kind: 'pending' }
  | { index: number; kind: 'create-failed'; error: RunDiagnostic }
  | ReadyAttempt
export interface RunState {
  version: 1
  intent: RunIntent
  phase: 'running' | 'cleaning' | 'complete'
  operation: { kind: 'unaccepted' } | { kind: 'accepted'; id: string; done: boolean }
  attempts: Attempt[]
  observations: RunDiagnostic[]
}
export interface StartAttempts {
  target: string
  snapshotId: string
  count: number
  maxParallelism: number
  script: string
  timeoutMs: number
  ttlMs: number
}

const journalLimit = 16 << 20
const journalName = 'journal.json'
class JournalError extends Error {}
export function isJournalError(error: unknown): boolean { return error instanceof JournalError }

export function runDiagnostic(error: unknown): RunDiagnostic {
  return {
    message: (error instanceof Error ? error.message : String(error)).slice(0, 8192),
    ...(error instanceof SandboxError && error.code !== undefined ? { code: error.code } : {}),
    ...(error instanceof SandboxError && error.requestId !== undefined ? { requestId: error.requestId } : {}),
    ...(error instanceof SandboxError && error.status !== undefined ? { status: error.status } : {}),
  }
}

export function canonicalTarget(value: string): string {
  const url = new URL(value)
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) {
    throw new Error('API target must be an HTTP(S) URL without credentials, query, or fragment')
  }
  return url.toString().replace(/\/+$/, '')
}

function syncDirectory(directory: string): void {
  const fd = openSync(directory, 'r')
  try { fsyncSync(fd) } finally { closeSync(fd) }
}

function atomicWrite(path: string, data: string | Uint8Array): void {
  const temporary = `${path}.${randomUUID()}.tmp`
  const fd = openSync(temporary, 'wx', 0o600)
  try {
    writeFileSync(fd, data)
    fsyncSync(fd)
  } finally { closeSync(fd) }
  try {
    renameSync(temporary, path)
    syncDirectory(dirname(path))
  } finally {
    try { unlinkSync(temporary) } catch (error) { if (!hasCode(error, 'ENOENT')) throw error }
  }
}

export class AttemptJournal {
  private current: RunState
  private failed = false
  private closed = false
  constructor(readonly directory: string, state: RunState) { this.current = structuredClone(state) }
  get state(): RunState { return structuredClone(this.current) }
  commit(update: (draft: RunState) => void): void {
    if (this.failed || this.closed) throw new JournalError('Run journal is no longer writable')
    try {
      const next = structuredClone(this.current)
      update(next)
      next.observations = next.observations.slice(-32)
      if (JSON.stringify(next.intent) !== JSON.stringify(this.current.intent)) throw new Error('Run intent is immutable')
      // Validate at the durable boundary as well as on reopen. Invalid local
      // transitions must not become the next process's recovery authority.
      const data = JSON.stringify(next, null, 2) + '\n'
      if (Buffer.byteLength(data) > journalLimit) throw new Error('Run journal exceeds 16 MiB')
      const validated = parseJournal(JSON.parse(data))
      const previousOperation = this.current.operation
      if (previousOperation.kind === 'accepted' && (validated.operation.kind !== 'accepted'
        || validated.operation.id !== previousOperation.id || previousOperation.done && !validated.operation.done)) {
        throw new Error('Accepted operation identity and completion are immutable')
      }
      if (this.current.phase === 'complete' && validated.phase !== 'complete'
        || this.current.phase === 'cleaning' && validated.phase === 'running') {
        throw new Error('Workspace cleanup cannot return to running')
      }
      for (const attempt of next.attempts) {
        const previous = this.current.attempts[attempt.index]
        if (previous?.kind === 'create-failed' && JSON.stringify(previous) !== JSON.stringify(attempt)) {
          throw new Error('Terminal creation failure is immutable')
        }
        if (previous?.kind === 'ready') {
          if (attempt.kind !== 'ready' || previous.sandboxId !== attempt.sandboxId
            || previous.readyAt !== attempt.readyAt || JSON.stringify(previous.resources) !== JSON.stringify(attempt.resources)) {
            throw new Error('Created workspace member identity is immutable')
          }
          if (previous.cleanup === 'deleted' && attempt.cleanup !== 'deleted') throw new Error('Deleted workspace member cannot be retained again')
          if (previous.command.kind === 'completed') {
            if (JSON.stringify(previous.command) !== JSON.stringify(attempt.command)) throw new Error('Completed result is immutable')
            continue
          }
        }
        if (attempt.kind !== 'ready' || attempt.command.kind !== 'completed') continue
        for (const stream of ['stdout', 'stderr'] as const) {
          atomicWrite(join(this.directory, `attempt-${attempt.index}.${stream}`), Buffer.from(attempt.command.result[stream].base64, 'base64'))
        }
      }
      atomicWrite(join(this.directory, journalName), data)
      this.current = validated
    } catch (error) {
      this.failed = true
      throw new JournalError(`Cannot commit workspace run: ${runDiagnostic(error).message}`, { cause: error })
    }
  }
  close(): void { this.closed = true }
}

function hasCode(error: unknown, code: string): boolean {
  return error instanceof Error && 'code' in error && error.code === code
}

export async function withAttemptJournal<T>(options: {
  directory: string; target?: string; start?: StartAttempts
}, action: (journal: AttemptJournal) => Promise<T>): Promise<T> {
  const directory = resolve(options.directory)
  let state = options.start ? initialState(options.start) : readAttemptJournal(directory)
  if (options.target !== undefined && canonicalTarget(options.target) !== state.intent.target) throw new Error('Run API target differs from the supplied endpoint')
  const port = 20_000 + createHash('sha256').update(state.intent.runId).digest().readUInt32BE(0) % 40_000
  const lock = createServer((socket) => socket.destroy())
  try {
    await new Promise<void>((done, reject) => {
      lock.once('error', reject)
      lock.listen({ host: '127.0.0.1', port, exclusive: true }, () => {
        lock.removeListener('error', reject)
        done()
      })
    })
  } catch (error) {
    if (hasCode(error, 'EADDRINUSE')) throw new Error(`Workspace run is busy; its local ownership port ${port} is in use`)
    throw error
  }
  let journal: AttemptJournal | undefined
  try {
    if (options.start) {
      mkdirSync(dirname(directory), { recursive: true })
      mkdirSync(directory, { mode: 0o700 })
      syncDirectory(dirname(directory))
      atomicWrite(join(directory, journalName), JSON.stringify(state, null, 2) + '\n')
    } else {
      const reread = readAttemptJournal(directory)
      if (reread.intent.runId !== state.intent.runId || reread.intent.target !== state.intent.target) throw new Error('Run identity changed while acquiring ownership')
      state = reread
    }
    journal = new AttemptJournal(directory, state)
    return await action(journal)
  } finally {
    journal?.close()
    await new Promise<void>((done, reject) => lock.close((error) => error ? reject(error) : done()))
  }
}

function initialState(input: StartAttempts): RunState {
  const count = integer(input.count, 2, 32)
  const runId = randomUUID()
  const state: RunState = {
    version: 1,
    intent: { ...input, target: canonicalTarget(input.target), runId, startedAt: new Date().toISOString(), requestKey: `workspace-${runId}-attempts` },
    phase: 'running', operation: { kind: 'unaccepted' },
    attempts: Array.from({ length: count }, (_, index): Attempt => ({ index, kind: 'pending' })), observations: [],
  }
  return parseJournal(state)
}

export function readAttemptJournal(directory: string): RunState {
  const fd = openSync(join(directory, journalName), 'r')
  try {
    const size = fstatSync(fd).size
    if (size < 1 || size > journalLimit) throw new Error('Invalid workspace journal size')
    const body = Buffer.alloc(size)
    let offset = 0
    while (offset < size) {
      const count = readSync(fd, body, offset, size - offset, null)
      if (count === 0) throw new Error('Truncated workspace journal')
      offset += count
    }
    if (readSync(fd, Buffer.alloc(1), 0, 1, null) !== 0) throw new Error('Workspace journal grew while reading')
    return parseJournal(JSON.parse(body.toString('utf8')))
  } finally { closeSync(fd) }
}

export function summarizeRun(state: RunState) {
  const succeeded = state.attempts.filter((a) => a.kind === 'ready' && a.command.kind === 'completed' && a.command.result.exitCode === 0).length
  const failed = state.attempts.filter((a) => a.kind === 'create-failed' || a.kind === 'ready' && (a.command.kind === 'unavailable' || a.command.kind === 'completed' && a.command.result.exitCode !== 0)).length
  return { run_id: state.intent.runId, phase: state.phase, requested: state.intent.count, succeeded, failed,
    unresolved: state.intent.count - succeeded - failed,
    passed: state.phase === 'complete' && succeeded === state.intent.count }
}

function isObject(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}
function record(value: unknown): Record<string, unknown> {
  if (!isObject(value)) throw new Error('Invalid workspace journal object')
  return value
}
function text(value: unknown, limit = 8192): string {
  if (typeof value !== 'string' || !value || value.length > limit) throw new Error('Invalid workspace journal string')
  return value
}
function number(value: unknown, min = 0, max = Number.MAX_SAFE_INTEGER): number {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < min || value > max) throw new Error('Invalid workspace journal number')
  return value
}
function integer(value: unknown, min = 0, max = Number.MAX_SAFE_INTEGER): number {
  const result = number(value, min, max)
  if (!Number.isInteger(result)) throw new Error('Invalid workspace journal integer')
  return result
}
function boolean(value: unknown): boolean {
  if (typeof value !== 'boolean') throw new Error('Invalid workspace journal boolean')
  return value
}
function timestamp(value: unknown): string {
  const result = text(value)
  if (!Number.isFinite(Date.parse(result))) throw new Error('Invalid workspace journal timestamp')
  return result
}
function diagnostic(value: unknown): RunDiagnostic {
  const v = record(value)
  return { message: text(v.message), ...(v.code === undefined ? {} : { code: text(v.code) }),
    ...(v.requestId === undefined ? {} : { requestId: text(v.requestId) }),
    ...(v.status === undefined ? {} : { status: integer(v.status, 100, 599) }) }
}
function command(value: unknown): CommandState {
  const v = record(value)
  switch (v.kind) {
    case 'waiting': case 'issued': return { kind: v.kind }
    case 'uncertain': case 'unavailable': return { kind: v.kind, error: diagnostic(v.error) }
    case 'completed': return { kind: 'completed', result: parseCompletedCommandResult(v.result) }
    default: throw new Error('Invalid workspace command state')
  }
}
function usage(value: unknown): UsageState {
  const v = record(value)
  if (v.kind === 'unobserved') return { kind: 'unobserved' }
  if (v.kind === 'unavailable') return { kind: 'unavailable', error: diagnostic(v.error) }
  if (v.kind !== 'reported') throw new Error('Invalid workspace usage state')
  const totals = record(v.totals), coverage = record(v.coverage)
  if (coverage.scope !== 'live_hosts') throw new Error('Invalid workspace usage coverage')
  return { kind: 'reported', totals: {
    intervals: integer(totals.intervals), openIntervals: integer(totals.openIntervals),
    durationSeconds: number(totals.durationSeconds), vcpuSeconds: number(totals.vcpuSeconds),
    memoryMibSeconds: number(totals.memoryMibSeconds), cpuSeconds: number(totals.cpuSeconds),
  }, coverage: { scope: 'live_hosts', hostsReporting: integer(coverage.hostsReporting), truncated: boolean(coverage.truncated) } }
}
function attempt(value: unknown, index: number): Attempt {
  const v = record(value)
  if (integer(v.index) !== index) throw new Error('Workspace member indexes do not match the original batch')
  if (v.kind === 'pending') return { index, kind: 'pending' }
  if (v.kind === 'create-failed') return { index, kind: 'create-failed', error: diagnostic(v.error) }
  if (v.kind !== 'ready' || v.cleanup !== 'retained' && v.cleanup !== 'deleted') throw new Error('Invalid workspace member')
  const resources = record(v.resources)
  return { index, kind: 'ready', sandboxId: text(v.sandboxId, 256), readyAt: timestamp(v.readyAt),
    resources: { vcpus: number(resources.vcpus, 1), memoryMib: number(resources.memoryMib, 1) },
    command: command(v.command), cleanup: v.cleanup, usage: usage(v.usage) }
}
function parseJournal(value: unknown): RunState {
  const v = record(value), i = record(v.intent), op = record(v.operation)
  if (v.version !== 1 || !['running', 'cleaning', 'complete'].includes(text(v.phase))) throw new Error('Unsupported workspace journal version or phase')
  const phase = v.phase
  if (phase !== 'running' && phase !== 'cleaning' && phase !== 'complete') throw new Error('Invalid workspace phase')
  const runId = text(i.runId)
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(runId)) throw new Error('Invalid workspace run ID')
  const count = integer(i.count, 2, 32)
  const script = text(i.script, 65536)
  if (Buffer.byteLength(script) > 65536) throw new Error('Workspace script exceeds 64 KiB')
  const requestKey = text(i.requestKey)
  if (requestKey !== `workspace-${runId}-attempts`) throw new Error('Workspace request key differs from run identity')
  const intent: RunIntent = { runId, target: canonicalTarget(text(i.target)), snapshotId: text(i.snapshotId, 256),
    count, maxParallelism: integer(i.maxParallelism, 1, Math.min(count, 8)), script,
    timeoutMs: integer(i.timeoutMs, 1000, 300_000), ttlMs: integer(i.ttlMs, 60_000, 24 * 60 * 60_000),
    startedAt: timestamp(i.startedAt), requestKey }
  let operation: RunState['operation']
  if (op.kind === 'unaccepted') operation = { kind: 'unaccepted' }
  else if (op.kind === 'accepted') operation = { kind: 'accepted', id: text(op.id, 256), done: boolean(op.done) }
  else throw new Error('Invalid workspace batch acceptance')
  if (!Array.isArray(v.attempts) || v.attempts.length !== count) throw new Error('Workspace journal does not contain every requested index')
  const attempts = v.attempts.map(attempt)
  const ids = attempts.flatMap((a) => a.kind === 'ready' ? [a.sandboxId] : [])
  if (new Set(ids).size !== ids.length) throw new Error('Workspace indexes share a sandbox identity')
  if (operation.kind === 'unaccepted' && attempts.some((a) => a.kind !== 'pending')) throw new Error('Workspace results precede batch acceptance')
  if (phase === 'complete' && (operation.kind !== 'accepted' || !operation.done || attempts.some((a) => a.kind === 'pending' || a.kind === 'ready' && a.cleanup !== 'deleted'))) throw new Error('Workspace cleanup completed before all children settled')
  if (!Array.isArray(v.observations) || v.observations.length > 32) throw new Error('Invalid workspace observations')
  return { version: 1, intent, phase, operation, attempts, observations: v.observations.map(diagnostic) }
}
