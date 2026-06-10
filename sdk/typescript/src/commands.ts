import type { ApiClient } from './client.js'
import { CommandExitError, TimeoutError } from './errors.js'
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
   * @param cmd Shell command to execute.
   * @param opts Working directory, extra env vars, and timeout.
   * @returns The command's output and timing on success (exit code 0).
   * @throws {CommandExitError} when the command exits non-zero (the error carries the full {@link CommandResult}).
   * @throws {TimeoutError} when the command runs past `timeoutMs` and is killed.
   */
  async run(cmd: string, opts: CommandRunOpts = {}): Promise<CommandResult> {
    const timeoutMs = opts.timeoutMs ?? DEFAULT_COMMAND_TIMEOUT_MS

    const payload: Record<string, unknown> = {
      cmd,
      timeout_sec: Math.ceil(timeoutMs / 1000),
    }
    if (opts.cwd !== undefined) payload.cwd = opts.cwd
    if (opts.envs !== undefined) payload.env = opts.envs

    const res = await this.client.request('POST', `/sandboxes/${this.sandboxId}/exec`, {
      json: payload,
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
}
