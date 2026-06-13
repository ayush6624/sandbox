import type { CommandResult } from './types.js'

/**
 * Base class for all errors thrown by the sandbox SDK.
 */
export class SandboxError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'SandboxError'
  }
}

/**
 * Thrown when the API rejects the request with 401 or 403 —
 * the API key is missing, invalid, or not authorized.
 */
export class AuthenticationError extends SandboxError {
  constructor(message: string) {
    super(message)
    this.name = 'AuthenticationError'
  }
}

/**
 * Thrown when the API responds with 404 — e.g. an unknown sandbox id
 * or a file path that does not exist in the guest.
 */
export class NotFoundError extends SandboxError {
  constructor(message: string) {
    super(message)
    this.name = 'NotFoundError'
  }
}

/**
 * Thrown when an operation exceeds its time budget: a command that the
 * guest killed for running past `timeoutMs`, or an HTTP request that
 * hit the client-side request timeout.
 */
export class TimeoutError extends SandboxError {
  constructor(message: string) {
    super(message)
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
