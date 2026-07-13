import type { ApiClient } from './client.js'
import {
  AuthenticationError,
  NotFoundError,
  SandboxError,
  TimeoutError,
} from './errors.js'

/**
 * Structural view of the standard WebSocket API — enough for the PTY client.
 * Declared locally so the SDK compiles without DOM libs and works with any
 * spec-compliant implementation (browsers, Node 22+'s built-in client).
 */
interface WsLike {
  binaryType: string
  readonly readyState: number
  send(data: string | ArrayBufferLike | Uint8Array): void
  close(code?: number, reason?: string): void
  addEventListener(type: string, listener: (event: never) => void): void
}

type WsCtor = new (url: string) => WsLike

/** Options for {@link Pty.create}. */
export interface PtyCreateOpts {
  /** Called with every chunk of terminal output (stdout + stderr, raw bytes). */
  onData: (data: Uint8Array) => void
  /** Initial terminal width in columns (default 80). */
  cols?: number
  /** Initial terminal height in rows (default 24). */
  rows?: number
  /** Working directory the shell starts in (default `/home/sandbox/app`). */
  cwd?: string
  /** Time budget for the WebSocket to connect, in milliseconds (default 30 000). */
  connectTimeoutMs?: number
}

const DEFAULT_CONNECT_TIMEOUT_MS = 30_000

/**
 * How long after the WebSocket opens {@link Pty.create} keeps listening for
 * an immediate rejection before declaring the pty live. The server delivers
 * auth and routing failures as close frames right AFTER a successful
 * handshake (the only channel browsers surface), so "open" alone doesn't
 * mean accepted. The first output frame short-circuits the wait.
 */
const POST_OPEN_GRACE_MS = 100

/**
 * Close codes 4000-4999 mirror the HTTP status the server would have answered
 * with (4000 + status): the server delivers endpoint errors as post-handshake
 * close frames because browsers reduce a failed WebSocket handshake to an
 * opaque 1006. This maps them back onto the SDK's error classes.
 */
function closeError(code: number, reason: string): SandboxError {
  const detail = reason || `WebSocket closed with code ${code}`
  if (code === 4401 || code === 4403) return new AuthenticationError(detail)
  if (code === 4404) return new NotFoundError(detail)
  if (code >= 4000 && code < 5000) return new SandboxError(detail)
  return new SandboxError(
    `PTY connection closed unexpectedly (code ${code}${reason ? `: ${reason}` : ''})` +
      (code === 1006
        ? ' — the connection failed before or during the handshake; check that the API URL is reachable and uses the right scheme'
        : '')
  )
}

/**
 * A live interactive shell (`bash -l` on a real pty) inside a sandbox,
 * returned by {@link Pty.create}. Terminal output arrives via the `onData`
 * callback; write input with {@link sendInput}, adjust the window with
 * {@link resize}, and await {@link exited} for the shell's exit code.
 */
export class PtyHandle {
  /**
   * Resolves with the shell's exit code when it exits cleanly (including
   * after {@link kill}, which ends the shell's process group). Rejects with
   * an SDK error when the connection ends abnormally.
   */
  readonly exited: Promise<number>

  private readonly ws: WsLike
  private readonly encoder = new TextEncoder()

  /** @internal — construct via {@link Pty.create}. */
  constructor(ws: WsLike, exited: Promise<number>) {
    this.ws = ws
    this.exited = exited
  }

  /** Writes input to the shell's stdin (strings are UTF-8 encoded). */
  sendInput(data: Uint8Array | string): void {
    this.ws.send(typeof data === 'string' ? this.encoder.encode(data) : data)
  }

  /** Resizes the pty window. */
  resize(size: { cols: number; rows: number }): void {
    this.ws.send(JSON.stringify({ type: 'resize', cols: size.cols, rows: size.rows }))
  }

  /**
   * Closes the connection; the guest kills the shell's process group. The
   * {@link exited} promise still settles.
   */
  kill(): void {
    this.ws.close(1000, 'client closed')
  }
}

