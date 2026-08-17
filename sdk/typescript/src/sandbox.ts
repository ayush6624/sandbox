import { ApiClient, CREATE_REQUEST_TIMEOUT_MS } from './client.js'
import { Commands } from './commands.js'
import { SandboxError } from './errors.js'
import { Files } from './files.js'
import { Pty } from './pty.js'
import { ClientSandbox, Operation, SandboxClient } from './v1.js'
import type { CreateManyOptions, CreateSandboxOptions, SandboxSource } from './v1.js'
import { toHostInfo, toSandboxInfo, toSnapshotInfo } from './types.js'
import type {
  ApiHostInfo,
  ApiPortMapping,
  ApiSandbox,
  ApiSnapshot,
  HostInfo,
  PortMapping,
  PortExposeOpts,
  RawPortMapping,
  SandboxCreateOpts,
  SandboxFanoutOpts,
  SandboxInfo,
  SandboxOpts,
  SandboxRestoreOpts,
  SnapshotInfo,
} from './types.js'

/**
 * Builds the JSON body shared by create/restore/fanout from the options they
 * have in common. Absent options are omitted rather than zeroed — `0` is a
 * meaningful value on both fields (clear the TTL / inherit the host's
 * hibernation default).
 */
function bringUpBody(opts: {
  ttlMs?: number
  idleTimeoutMs?: number
  timeoutMs?: number
  hibernateAfterMs?: number
}): Record<string, number | string> {
  const body: Record<string, number | string> = {}
  const ttlMs = opts.ttlMs ?? opts.timeoutMs
  const idleTimeoutMs = opts.idleTimeoutMs ?? opts.hibernateAfterMs
  if (ttlMs !== undefined) {
    body.timeout_sec = Math.ceil(ttlMs / 1000)
  }
  if (idleTimeoutMs !== undefined) {
    // -1 is the "never hibernate" sentinel, passed through unscaled.
    body.hibernate_after_sec =
      idleTimeoutMs < 0 ? -1 : Math.ceil(idleTimeoutMs / 1000)
  }
  return body
}

/**
 * A Firecracker microVM sandbox running Ubuntu 24.04 with Node 22, pnpm,
 * TypeScript, Python 3, and common build tooling. No app server runs by
 * default, and guest ports are private until explicitly exposed.
 *
 * Mirrors the e2b `Sandbox` API:
 *
 * ```ts
 * const sbx = await Sandbox.create({ timeoutMs: 300_000 })
 * await sbx.commands.run('node --version')
 * await sbx.files.write('/home/sandbox/server.js', code)
 * const host = await sbx.exposePort(3000)
 * const api = await sbx.exposePort(8000)
 * await sbx.terminate()
 * ```
 */
export class Sandbox {
  /** Unique id of this sandbox. */
  readonly sandboxId: string
  /** Run commands inside the sandbox. */
  readonly commands: Commands
  /** Read, write, and list files inside the sandbox. */
  readonly files: Files
  /** Interactive PTY shells inside the sandbox (WebSocket-backed). */
  readonly pty: Pty
  /** Static info captured when the sandbox handle was created. */
  readonly info: SandboxInfo

  private readonly client: ApiClient
  /** Known guest → host port mappings, used by the synchronous getHost(). */
  private readonly portCache = new Map<number, number>()
  /** Known public ingress URLs, used by the synchronous getUrl(). */
  private readonly urlCache = new Map<number, string>()

  private constructor(client: ApiClient, info: SandboxInfo) {
    this.client = client
    this.info = info
    this.sandboxId = info.sandboxId
    this.commands = new Commands(client, info.sandboxId)
    this.files = new Files(client, info.sandboxId)
    this.pty = new Pty(client, info.sandboxId)
    this.rememberPorts(info.ports ?? [])
  }

