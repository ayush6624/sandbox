import {
  AuthenticationError,
  CapacityError,
  ConflictError,
  NotFoundError,
  SandboxError,
  TimeoutError,
} from './errors.js'
import type { ApiErrorContext, ProblemDetails } from './errors.js'
import type { SandboxOpts } from './types.js'

/** Default timeout for ordinary API requests. */
export const DEFAULT_REQUEST_TIMEOUT_MS = 30_000
/**
 * Timeout for `POST /sandboxes` — creation blocks until the VM is ready.
 * Must exceed the gateway's create queue-wait (240 s default): during a burst
 * a create may sit queued that long while the autoscaler adds a host, then
 * still pay the bring-up + 60 s agent gate. 90 s abandoned creates the queue
 * would have served.
 */
export const CREATE_REQUEST_TIMEOUT_MS = 300_000

/** Body type accepted by the runtime's `fetch` (avoids relying on a global `BodyInit`). */
type FetchBody = NonNullable<Parameters<typeof fetch>[1]>['body']

export interface RequestOpts {
  /** Query string parameters. */
  query?: Record<string, string>
  /** JSON body (sets `Content-Type: application/json`). */
  json?: unknown
  /** Raw body (sets `Content-Type: application/octet-stream`). */
  body?: Uint8Array
  /** Per-request timeout override in milliseconds. */
  timeoutMs?: number
  /** Extra request headers, including Idempotency-Key for v1 mutations. */
  headers?: Record<string, string>
  /** Caller cancellation signal. */
  signal?: AbortSignal
  /** Number of safe retries after the initial attempt. Disabled by default. */
  retries?: number
  /** Override the JSON media type (PATCH uses application/merge-patch+json). */
  jsonContentType?: string
}

/**
 * Minimal HTTP plumbing for the sandbox REST API.
 * Zero dependencies — uses the global `fetch` available in Node 18+.
 */
export class ApiClient {
  /** Normalized base URL (no trailing slash). */
  readonly baseUrl: string
  /** Hostname of the API server, used to build sandbox preview hosts. */
  readonly apiHostname: string
  private readonly apiKey: string
  private readonly requestTimeoutMs: number

  constructor(opts: SandboxOpts = {}) {
    const apiUrl = opts.apiUrl ?? process.env.SANDBOX_API_URL
    if (!apiUrl) {
      throw new SandboxError(
        'Missing API URL: set the SANDBOX_API_URL environment variable (e.g. http://100.99.183.74:8080) or pass { apiUrl } explicitly.'
      )
    }
    const apiKey = opts.apiKey ?? process.env.SANDBOX_API_KEY
    if (!apiKey) {
      throw new AuthenticationError(
        'Missing API key: set the SANDBOX_API_KEY environment variable or pass { apiKey } explicitly.'
      )
    }

    this.baseUrl = apiUrl.replace(/\/+$/, '')
    this.apiHostname = new URL(this.baseUrl).hostname
    this.apiKey = apiKey
    this.requestTimeoutMs = opts.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS
  }

  /**
   * Builds a WebSocket URL for an API path. The URL carries no credential —
   * the token rides in the subprotocol list from {@link wsSubprotocols},
   * because the WebSocket API (browsers, and Node's built-in client) cannot
   * set request headers and the server does not accept query credentials.
   */
  wsUrl(path: string, query: Record<string, string> = {}): string {
    const url = new URL(this.baseUrl + path)
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
    for (const [k, v] of Object.entries(query)) {
      url.searchParams.set(k, v)
    }
    return url.toString()
  }

  /**
   * Subprotocols to offer on an authenticated WebSocket: the bearer credential
   * followed by the negotiable protocol the server echoes back. Two entries,
   * not one, so the handshake completes without the server reflecting the
   * secret — and offering the second is required, since a client that offers
   * subprotocols and is answered with none fails the connection.
   *
   * The token is base64url-encoded without padding because a subprotocol name
   * must be an RFC 7230 token: standard base64's `/` and `=` are rejected by
   * the WebSocket constructor itself, before any request goes out.
   */
  wsSubprotocols(): string[] {
    const bytes = new TextEncoder().encode(this.apiKey)
    let binary = ''
    for (const b of bytes) binary += String.fromCharCode(b)
    const b64url = btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
    return [`sandbox.bearer.${b64url}`, 'sandbox.shell.v1']
  }

