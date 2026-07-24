import type { CommandResult } from './types.js'

/**
 * Base class for all errors thrown by the sandbox SDK.
 *
 * When the error came from an API response, {@link status} carries the HTTP
 * status the server answered with. Errors raised on WebSocket endpoints carry
 * the status the server encoded in the close code (`4000 + status`), so the
 * same `status` checks work on both transports.
 */
export class SandboxError extends Error {
  /** HTTP status behind this error; absent for client-side failures. */
  readonly status?: number

  constructor(message: string, status?: number) {
    super(message)
    this.name = 'SandboxError'
    if (status !== undefined) this.status = status
  }
}

/**
 * Thrown when the API rejects the request with 401 or 403 —
 * the API key is missing, invalid, or not authorized.
 */
export class AuthenticationError extends SandboxError {
  constructor(message: string, status?: number) {
    super(message, status)
    this.name = 'AuthenticationError'
  }
}

/**
 * Thrown when the API responds with 404 — e.g. an unknown sandbox id
 * or a file path that does not exist in the guest.
 */
export class NotFoundError extends SandboxError {
  constructor(message: string, status?: number) {
    super(message, status)
    this.name = 'NotFoundError'
  }
}

/**
 * Thrown when the API responds with 409 — the operation conflicts with the
 * sandbox's current state. Common cases: snapshotting or hibernating a
 * sandbox that isn't running on its host, or restoring a snapshot whose
 * source (or a previous restore) still holds its baked network identity.
 */
export class ConflictError extends SandboxError {
  constructor(message: string, status?: number) {
    super(message, status)
    this.name = 'ConflictError'
  }
}

/**
 * Thrown when the fleet is out of capacity rather than broken (429/503):
 * every host is full and the gateway's create queue timed out, a host's
 * tap/IP/port pool is exhausted, or a wake was refused by memory admission.
 *
 * This is the retryable failure class — unlike a plain {@link SandboxError},
 * the same request may well succeed shortly after, once a sandbox is
 * destroyed or the autoscaler adds a host. {@link retryAfterMs} carries the
 * server's `Retry-After` hint when it sent one.
 */
export class CapacityError extends SandboxError {
  /** Server's `Retry-After` hint in milliseconds; absent when it sent none. */
  readonly retryAfterMs?: number

  constructor(message: string, status?: number, retryAfterMs?: number) {
    super(message, status)
    this.name = 'CapacityError'
    if (retryAfterMs !== undefined) this.retryAfterMs = retryAfterMs
  }
}

/**
 * Thrown when an operation exceeds its time budget: a command that the
 * guest killed for running past `timeoutMs`, or an HTTP request that
 * hit the client-side request timeout.
 */
export class TimeoutError extends SandboxError {
  constructor(message: string, status?: number) {
    super(message, status)
    this.name = 'TimeoutError'
  }
}

/**
 * Thrown by `sandbox.commands.run()` when the command exits with a
 * non-zero exit code (matching e2b semantics). Carries the full
 * {@link CommandResult} plus convenience accessors.
 */
export class CommandExitError extends SandboxError {
  /** The full result of the failed command. */
  readonly result: CommandResult

  constructor(result: CommandResult, cmd?: string) {
    const what = cmd ? `Command failed: ${cmd}` : 'Command failed'
    const stderr = result.stderr.trim()
    super(`${what} (exit code ${result.exitCode})${stderr ? `\n${stderr}` : ''}`)
    this.name = 'CommandExitError'
    this.result = result
  }

  /** Exit code of the failed command. */
  get exitCode(): number {
    return this.result.exitCode
  }

  /** Captured stdout of the failed command. */
  get stdout(): string {
    return this.result.stdout
  }

  /** Captured stderr of the failed command. */
  get stderr(): string {
    return this.result.stderr
  }
}