  /**
   * Creates a new sandbox and waits until it is ready to use. The server
   * normally serves this from a pre-booted golden snapshot (a few hundred
   * milliseconds); it falls back to a full cold boot (~2-3 s) when no
   * snapshot is available yet, e.g. right after a server restart — or when
   * `vcpus`/`memMib` is set, since resource overrides can't be served from
   * the golden snapshot (it bakes the template's resources).
   *
   * @param opts API URL/key overrides (default to the `SANDBOX_API_URL` /
   *             `SANDBOX_API_KEY` environment variables) plus an optional
   *             `timeoutMs` after which the sandbox is auto-destroyed, an
   *             optional `hibernateAfterMs` idle-hibernation override,
   *             optional `vcpus`/`memMib` resource overrides.
   * @throws {CapacityError} when the fleet has no free slot (retryable).
   */
  static async create(opts: SandboxCreateOpts = {}): Promise<Sandbox> {
    const client = new ApiClient(opts)
    const body = bringUpBody(opts)
    if (opts.name !== undefined) {
      body.name = opts.name
    }
    if (opts.vcpus !== undefined) {
      body.vcpus = opts.vcpus
    }
    if (opts.memMib !== undefined) {
      body.mem_mib = opts.memMib
    }
    if (opts.sshPubkey !== undefined) {
      body.ssh_pubkey = opts.sshPubkey
    }
    const res = await client.request('POST', '/sandboxes', {
      timeoutMs: opts.requestTimeoutMs ?? CREATE_REQUEST_TIMEOUT_MS,
      ...(Object.keys(body).length > 0 ? { json: body } : {}),
    })
    const raw = (await res.json()) as ApiSandbox
    return new Sandbox(client, toSandboxInfo(raw))
  }

  /**
   * Creates a sandbox through the resource-oriented v1 API. This facade keeps
   * the familiar static entry point while making the source explicit.
   */
  static async createFromSource(
    source: SandboxSource,
    opts: CreateSandboxOptions & SandboxOpts = {},
  ): Promise<ClientSandbox> {
    const client = new SandboxClient({
      baseUrl: opts.apiUrl,
      apiKey: opts.apiKey,
      requestTimeoutMs: opts.requestTimeoutMs,
    })
    return client.sandboxes.create({ ...opts, source })
  }