  /**
   * Performs an authenticated request against the API and returns the raw
   * `Response`. Non-2xx responses are mapped to SDK error classes
   * ({@link AuthenticationError}, {@link NotFoundError}, {@link ConflictError},
   * {@link CapacityError}, {@link SandboxError}); client-side timeouts throw
   * {@link TimeoutError}.
   */
  async request(method: string, path: string, opts: RequestOpts = {}): Promise<Response> {
    const url = new URL(this.baseUrl + path)
    for (const [k, v] of Object.entries(opts.query ?? {})) {
      url.searchParams.set(k, v)
    }

    const headers: Record<string, string> = {
      Authorization: `Bearer ${this.apiKey}`,
      ...opts.headers,
    }
    let body: FetchBody
    if (opts.json !== undefined) {
      headers['Content-Type'] = opts.jsonContentType ?? 'application/json'
      body = JSON.stringify(opts.json)
    } else if (opts.body !== undefined) {
      headers['Content-Type'] = 'application/octet-stream'
      body = opts.body
    }

    const timeoutMs = opts.timeoutMs ?? this.requestTimeoutMs

    const retries = Math.max(0, opts.retries ?? 0)
    for (let attempt = 0; ; attempt++) {
      const controller = new AbortController()
      let timedOut = false
      const timer = setTimeout(() => {
        timedOut = true
        controller.abort()
      }, timeoutMs)
      const onAbort = () => controller.abort(opts.signal?.reason)
      opts.signal?.addEventListener('abort', onAbort, { once: true })
      try {
        const res = await fetch(url.toString(), { method, headers, body, signal: controller.signal })
        if (res.ok) return res
        if (attempt < retries && (res.status === 429 || res.status === 503)) {
          const delay = retryAfterMs(res.headers.get('Retry-After')) ?? Math.min(250 * 2 ** attempt, 2_000)
          await res.body?.cancel()
          await wait(delay, opts.signal)
          continue
        }
        throw await this.toError(res, method, url.pathname)
      } catch (err) {
        if (err instanceof SandboxError) throw err
        if (opts.signal?.aborted) throw abortReason(opts.signal)
        if (timedOut) {
          throw new TimeoutError(`Request timed out after ${timeoutMs} ms: ${method} ${url.pathname}`)
        }
        if (attempt < retries) {
          await wait(Math.min(250 * 2 ** attempt, 2_000), opts.signal)
          continue
        }
        throw new SandboxError(
          `Request failed: ${method} ${url} — ${err instanceof Error ? err.message : String(err)}`
        )
      } finally {
        clearTimeout(timer)
        opts.signal?.removeEventListener('abort', onAbort)
      }
    }
  }

  private async toError(res: Response, method: string, path: string): Promise<SandboxError> {
    let message = `${method} ${path} failed with status ${res.status}`
    let context: ApiErrorContext = { requestId: res.headers.get('X-Request-Id') ?? undefined }
    try {
      const text = await res.text()
      if (text) {
        try {
          const parsed = JSON.parse(text) as { error?: string } | ProblemDetails
          if (isProblem(parsed)) {
            context = { requestId: parsed.request_id, code: parsed.code, problem: parsed }
            message += `: ${parsed.detail ?? parsed.title}`
          } else {
            message += `: ${parsed.error ?? text}`
          }
        } catch {
          message += `: ${text}`
        }
      }
    } catch {
      // ignore body read failures; keep the status-only message
    }

    if (res.status === 401 || res.status === 403) return new AuthenticationError(message, res.status, context)
    if (res.status === 404) return new NotFoundError(message, res.status, context)
    if (res.status === 409) return new ConflictError(message, res.status, context)
    if (res.status === 429 || res.status === 503) {
      return new CapacityError(message, res.status, retryAfterMs(res.headers.get('Retry-After')), context)
    }
    return new SandboxError(message, res.status, context)
  }
}

/**
 * Parses a `Retry-After` header into milliseconds. The API sends delay-seconds
 * (`Retry-After: 5`); the HTTP-date form is accepted too, for completeness.
 */
function retryAfterMs(header: string | null): number | undefined {
  if (!header) return undefined
  const seconds = Number(header)
  if (Number.isFinite(seconds)) return Math.max(0, seconds * 1000)
  const at = Date.parse(header)
  if (Number.isNaN(at)) return undefined
  return Math.max(0, at - Date.now())
}

function isProblem(value: { error?: string } | ProblemDetails): value is ProblemDetails {
  return 'code' in value && 'request_id' in value && 'status' in value && 'title' in value
}

function abortReason(signal: AbortSignal): Error {
  return signal.reason instanceof Error ? signal.reason : new SandboxError('Request aborted')
}

function wait(ms: number, signal?: AbortSignal): Promise<void> {
  if (signal?.aborted) return Promise.reject(abortReason(signal))
  return new Promise((resolve, reject) => {
    const timer = setTimeout(resolve, ms)
    signal?.addEventListener('abort', () => {
      clearTimeout(timer)
      reject(abortReason(signal))
    }, { once: true })
  })
}