/**
 * Interactive PTY sessions inside a sandbox, e2b-style, backed by the
 * `GET /sandboxes/{id}/shell` WebSocket:
 *
 * ```ts
 * const pty = await sandbox.pty.create({
 *   cols: 120,
 *   rows: 30,
 *   onData: (data) => process.stdout.write(data),
 * })
 * pty.sendInput('ls -la\n')
 * pty.resize({ cols: 200, rows: 50 })
 * pty.sendInput('exit\n')
 * console.log('shell exited with', await pty.exited)
 * ```
 *
 * Requires a global `WebSocket` (browsers, Node 22+). Auth and routing
 * failures reject with the same typed errors as the REST API
 * ({@link AuthenticationError}, {@link NotFoundError}) — the server reports
 * them through WebSocket close codes 4401/4404/45xx.
 */
export class Pty {
  private readonly client: ApiClient
  private readonly sandboxId: string

  /** @internal — reached as `sandbox.pty`. */
  constructor(client: ApiClient, sandboxId: string) {
    this.client = client
    this.sandboxId = sandboxId
  }

  /**
   * Opens an interactive shell and resolves once the connection is up.
   * A hibernated sandbox is woken transparently first.
   */
  async create(opts: PtyCreateOpts): Promise<PtyHandle> {
    const Ws = (globalThis as { WebSocket?: WsCtor }).WebSocket
    if (!Ws) {
      throw new SandboxError(
        'PTY support requires a global WebSocket implementation — use Node 22+ or a browser.'
      )
    }

    const url = this.client.wsUrl(`/sandboxes/${this.sandboxId}/shell`, {
      cols: String(opts.cols ?? 80),
      rows: String(opts.rows ?? 24),
      ...(opts.cwd ? { cwd: opts.cwd } : {}),
    })
    const ws = new Ws(url)
    ws.binaryType = 'arraybuffer'

    let settleExit: { resolve: (code: number) => void; reject: (err: Error) => void }
    const exited = new Promise<number>((resolve, reject) => {
      settleExit = { resolve, reject }
    })
    // Keep an unobserved abnormal close (or a create-time rejection, where no
    // handle exists yet) from surfacing as an unhandled rejection; callers
    // that await `exited` still receive it.
    exited.catch(() => {})

    ws.addEventListener('message', (event: { data: unknown }) => {
      // Binary frames are raw terminal bytes; the guest sends nothing else.
      if (event.data instanceof ArrayBuffer) {
        opts.onData(new Uint8Array(event.data))
      }
    })

    // The connection counts as established on the first output frame, or a
    // grace period after open with no rejection — NOT on open alone: the
    // server reports auth/routing failures as close frames right after a
    // successful handshake (see closeError).
    await new Promise<void>((resolve, reject) => {
      const timeoutMs = opts.connectTimeoutMs ?? DEFAULT_CONNECT_TIMEOUT_MS
      let settled = false
      let grace: ReturnType<typeof setTimeout> | undefined
      const settle = (fn: () => void) => {
        if (settled) return
        settled = true
        clearTimeout(timer)
        if (grace !== undefined) clearTimeout(grace)
        fn()
      }
      const timer = setTimeout(() => {
        settle(() => {
          ws.close()
          reject(new TimeoutError(`PTY connection timed out after ${timeoutMs} ms`))
        })
      }, timeoutMs)

      ws.addEventListener('open', () => {
        grace = setTimeout(() => settle(resolve), POST_OPEN_GRACE_MS)
      })
      ws.addEventListener('message', () => settle(resolve))
      ws.addEventListener('error', () => {
        // The close event that follows carries the actual code/reason.
      })
      ws.addEventListener('close', (event: { code: number; reason: string }) => {
        // A clean shell exit closes with reason "exit:<code>"; a plain 1000
        // is a deliberate local close (kill()). Both are normal ends.
        if (event.reason.startsWith('exit:')) {
          settleExit.resolve(Number(event.reason.slice('exit:'.length)) || 0)
          settle(resolve)
          return
        }
        if (event.code === 1000) {
          settleExit.resolve(0)
          settle(resolve)
          return
        }
        const err = closeError(event.code, event.reason)
        settleExit.reject(err)
        settle(() => reject(err))
      })
    })

    return new PtyHandle(ws, exited)
  }
}
