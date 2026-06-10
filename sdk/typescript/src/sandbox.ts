import { ApiClient, CREATE_REQUEST_TIMEOUT_MS } from './client.js'
import { Commands } from './commands.js'
import { SandboxError } from './errors.js'
import { Files } from './files.js'
import { toSandboxInfo } from './types.js'
import type { ApiSandbox, SandboxInfo, SandboxOpts } from './types.js'

/** The only guest port forwarded to the host (the Vite dev server). */
const FORWARDED_GUEST_PORT = 5173

/**
 * A Firecracker microVM sandbox running Ubuntu 24.04 with Node 22, pnpm,
 * and a Vite React-TS app served on guest port 5173.
 *
 * Mirrors the e2b `Sandbox` API:
 *
 * ```ts
 * const sbx = await Sandbox.create()
 * await sbx.commands.run('node --version')
 * await sbx.files.write('/home/sandbox/app/src/App.tsx', code)
 * const host = sbx.getHost(5173)
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

  private constructor(client: ApiClient, info: SandboxInfo) {
    this.client = client
    this.info = info
    this.sandboxId = info.sandboxId
    this.commands = new Commands(client, info.sandboxId)
    this.files = new Files(client, info.sandboxId)
  }

  /**
   * Creates a new sandbox and waits until it is ready (the API blocks
   * for roughly two seconds while the VM boots).
   *
   * @param opts API URL/key overrides; both default to the
   *             `WEBSANDBOX_API_URL` / `WEBSANDBOX_API_KEY` environment variables.
   */
  static async create(opts: SandboxOpts = {}): Promise<Sandbox> {
    const client = new ApiClient(opts)
    const res = await client.request('POST', '/sandboxes', {
      timeoutMs: opts.requestTimeoutMs ?? CREATE_REQUEST_TIMEOUT_MS,
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
   * Only guest port 5173 (the Vite dev server) is forwarded by the host.
   *
   * @param port Guest port (default 5173).
   * @throws {SandboxError} for any port other than 5173.
   */
  getHost(port: number = FORWARDED_GUEST_PORT): string {
    if (port !== FORWARDED_GUEST_PORT) {
      throw new SandboxError(
        `Only guest port ${FORWARDED_GUEST_PORT} (the Vite dev server) is forwarded to the host; ` +
          `got port ${port}. Other ports are not reachable from outside the sandbox.`
      )
    }
    return `${this.client.apiHostname}:${this.info.hostPort}`
  }

  /**
   * Destroys this sandbox and releases its resources on the host.
   */
  async kill(): Promise<void> {
    await this.client.request('DELETE', `/sandboxes/${this.sandboxId}`)
  }
}