  /** Starts a typed batch-create operation through the v1 API. */
  static async createMany(opts: CreateManyOptions & SandboxOpts): Promise<Operation<ClientSandbox>> {
    const client = new SandboxClient({
      baseUrl: opts.apiUrl,
      apiKey: opts.apiKey,
      requestTimeoutMs: opts.requestTimeoutMs,
    })
    return client.sandboxes.createMany(opts)
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
   * Returns the host's template defaults and per-sandbox override limits:
   * the vCPUs/memory a sandbox runs with when created without overrides, the
   * accepted override bounds, and behavior flags. Against a fleet gateway the
   * answer comes from one live host (hosts share a template config).
   */
  static async hostInfo(opts: SandboxOpts = {}): Promise<HostInfo> {
    const client = new ApiClient(opts)
    const res = await client.request('GET', '/info')
    const raw = (await res.json()) as ApiHostInfo
    return toHostInfo(raw)
  }


  /**
   * Lists all sandboxes — `running` and `hibernated` alike (a hibernated
   * sandbox is still addressable; its next request wakes it).
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
  static async terminate(sandboxId: string, opts: SandboxOpts = {}): Promise<void> {
    const client = new ApiClient(opts)
    await client.request('DELETE', `/sandboxes/${sandboxId}`)
  }


  /**
   * Restores a brand-new sandbox from a snapshot, resuming it from the saved
   * memory + device state — running processes, memory contents, and disk all
   * come back exactly as they were at snapshot time. Use this (or
   * {@link Sandbox.fanout}) to resume prepared state; for a blank sandbox,
   * plain {@link Sandbox.create} is already snapshot-fast.
   *
   * The source sandbox the snapshot was taken from must no longer be running:
   * the snapshot reuses its guest IP and tap device, which would otherwise
   * collide. To run many restores of one snapshot side by side, use
   * {@link Sandbox.fanout} instead.
   *
   * `vcpus`/`memMib` are not sent: resources are baked into the snapshot when
   * it is taken, so a restore always runs with the source sandbox's resources.
   *
   * @param snapshotId Id returned by {@link Sandbox#snapshot}.
   * @param opts API overrides plus an optional `timeoutMs` auto-destroy and
   *             `hibernateAfterMs` idle-hibernation override.
   * @throws {ConflictError} when the snapshot's baked identity is still in use
   *                         by its source sandbox or an earlier restore.
   */
  /** @deprecated Use Sandbox.createFromSource({ snapshotId }, opts). */
  static async restore(snapshotId: string, opts: SandboxRestoreOpts = {}): Promise<Sandbox> {
    const client = new ApiClient(opts)
    const body = bringUpBody(opts)
    if (opts.name !== undefined) {
      body.name = opts.name
    }
    const res = await client.request('POST', `/snapshots/${snapshotId}/restore`, {
      timeoutMs: opts.requestTimeoutMs ?? CREATE_REQUEST_TIMEOUT_MS,
      ...(Object.keys(body).length > 0 ? { json: body } : {}),
    })
    const raw = (await res.json()) as ApiSandbox
    return new Sandbox(client, toSandboxInfo(raw))
  }

  /**
   * Fans out N identity-neutral clones from a single snapshot, concurrently.
   * Unlike {@link Sandbox.restore} (which reuses the snapshot's baked guest IP
   * and is therefore strictly 1-at-a-time), each clone is allocated a fresh
   * IP/tap/host-port from the pool and reidentifies itself from MMDS on resume,
   * so many clones of one snapshot run side by side. Each gets its own
   * copy-on-write rootfs, so writes are isolated.
   *
   * The source sandbox the snapshot was taken from must no longer be running.
   *
   * @param snapshotId Id returned by {@link Sandbox#snapshot}.
   * Fan-out is **partially successful by design**: the returned array holds
   * every clone that came up and may be shorter than `count` (failures are
   * logged server-side and their resources reclaimed). Check `.length`.
   *
   * @param count Number of clones to start (>= 1).
   * @param opts API overrides plus an optional `timeoutMs` auto-destroy and
   *             `hibernateAfterMs` idle-hibernation override, applied to every clone.
   * @returns One {@link Sandbox} per clone that came up successfully.
   */
  /** @deprecated Use Sandbox.createMany({ count, source: { snapshotId }, ...opts }). */
  static async fanout(snapshotId: string, count: number, opts: SandboxFanoutOpts = {}): Promise<Sandbox[]> {
    if (!Number.isInteger(count) || count < 1) throw new Error('count must be a positive integer')
    const client = new ApiClient(opts)
    const res = await client.request('POST', `/snapshots/${snapshotId}/fanout`, {
      // The server holds the request open until every clone is up; scale with count.
      timeoutMs: opts.requestTimeoutMs ?? Math.max(CREATE_REQUEST_TIMEOUT_MS, count * 3_000),
      json: { count, ...bringUpBody(opts) },
    })
    const raw = (await res.json()) as ApiSandbox[]
    return raw.map((r) => new Sandbox(client, toSandboxInfo(r)))
  }

  /**
   * Lists all saved snapshots on the host.
   */
  static async listSnapshots(opts: SandboxOpts = {}): Promise<SnapshotInfo[]> {
    const client = new ApiClient(opts)
    const res = await client.request('GET', '/snapshots')
    const raw = (await res.json()) as ApiSnapshot[] | null
    return (raw ?? []).map(toSnapshotInfo)
  }

  /**
   * Deletes a snapshot and its on-disk artifacts.
   */
  static async deleteSnapshot(snapshotId: string, opts: SandboxOpts = {}): Promise<void> {
    const client = new ApiClient(opts)
    await client.request('DELETE', `/snapshots/${snapshotId}`)
  }

  /**
   * Sets a snapshot's display name; an empty string clears it.
   */
  static async renameSnapshot(
    snapshotId: string,
    name: string,
    opts: SandboxOpts = {}
  ): Promise<SnapshotInfo> {
    const client = new ApiClient(opts)
    const res = await client.request('POST', `/snapshots/${snapshotId}/rename`, {
      json: { name },
    })
    const raw = (await res.json()) as ApiSnapshot
    return toSnapshotInfo(raw)
  }

  /**
   * Returns the `host:port` to reach a service running inside the sandbox
   * from the outside, e.g. `100.99.183.74:5200`.
   *
   * Synchronous: works for any port previously exposed through
   * {@link exposePort} or seen via {@link listPorts} on this instance.
   *
   * @param port Guest port previously exposed on this instance.
   * @throws {SandboxError} when the port has not been exposed yet.
   */
  getHost(port: number): string {
    const hostPort = this.portCache.get(port)
    if (hostPort === undefined) {
      throw new SandboxError(
        `Guest port ${port} is not forwarded to the host. Call \`await sandbox.exposePort(${port})\` first.`
      )
    }
    return `${this.hostname}:${hostPort}`
  }

  /**
   * Returns the public ingress URL for an exposed guest port.
   *
   * Synchronous after create/connect/refresh when the server included `ports`,
   * or after {@link exposePort}/{@link listPorts} on this instance.
   */
  getUrl(port: number): string {
    const url = this.urlCache.get(port)
    if (url === undefined) {
      throw new SandboxError(
        `Guest port ${port} has no public ingress URL. Expose it and configure the worker ingress_domain first.`
      )
    }
    return url
  }

  /**
   * Hostname where this sandbox's forwarded ports live: the owning host in
   * fleet mode (the gateway annotates responses with it), else the API host.
   */
  private get hostname(): string {
    return this.info.hostAddr ?? this.client.apiHostname
  }

  /**
   * Re-reads this sandbox from the API and updates {@link info} in place, so
   * references already handed out see the fresh values.
   *
   * {@link info} is otherwise a snapshot from when the handle was made, and
   * `status` in particular drifts on its own: the idle reaper can hibernate
   * the sandbox, and any later command wakes it again. Call this before
   * trusting `status`, `expiresAt`, or `hostAddr`.
   *
   * @throws {NotFoundError} when the sandbox no longer exists (killed or expired).
   */
  async refresh(): Promise<SandboxInfo> {
    const res = await this.client.request('GET', `/sandboxes/${this.sandboxId}`)
    const fresh = toSandboxInfo((await res.json()) as ApiSandbox)
    // Drop fields the server no longer reports (e.g. a cleared TTL) before
    // copying the new ones over, so `info` never keeps a stale value.
    const bag = this.info as unknown as Record<string, unknown>
    for (const key of Object.keys(bag)) {
      if (!(key in fresh)) delete bag[key]
    }
    Object.assign(this.info, fresh)
    this.rememberPorts(fresh.ports ?? [])
    return this.info
  }

  /**
   * Forwards a guest port to a dedicated host port (idempotent — exposing
   * the same port again returns the existing mapping).
   *
   * @param guestPort Port a service listens on inside the sandbox.
   * @returns The externally reachable `host:port` string.
   */
  async exposePort(guestPort: number, opts: PortExposeOpts = {}): Promise<string> {
    const body: { guest_port: number; host_port?: boolean } = { guest_port: guestPort }
    if (opts.hostPort !== undefined) body.host_port = opts.hostPort
    const res = await this.client.request('POST', `/sandboxes/${this.sandboxId}/ports`, {
      json: body,
    })
    const raw = (await res.json()) as ApiPortMapping
    if (raw.host_port !== undefined) {
      this.portCache.set(raw.guest_port, raw.host_port)
    }
    if (raw.url) this.urlCache.set(raw.guest_port, raw.url)
    if (raw.host_port !== undefined) return `${this.hostname}:${raw.host_port}`
    if (raw.url) return raw.url
    throw new SandboxError('Server created a URL-only exposure without returning its public URL')
  }

  /** Allocates a fleet-wide public raw TCP address for a non-HTTP service. SSH is CLI-owned. */
  async exposeRawPort(guestPort: number): Promise<RawPortMapping> {
    const res = await this.client.request('POST', `/sandboxes/${this.sandboxId}/raw-ports`, {
      json: { guest_port: guestPort },
    })
    const raw = (await res.json()) as {
      guest_port: number
      public_host: string
      public_port: number
    }
    return {
      guestPort: raw.guest_port,
      publicHost: raw.public_host,
      publicPort: raw.public_port,
      address: `${raw.public_host}:${raw.public_port}`,
    }
  }

  /** Removes an exposure and releases its worker-local and raw public ports. */
  async unexposePort(guestPort: number): Promise<void> {
    await this.client.request('DELETE', `/sandboxes/${this.sandboxId}/ports/${guestPort}`)
    this.portCache.delete(guestPort)
    this.urlCache.delete(guestPort)
  }

  /**
   * Lists every explicitly forwarded port of this sandbox. Also refreshes the
   * cache used by {@link getHost}.
   */
  async listPorts(): Promise<PortMapping[]> {
    const res = await this.client.request('GET', `/sandboxes/${this.sandboxId}/ports`)
    const raw = (await res.json()) as ApiPortMapping[] | null
    const mappings = (raw ?? []).map((m) => {
      const mapping: PortMapping = { guestPort: m.guest_port }
      if (m.host_port !== undefined) mapping.hostPort = m.host_port
      if (m.mode !== undefined) mapping.mode = m.mode
      if (m.url !== undefined) mapping.url = m.url
      if (m.public_port !== undefined) mapping.publicPort = m.public_port
      return mapping
    })
    this.rememberPorts(mappings)
    return mappings
  }

  private rememberPorts(mappings: PortMapping[]): void {
    for (const m of mappings) {
      if (m.hostPort !== undefined) this.portCache.set(m.guestPort, m.hostPort)
      if (m.url !== undefined) this.urlCache.set(m.guestPort, m.url)
    }
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
   * Sets this sandbox's display name; an empty string clears it. The name is
   * a free-form label shown in listings — not unique and not a lookup key.
   */
  async rename(name: string): Promise<void> {
    const res = await this.client.request('POST', `/sandboxes/${this.sandboxId}/rename`, {
      json: { name },
    })
    const raw = (await res.json()) as ApiSandbox
    this.info.name = raw.name
  }

  /**
   * Captures a snapshot of this sandbox (Firecracker memory + device state plus
   * a frozen rootfs copy) that can later be restored into a new sandbox with
   * {@link Sandbox.restore}. The sandbox is paused briefly during capture and
   * then keeps running.
   *
   * @param opts Optional `name`, a display label for the snapshot.
   * @returns Metadata for the saved snapshot, including its `snapshotId`.
   */
  async snapshot(opts: { name?: string } = {}): Promise<SnapshotInfo> {
    const res = await this.client.request('POST', `/sandboxes/${this.sandboxId}/snapshot`, {
      timeoutMs: CREATE_REQUEST_TIMEOUT_MS,
      ...(opts.name !== undefined ? { json: { name: opts.name } } : {}),
    })
    const raw = (await res.json()) as ApiSnapshot
    return toSnapshotInfo(raw)
  }

  /**
   * Freezes this sandbox to disk immediately (memory snapshot, VM torn down),
   * releasing its slot on the host — the explicit version of what the idle
   * reaper does after the hibernation window. While frozen, `status` reads
   * `"hibernated"`; the next command/file/shell request wakes it
   * transparently, with all processes resuming where they stopped.
   */
  /** Pauses this sandbox while preserving its identity and runtime state. */
  async pause(): Promise<void> {
    const res = await this.client.request('POST', `/sandboxes/${this.sandboxId}/hibernate`, {
      timeoutMs: CREATE_REQUEST_TIMEOUT_MS,
    })
    const raw = (await res.json()) as ApiSandbox
    this.info.status = raw.status
  }

  /** Explicitly resumes a paused sandbox without changing its identity. */
  async resume(): Promise<void> {
    const res = await this.client.request('POST', `/sandboxes/${this.sandboxId}/resume`, {
      timeoutMs: CREATE_REQUEST_TIMEOUT_MS,
    })
    const raw = (await res.json()) as ApiSandbox
    this.info.status = raw.status
  }

  /**
   * Destroys this sandbox and releases its resources on the host.
   */
  async terminate(): Promise<void> {
    await this.client.request('DELETE', `/sandboxes/${this.sandboxId}`)
  }
}
