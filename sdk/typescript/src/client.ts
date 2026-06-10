import {
  AuthenticationError,
  NotFoundError,
  SandboxError,
  TimeoutError,
} from './errors.js'
import type { SandboxOpts } from './types.js'

/** Default timeout for ordinary API requests. */
export const DEFAULT_REQUEST_TIMEOUT_MS = 30_000
/** Timeout for `POST /sandboxes` — creation blocks until the VM is ready. */
export const CREATE_REQUEST_TIMEOUT_MS = 90_000

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
}

/**
 * Minimal HTTP plumbing for the websandbox REST API.
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
    const apiUrl = opts.apiUrl ?? process.env.WEBSANDBOX_API_URL
    if (!apiUrl) {
      throw new SandboxError(
        'Missing API URL: set the WEBSANDBOX_API_URL environment variable (e.g. http://100.99.183.74:8080) or pass { apiUrl } explicitly.'
      )
    }
    const apiKey = opts.apiKey ?? process.env.WEBSANDBOX_API_KEY
    if (!apiKey) {
      throw new AuthenticationError(
        'Missing API key: set the WEBSANDBOX_API_KEY environment variable or pass { apiKey } explicitly.'
      )
    }

    this.baseUrl = apiUrl.replace(/\/+$/, '')
    this.apiHostname = new URL(this.baseUrl).hostname
    this.apiKey = apiKey
    this.requestTimeoutMs = opts.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS
  }

  /**
   * Performs an authenticated request against the API and returns the raw
   * `Response`. Non-2xx responses are mapped to SDK error classes
   * ({@link AuthenticationError}, {@link NotFoundError}, {@link SandboxError});
   * client-side timeouts throw {@link TimeoutError}.
   */
  async request(method: string, path: string, opts: RequestOpts = {}): Promise<Response> {
    const url = new URL(this.baseUrl + path)
    for (const [k, v] of Object.entries(opts.query ?? {})) {
      url.searchParams.set(k, v)
    }

    const headers: Record<string, string> = {
      Authorization: `Bearer ${this.apiKey}`,
    }
    let body: FetchBody
    if (opts.json !== undefined) {
      headers['Content-Type'] = 'application/json'
      body = JSON.stringify(opts.json)
    } else if (opts.body !== undefined) {
      headers['Content-Type'] = 'application/octet-stream'
      body = opts.body
    }

    const timeoutMs = opts.timeoutMs ?? this.requestTimeoutMs

    let res: Response
    try {
      res = await fetch(url.toString(), {
        method,
        headers,
        body,
        signal: AbortSignal.timeout(timeoutMs),
      })
    } catch (err) {
      if (err instanceof Error && (err.name === 'TimeoutError' || err.name === 'AbortError')) {
        throw new TimeoutError(
          `Request timed out after ${timeoutMs} ms: ${method} ${url.pathname}`
        )
      }
      throw new SandboxError(
        `Request failed: ${method} ${url} — ${err instanceof Error ? err.message : String(err)}`
      )
    }

    if (!res.ok) {
      throw await this.toError(res, method, url.pathname)
    }
    return res
  }

  private async toError(res: Response, method: string, path: string): Promise<SandboxError> {
    let message = `${method} ${path} failed with status ${res.status}`
    try {
      const text = await res.text()
      if (text) {
        try {
          const parsed = JSON.parse(text) as { error?: string }
          message += `: ${parsed.error ?? text}`
        } catch {
          message += `: ${text}`
        }
      }
    } catch {
      // ignore body read failures; keep the status-only message
    }

    if (res.status === 401 || res.status === 403) return new AuthenticationError(message)
    if (res.status === 404) return new NotFoundError(message)
    return new SandboxError(message)
  }
}
