import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import type { ClientSandbox } from '../../src/index.js'

export interface CommandOutput {
  base64: string
  sha256: string
  bytes: number
  truncated: boolean
}

export interface CompletedCommandResult {
  exitCode: number
  durationMs: number
  stdout: CommandOutput
  stderr: CommandOutput
}

export type GuestCommandResult =
  | { kind: 'completed'; result: CompletedCommandResult }
  | { kind: 'uncertain'; reason: string }

const helper = readFileSync(new URL('./command-receipt.py', import.meta.url), 'utf8')
const outputLimit = 65_536

function record(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

function parseOutput(value: unknown): CommandOutput {
  if (!record(value) || typeof value.base64 !== 'string' || typeof value.sha256 !== 'string'
    || typeof value.bytes !== 'number' || !Number.isSafeInteger(value.bytes)
    || value.bytes < 0 || value.bytes > outputLimit || typeof value.truncated !== 'boolean'
    || value.base64.length > 4 * Math.ceil(outputLimit / 3)
    || value.truncated && value.bytes !== outputLimit) {
    throw new Error('Invalid command output receipt')
  }
  const bytes = Buffer.from(value.base64, 'base64')
  if (bytes.toString('base64') !== value.base64 || bytes.length !== value.bytes
    || createHash('sha256').update(bytes).digest('hex') !== value.sha256) {
    throw new Error('Command output receipt does not match its bytes')
  }
  return { base64: value.base64, sha256: value.sha256, bytes: value.bytes, truncated: value.truncated }
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`
}

export async function executeReceiptedCommand(
  sandbox: ClientSandbox,
  input: { runId: string; index: number; script: string; timeoutMs: number },
): Promise<GuestCommandResult> {
  if (!/^[A-Za-z0-9_-]{1,128}$/.test(input.runId) || !Number.isInteger(input.index)
    || input.index < 0 || input.index >= 32 || Buffer.byteLength(input.script) > 65_536
    || !Number.isSafeInteger(input.timeoutMs) || input.timeoutMs <= 0) {
    throw new Error('Invalid receipted command input')
  }
  const request = {
    version: 1, run_id: input.runId, index: input.index, sandbox_id: sandbox.id,
    script: input.script, script_sha256: createHash('sha256').update(input.script).digest('hex'),
  }
  const encoded = Buffer.from(JSON.stringify(request)).toString('base64')
  const response = await sandbox.commands.run(`python3 -c ${shellQuote(helper)} ${shellQuote(encoded)}`, {
    timeoutMs: input.timeoutMs,
  })
  if (Buffer.byteLength(response.stdout) > 262_144) throw new Error('Command receipt exceeds size limit')
  const value: unknown = JSON.parse(response.stdout)
  if (!record(value) || value.version !== 1 || value.run_id !== request.run_id
    || value.index !== request.index || value.sandbox_id !== request.sandbox_id
    || value.script_sha256 !== request.script_sha256) {
    throw new Error('Command receipt identity mismatch')
  }
  if (value.kind === 'uncertain' && typeof value.reason === 'string' && value.reason.length <= 2048) {
    return { kind: 'uncertain', reason: value.reason }
  }
  if (value.kind !== 'completed' || !record(value.result)) throw new Error('Invalid command receipt')
  return { kind: 'completed', result: parseCompletedCommandResult(value.result) }
}

export function parseCompletedCommandResult(value: unknown): CompletedCommandResult {
  if (!record(value)) throw new Error('Invalid completed command receipt')
  const result = value
  if (typeof result.exitCode !== 'number' || !Number.isInteger(result.exitCode)
    || result.exitCode < 0 || result.exitCode > 255 || typeof result.durationMs !== 'number'
    || !Number.isFinite(result.durationMs) || result.durationMs < 0) {
    throw new Error('Invalid completed command receipt')
  }
  return {
    exitCode: result.exitCode, durationMs: result.durationMs,
    stdout: parseOutput(result.stdout), stderr: parseOutput(result.stderr),
  }
}
