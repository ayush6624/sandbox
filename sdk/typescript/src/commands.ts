import type { ApiClient } from './client.js'
import { CommandExitError, SandboxError, TimeoutError } from './errors.js'
import type { CommandResult, CommandRunOpts } from './types.js'

/** Default command time budget (matches the API's `timeout_sec` default of 60). */
const DEFAULT_COMMAND_TIMEOUT_MS = 60_000
/** Extra headroom added to the HTTP request timeout on top of the command timeout. */
const REQUEST_TIMEOUT_HEADROOM_MS = 15_000

interface ExecResponse {
  stdout: string
  stderr: string
  exit_code: number
  timed_out: boolean
  duration_ms: number
}

/**
 * One NDJSON line of a streaming exec response. Fields other than `type`
 * are omitted by the server when zero.
 */
interface ExecStreamEvent {
  type: 'stdout' | 'stderr' | 'exit'
  data?: string
  exit_code?: number
  timed_out?: boolean
  duration_ms?: number
}

/**
 * Run commands inside the sandbox. Available as `sandbox.commands`.
 */
export class Commands {
  constructor(
    private readonly client: ApiClient,
    private readonly sandboxId: string
  ) {}

  /**
   * Runs a command inside the sandbox via `bash -lc` and waits for it
   * to finish.
   *
   * When `opts.onStdout` or `opts.onStderr` is provided, the command's
   * output is streamed and the callbacks receive chunks as they arrive;
   * the returned result (and error semantics) are identical to the
   * buffered path.
   *
   * @param cmd Shell command to execute.
   * @param opts Working directory, extra env vars, timeout, and stream callbacks.
   * @returns The command's output and timing on success (exit code 0).
   * @throws {CommandExitError} when the command exits non-zero (the error carries the full {@link CommandResult}).
   * @throws {TimeoutError} when the command runs past `timeoutMs` and is killed.
   */
  async run(cmd: string, opts: CommandRunOpts = {}): Promise<CommandResult> {
    if (opts.onStdout || opts.onStderr) {
      return this.runStreaming(cmd, opts)
    }
    return this.runBuffered(cmd, opts)
  }

  private async runBuffered(cmd: string, opts: CommandRunOpts): Promise<CommandResult> {
    const timeoutMs = opts.timeoutMs ?? DEFAULT_COMMAND_TIMEOUT_MS

    const res = await this.client.request('POST', `/sandboxes/${this.sandboxId}/exec`, {
      json: execPayload(cmd, opts, timeoutMs),
      timeoutMs: timeoutMs + REQUEST_TIMEOUT_HEADROOM_MS,
    })
    const raw = (await res.json()) as ExecResponse

    const result: CommandResult = {
      stdout: raw.stdout,
      stderr: raw.stderr,
      exitCode: raw.exit_code,
      durationMs: raw.duration_ms,
    }

    if (raw.timed_out) {
      throw new TimeoutError(`Command timed out after ${timeoutMs} ms: ${cmd}`)
    }
    if (result.exitCode !== 0) {
      throw new CommandExitError(result, cmd)
    }
    return result
  }

  private async runStreaming(cmd: string, opts: CommandRunOpts): Promise<CommandResult> {
    const timeoutMs = opts.timeoutMs ?? DEFAULT_COMMAND_TIMEOUT_MS

    const res = await this.client.request('POST', `/sandboxes/${this.sandboxId}/exec/stream`, {
      json: execPayload(cmd, opts, timeoutMs),
      timeoutMs: timeoutMs + REQUEST_TIMEOUT_HEADROOM_MS,
    })
    if (!res.body) {
      throw new SandboxError(`exec stream returned no body: ${cmd}`)
    }

    let stdout = ''
    let stderr = ''
    let exit: ExecStreamEvent | undefined

    const handleEvent = (ev: ExecStreamEvent): void => {
      if (ev.type === 'stdout') {
        stdout += ev.data ?? ''
        opts.onStdout?.(ev.data ?? '')
      } else if (ev.type === 'stderr') {
        stderr += ev.data ?? ''
        opts.onStderr?.(ev.data ?? '')
      } else if (ev.type === 'exit') {
        exit = ev
      }
    }

    // Parse NDJSON from the body stream, carrying partial lines across chunks.
    const reader = res.body.getReader()
    const decoder = new TextDecoder()
    let buffer = ''
    try {
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        buffer += decoder.decode(value, { stream: true })
        let nl: number
        while ((nl = buffer.indexOf('\n')) !== -1) {
          const line = buffer.slice(0, nl).trim()
          buffer = buffer.slice(nl + 1)
          if (line) handleEvent(JSON.parse(line) as ExecStreamEvent)
        }
      }
      buffer += decoder.decode()
      const tail = buffer.trim()
      if (tail) handleEvent(JSON.parse(tail) as ExecStreamEvent)
    } catch (err) {
      if (err instanceof SandboxError) throw err
      if (err instanceof Error && (err.name === 'TimeoutError' || err.name === 'AbortError')) {
        throw new TimeoutError(`Command timed out after ${timeoutMs} ms: ${cmd}`)
      }
      throw new SandboxError(
        `exec stream failed: ${err instanceof Error ? err.message : String(err)}`
      )
    }

    if (!exit) {
      throw new SandboxError(`exec stream ended without an exit event: ${cmd}`)
    }

    const result: CommandResult = {
      stdout,
      stderr,
      exitCode: exit.exit_code ?? 0,
      durationMs: exit.duration_ms ?? 0,
    }

    if (exit.timed_out) {
      throw new TimeoutError(`Command timed out after ${timeoutMs} ms: ${cmd}`)
    }
    if (result.exitCode !== 0) {
      throw new CommandExitError(result, cmd)
    }
    return result
  }
}

function execPayload(
  cmd: string,
  opts: CommandRunOpts,
  timeoutMs: number
): Record<string, unknown> {
  const payload: Record<string, unknown> = {
    cmd,
    timeout_sec: Math.ceil(timeoutMs / 1000),
  }
  if (opts.cwd !== undefined) payload.cwd = opts.cwd
  if (opts.envs !== undefined) payload.env = opts.envs
  return payload
}
