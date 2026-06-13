import { ApiClient, CREATE_REQUEST_TIMEOUT_MS } from './client.js'
import { Commands } from './commands.js'
import { SandboxError } from './errors.js'
import { Files } from './files.js'
import { toSandboxInfo } from './types.js'
import type {
  ApiPortMapping,
  ApiSandbox,
  PortMapping,
  SandboxCreateOpts,
  SandboxInfo,
  SandboxOpts,
} from './types.js'

/** The guest port forwarded to the host at create time (the primary app port). */
const PRIMARY_GUEST_PORT = 3000

/**
 * A Firecracker microVM sandbox running Ubuntu 24.04 with Node 22, pnpm,
 * TypeScript, Python 3, and common build tooling. No app server runs by
 * default — guest port 3000 is forwarded for whatever you start there.
 *
 * Mirrors the e2b `Sandbox` API:
 *
 * ```ts
 * const sbx = await Sandbox.create({ timeoutMs: 300_000 })
 * await sbx.commands.run('node --version')
 * await sbx.files.write('/home/sandbox/server.js', code)
 * const host = sbx.getHost(3000)
 * const api = await sbx.exposePort(8000)
 * await sbx.kill()
 * ```
 */
export class Sandbox {
  /** Unique id of this sandbox. */
  readonly sandboxId: string
  /** Run commands inside the sandbox. */
  readonly commands: Commands
  /** Read, write, and list files inside the sandbox. */
  readonly files: Files
  /** Static info captured when the sandbox handle was created. */
  readonly info: SandboxInfo

  private readonly client: ApiClient
  /** Known guest → host port mappings, used by the synchronous getHost(). */
  private readonly portCache = new Map<number, number>()

  private constructor(client: ApiClient, info: SandboxInfo) {
    this.client = client
    this.info = info
    this.sandboxId = info.sandboxId
    this.commands = new Commands(client, info.sandboxId)
    this.files = new Files(client, info.sandboxId)
    this.portCache.set(PRIMARY_GUEST_PORT, info.hostPort)
  }

  /**
   * Creates a new sandbox and waits until it is ready (the API blocks
   * for roughly two seconds while the VM boots).
   *
   * @param opts API URL/key overrides (default to the `SANDBOX_API_URL` /
   *             `SANDBOX_API_KEY` environment variables) plus an optional
   *             `timeoutMs` after which the sandbox is auto-destroyed.
   */
  static async create(opts: SandboxCreateOpts = {}): Promise<Sandbox> {
    const client = new ApiClient(opts)
    const res = await client.request('POST', '/sandboxes', {
      timeoutMs: opts.requestTimeoutMs ?? CREATE_REQUEST_TIMEOUT_MS,
      ...(opts.timeoutMs !== undefined
        ? { json: { timeout_sec: Math.ceil(opts.timeoutMs / 1000) } }
        : {}),
    })
    const raw = (await res.json()) as ApiSandbox
    return new Sandbox(client, toSandboxInfo(raw))
  }

  /**
   * Connects to an existing running sandbox by id.
   *
   * @throws {NotFoundError} when no sandbox with that id exists.
   */
  static async connect(sandboxId: string, opts: SandboxOpts = {}): Promise<Sandbox> {
    const client = new ApiClient(opts)
    const res = await client.request('GET', `/sandboxes/${sandboxId}`)
    const raw = (await res.json()) as ApiSandbox
    return new Sandbox(client, toSandboxInfo(raw))
  }

  /**
   * Lists all running sandboxes.
   */
  static async list(opts: SandboxOpts = {}): Promise<SandboxInfo[]> {
    const client = new ApiClient(opts)
    const res = await client.request('GET', '/sandboxes')
    const raw = (await res.json()) as ApiSandbox[] | null
    return (raw ?? []).map(toSandboxInfo)
  }

  /**
   * Destroys a sandbox by id without needing a `Sandbox` instance.
   */
  static async kill(sandboxId: string, opts: SandboxOpts = {}): Promise<void> {
    const client = new ApiClient(opts)
    await client.request('DELETE', `/sandboxes/${sandboxId}`)
  }

  /**
   * Returns the `host:port` to reach a service running inside the sandbox
   * from the outside, e.g. `100.99.183.74:5200`.
   *
   * Synchronous: works for the primary guest port (always forwarded) and for
   * any port previously exposed through {@link exposePort} or seen via
   * {@link listPorts} on this instance.
   *
   * @param port Guest port (default 3000).
   * @throws {SandboxError} when the port has not been exposed yet.
   */
  getHost(port: number = PRIMARY_GUEST_PORT): string {
    const hostPort = this.portCache.get(port)
    if (hostPort === undefined) {
      throw new SandboxError(
        `Guest port ${port} is not forwarded to the host. Call \`await sandbox.exposePort(${port})\` ` +
          `first — only guest port ${PRIMARY_GUEST_PORT} (the primary app port) is forwarded automatically.`
      )
    }
    return `${this.client.apiHostname}:${hostPort}`
  }

  /**
   * Forwards a guest port to a dedicated host port (idempotent — exposing
   * the same port again returns the existing mapping).
   *
   * @param guestPort Port a service listens on inside the sandbox.
   * @returns The externally reachable `host:port` string.
   */
  async exposePort(guestPort: number): Promise<string> {
    const res = await this.client.request('POST', `/sandboxes/${this.sandboxId}/ports`, {
      json: { guest_port: guestPort },
    })
    const raw = (await res.json()) as ApiPortMapping
    this.portCache.set(raw.guest_port, raw.host_port)
    return `${this.client.apiHostname}:${raw.host_port}`
  }

  /**
   * Lists every forwarded port of this sandbox, including the always-present
   * primary mapping. Also refreshes the cache used by {@link getHost}.
   */
  async listPorts(): Promise<PortMapping[]> {
    const res = await this.client.request('GET', `/sandboxes/${this.sandboxId}/ports`)
    const raw = (await res.json()) as ApiPortMapping[] | null
    const mappings = (raw ?? []).map((m) => ({ guestPort: m.guest_port, hostPort: m.host_port }))
    for (const m of mappings) {
      this.portCache.set(m.guestPort, m.hostPort)
    }
    return mappings
  }

  /**
   * Sets (or clears) the sandbox's auto-destroy timeout, e2b-style. The new
   * timeout replaces any previous one and counts from now.
   *
   * @param timeoutMs Milliseconds until auto-destroy (rounded up to whole
   *                  seconds); `0` removes the timeout.
   */
  async setTimeout(timeoutMs: number): Promise<void> {
    const res = await this.client.request('POST', `/sandboxes/${this.sandboxId}/timeout`, {
      json: { timeout_sec: Math.ceil(timeoutMs / 1000) },
    })
    const raw = (await res.json()) as ApiSandbox
    this.info.expiresAt = raw.expires_at ? new Date(raw.expires_at) : undefined
  }

  /**
   * Destroys this sandbox and releases its resources on the host.
   */
  async kill(): Promise<void> {
    await this.client.request('DELETE', `/sandboxes/${this.sandboxId}`)
  }
}
