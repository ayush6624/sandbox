import { randomUUID } from 'node:crypto'

import { ApiClient, CREATE_REQUEST_TIMEOUT_MS } from './client.js'
import { Commands } from './commands.js'
import { CreateAcceptanceError, SandboxError, TimeoutError } from './errors.js'
import type { ProblemDetails } from './errors.js'
import { Files } from './files.js'
import { Pty } from './pty.js'
import type { components } from './generated/api-v1.js'

type ApiSandbox = components['schemas']['Sandbox']
type ApiSnapshot = components['schemas']['Snapshot']
type ApiTemplate = components['schemas']['Template']
type ApiOperation = components['schemas']['Operation']
type ApiCreateProgress = components['schemas']['CreateProgress']
type ApiCreateStageMark = components['schemas']['CreateStageMark']
type ApiPortForward = components['schemas']['PortForward']
type ApiCreate = components['schemas']['CreateSandboxRequest']
type ApiUpdate = components['schemas']['UpdateSandboxRequest']
type ApiUsageReport = components['schemas']['UsageReport']
type ApiUsageInterval = components['schemas']['UsageInterval']

export interface SandboxClientOptions {
  baseUrl?: string
  apiKey?: string
  requestTimeoutMs?: number
  /** Safe retry count for reads and idempotent mutations (default 2). */
  maxRetries?: number
}

export type SandboxSource =
  | { type?: 'default' }
  | { templateId: string }
  | { snapshotId: string }
  | { type: 'template' | 'snapshot'; id: string }

export interface SandboxResources {
  vcpus: number
  memoryMib: number
}

type ApiResourceOverrides = components['schemas']['ResourceOverrides']

export type SandboxResourceOverrides = {
  [Key in keyof ApiResourceOverrides as Key extends 'vcpu' ? 'vcpus' : 'memoryMib']: ApiResourceOverrides[Key]
}

export interface CreateSandboxOptions {
  name?: string
  source?: SandboxSource
  ttlMs?: number
  idleTimeoutMs?: number
  resources?: SandboxResourceOverrides
  metadata?: Record<string, string>
  requestTimeoutMs?: number
  idempotencyKey?: string
  signal?: AbortSignal
}

export interface CreateManyOptions extends CreateSandboxOptions {
  count: number
  maxParallelism?: number
}

export interface UpdateSandboxOptions {
  name?: string
  ttlMs?: number
  idleTimeoutMs?: number
  metadata?: Record<string, string>
  idempotencyKey?: string
  signal?: AbortSignal
}

export interface ListSandboxesOptions {
  pageSize?: number
  status?: 'running' | 'paused'
  sourceType?: 'default' | 'template' | 'snapshot'
  createdAfter?: Date
  createdBefore?: Date
  metadata?: Record<string, string>
  signal?: AbortSignal
}

export interface ListOptions {
  pageSize?: number
  signal?: AbortSignal
}

export interface WaitOptions {
  pollIntervalMs?: number
  timeoutMs?: number
  signal?: AbortSignal
}

export interface SnapshotOptions {
  name?: string
  retentionMs?: number
  idempotencyKey?: string
  signal?: AbortSignal
}

export interface SnapshotResource {
  id: string
  name?: string
  sourceSandboxId: string
  state: 'local' | 'durable'
  /** Present only on the host that owns an unfinished original capture. */
  upload?: SnapshotUploadResource
  createdAt: Date
  expiresAt?: Date
}

/** Progress for an original snapshot capture becoming durable. */
export type SnapshotUploadResource = Omit<NonNullable<ApiSnapshot['upload']>, 'next_attempt_at'> & {
  nextAttemptAt?: Date
}

export interface TemplateResource {
  id: string
  revision: string
  resources: SandboxResources
  warmTarget: number
}

export interface PortForwardResource {
  id: string
  sandboxId: string
  guestPort: number
  /** Worker-local host port. Absent for a URL-only exposure — switch on {@link mode}. */
  hostPort?: number
  /** How this port is reachable. */
  mode: 'host_port' | 'url' | 'both' | 'raw'
  /** Public ingress URL; present only when the worker has an ingress domain configured. */
  url?: string
  /** Fleet-wide public TCP port, when raw TCP access is allocated. */
  publicPort?: number
  /** Fleet-wide public hostname, when raw TCP access is allocated. */
  publicHost?: string
  /** Ready-to-dial `publicHost:publicPort`, when raw TCP access is allocated. */
  address?: string
  status: 'active'
}

export interface RawPortForwardResource extends PortForwardResource {
  mode: 'raw'
  publicPort: number
  publicHost: string
  address: string
}

/** Options for creating a port forward. */
export interface PortForwardCreateOptions extends RequestControl {
  /**
   * Whether to also reserve a worker-local host port: `true` for
   * `host:port` plus an ingress URL, `false` for URL-only (which consumes no
   * host-port slot). Omitted follows the worker's own default. URL-only
   * exposure needs a worker with an ingress domain configured.
   */
  hostPort?: boolean
  /** Allocate a durable fleet-wide public TCP address. Mutually exclusive with hostPort. */
  mode?: 'raw'
}

export interface CreateCoordination extends Omit<components['schemas']['CreateCoordination'], 'updated_at'> {
  updatedAt?: Date
}

export interface CreateStageMark extends Omit<ApiCreateStageMark, 'started_at' | 'completed_at'> {
  startedAt: Date
  completedAt?: Date
}

export interface CreateCompletedStageMark extends Omit<CreateStageMark, 'completedAt'> {
  completedAt: Date
}

export interface CreateWorkerProgress extends Omit<components['schemas']['CreateWorkerProgress'], 'current' | 'last_completed' | 'observed_at'> {
  current: CreateStageMark
  lastCompleted?: CreateCompletedStageMark
  observedAt: Date
}

export interface CreateProgress extends Omit<ApiCreateProgress, 'coordination' | 'worker'> {
  coordination: CreateCoordination
  worker?: CreateWorkerProgress
}

export interface BatchResult<T> {
  index: number
  value?: T
  error?: ProblemDetails
  progress?: CreateProgress
}

export interface OperationState<T> {
  id: string
  type: ApiOperation['type']
  requestId?: ApiOperation['request_id']
  status: 'pending' | 'running' | 'succeeded' | 'partially_succeeded' | 'failed'
  requested: number
  succeeded: number
  failed: number
  results: Array<BatchResult<T>>
  createdAt: Date
  completedAt?: Date
}

/**
 * One billable span: the time a single VM served a sandbox. Pausing and
 * resuming produces two intervals, because it runs two VMs; the paused span in
 * between bills nothing.
 */
export interface UsageIntervalResource {
  /** Line item identity, `<sandboxId>:<sequence>`. Stable, and never reused. */
  id: string
  sandboxId: string
  /** Counts a sandbox's intervals from 1; a resume opens the next. */
  sequence: number
  state: 'open' | 'closed'
  resources: SandboxResources
  startedAt: Date
  /** Absent while the interval is open. */
  endedAt?: Date
  /**
   * Billable span. An open interval is measured to its last heartbeat, never
   * to "now", so the number is reproducible and an outage cannot be billed.
   */
  durationSeconds: number
  /** Billed: allocated vCPUs × duration. */
  vcpuSeconds: number
  /** Billed: allocated MiB × duration. */
  memoryMibSeconds: number
  /**
   * Host CPU actually consumed. Recorded for transparency and NOT the billing
   * base — CPU is deliberately oversubscribed.
   */
  cpuSeconds: number
  endReason?: 'destroy' | 'hibernate' | 'expire' | 'shutdown' | 'vm_exit' | 'crash'
  /**
   * The sandbox's labels while this interval was accruing. An open interval
   * tracks the sandbox's current labels; a closed one is history and is never
   * rewritten.
   */
  metadata: Record<string, string>
}

export interface UsageTotals {
  intervals: number
  openIntervals: number
  durationSeconds: number
  vcpuSeconds: number
  memoryMibSeconds: number
  cpuSeconds: number
}

export interface UsageReport {
  intervals: UsageIntervalResource[]
  /** Covers the whole selection, not just the returned page. */
  totals: UsageTotals
  window: {
    from?: Date
    to?: Date
    /** Intervals overlapping the window are included whole, never clipped. */
    selection: 'overlap'
  }
  coverage: {
    /** How many hosts answered. A count, not identities. */
    hostsReporting: number
    /**
     * Always `live_hosts`: usage from a host that no longer exists survives
     * only in the deployment's durability bucket, which is the billing record
     * of truth. This API is for dashboards and debugging.
     */
    scope: 'live_hosts'
    /** A host returned fewer rows than it holds; totals are unaffected. */
    truncated: boolean
  }
  nextPageToken?: string
}

export interface UsageQueryOptions {
  /** Also matches sandboxes that have been deleted. */
  sandboxId?: string
  /** Selects intervals overlapping [from, to). */
  from?: Date
  to?: Date
  pageSize?: number
  pageToken?: string
  signal?: AbortSignal
}

export interface SshInstructions {
  /** The only supported user-facing SSH command. */
  command: string
  /** Short copy suitable for a web UI, terminal hint, or documentation. */
  description: string
}

interface Page<T> {
  items: T[]
  nextPageToken?: string
}

interface RequestControl {
  timeoutMs?: number
  signal?: AbortSignal
  idempotencyKey?: string
}

class V1Transport {
  readonly http: ApiClient
  readonly maxRetries: number

  constructor(options: SandboxClientOptions) {
    this.http = new ApiClient({
      apiUrl: options.baseUrl,
      apiKey: options.apiKey,
      requestTimeoutMs: options.requestTimeoutMs,
    })
    this.maxRetries = options.maxRetries ?? 2
  }

  async get<T>(path: string, query: Record<string, string> = {}, signal?: AbortSignal, timeoutMs?: number): Promise<T> {
    signal?.throwIfAborted()
    if (timeoutMs === undefined) {
      const response = await this.http.request('GET', path, { query, signal, retries: this.maxRetries })
      return response.json() as Promise<T>
    }

    // ApiClient bounds individual fetch attempts. This outer signal bounds the
    // complete poll instead: retry delays and JSON body consumption included.
    const deadline = new AbortController()
    let timedOut = false
    const timer = setTimeout(() => {
      timedOut = true
      deadline.abort()
    }, timeoutMs)
    const relayAbort = () => deadline.abort(signal?.reason)
    signal?.addEventListener('abort', relayAbort, { once: true })
    let response: Response | undefined
    try {
      response = await this.http.request('GET', path, {
        query,
        signal: deadline.signal,
        timeoutMs,
        retries: this.maxRetries,
      })
      return await readJSONWithinDeadline<T>(response, deadline.signal)
    } catch (error) {
      // A response can have resolved its headers while its JSON body stalls.
      // Cancel it explicitly so its socket/stream is not retained after the
      // deadline aborts the enclosing poll.
      await response?.body?.cancel().catch(() => {})
      if (timedOut) throw new TimeoutError(`Request timed out after ${timeoutMs} ms: GET ${path}`)
      throw error
    } finally {
      clearTimeout(timer)
      signal?.removeEventListener('abort', relayAbort)
    }
  }

  async createAsync(body: ApiCreate, control: RequestControl): Promise<ApiOperation> {
    control.signal?.throwIfAborted()
    const idempotencyKey = control.idempotencyKey ?? randomUUID()
    const timeoutMs = control.timeoutMs ?? CREATE_REQUEST_TIMEOUT_MS
    const deadline = new AbortController()
    let timedOut = false
    const timer = setTimeout(() => {
      timedOut = true
      deadline.abort()
    }, timeoutMs)
    const relayAbort = () => deadline.abort(control.signal?.reason)
    control.signal?.addEventListener('abort', relayAbort, { once: true })
    let response: Response | undefined
    try {
      response = await this.http.request('POST', '/v1/sandbox-creations', {
        json: body,
        headers: { 'Idempotency-Key': idempotencyKey },
        redirect: 'manual',
        timeoutMs,
        signal: deadline.signal,
        retries: this.maxRetries,
      })
      if (response.status !== 202) throw new SandboxError(`Expected HTTP 202 acceptance, received ${response.status}`)
      return parseCreateAcceptance(await readJSONWithinDeadline<unknown>(response, deadline.signal))
    } catch (error) {
      void response?.body?.cancel().catch(() => {})
      if (error instanceof SandboxError && error.status !== undefined && error.status >= 400 && error.status < 500) throw error
      const cause = timedOut ? new TimeoutError(`Create acceptance timed out after ${timeoutMs} ms`) : error
      throw new CreateAcceptanceError(idempotencyKey, cause)
    } finally {
      clearTimeout(timer)
      control.signal?.removeEventListener('abort', relayAbort)
    }
  }

  async mutate<T>(method: 'POST' | 'PATCH' | 'DELETE', path: string, body: unknown, control: RequestControl = {}): Promise<T> {
    const response = await this.http.request(method, path, {
      ...(body === undefined ? {} : { json: body }),
      ...(method === 'PATCH' ? { jsonContentType: 'application/merge-patch+json' } : {}),
      headers: { 'Idempotency-Key': control.idempotencyKey ?? randomUUID() },
      timeoutMs: control.timeoutMs,
      signal: control.signal,
      retries: this.maxRetries,
    })
    if (response.status === 204) return undefined as T
    return response.json() as Promise<T>
  }
}

/** A v1 sandbox resource plus the existing command, file, and PTY capabilities. */
export class ClientSandbox {
  readonly commands: Commands
  readonly files: Files
  readonly pty: Pty
  private raw: ApiSandbox

  constructor(private readonly transport: V1Transport, raw: ApiSandbox) {
    this.raw = raw
    this.commands = new Commands(transport.http, raw.id)
    this.files = new Files(transport.http, raw.id)
    this.pty = new Pty(transport.http, raw.id)
  }

  get id(): string { return this.raw.id }
  get sandboxId(): string { return this.raw.id }
  get name(): string | undefined { return this.raw.name }
  get status(): ApiSandbox['status'] { return this.raw.status }
  get source(): ApiSandbox['source'] { return this.raw.source }
  get metadata(): Readonly<Record<string, string>> { return this.raw.metadata }
  get resources(): SandboxResources {
    return { vcpus: this.raw.resources.vcpu, memoryMib: this.raw.resources.memory_mib }
  }
  get ttlMs(): number | undefined {
    return this.raw.lifecycle.ttl_seconds === undefined ? undefined : this.raw.lifecycle.ttl_seconds * 1000
  }
  get idleTimeoutMs(): number | undefined {
    const seconds = this.raw.lifecycle.idle_timeout_seconds
    return seconds === undefined || seconds === -1 ? seconds : seconds * 1000
  }
  get createdAt(): Date { return new Date(this.raw.created_at) }
  get expiresAt(): Date | undefined { return this.raw.expires_at ? new Date(this.raw.expires_at) : undefined }

  /**
   * User-facing SSH guidance. Endpoint allocation, key authorization, and
   * host-key handling belong to the CLI rather than the SDK.
   */
  get sshInstructions(): SshInstructions {
    return {
      command: `sandbox ssh ${this.id}`,
      description: 'Install the sandbox CLI, set SANDBOX_API_URL and SANDBOX_API_KEY, then run this command.',
    }
  }

  async refresh(signal?: AbortSignal): Promise<this> {
    this.raw = await this.transport.get<ApiSandbox>(`/v1/sandboxes/${encodeURIComponent(this.id)}`, {}, signal)
    return this
  }

  async update(options: UpdateSandboxOptions): Promise<this> {
    const body: ApiUpdate = {}
    if (options.name !== undefined) body.name = options.name
    if (options.metadata !== undefined) body.metadata = options.metadata
    if (options.ttlMs !== undefined || options.idleTimeoutMs !== undefined) {
      body.lifecycle = {}
      if (options.ttlMs !== undefined) body.lifecycle.ttl_seconds = millisecondsToSeconds(options.ttlMs, 'ttlMs')
      if (options.idleTimeoutMs !== undefined) body.lifecycle.idle_timeout_seconds = idleMillisecondsToSeconds(options.idleTimeoutMs)
    }
    this.raw = await this.transport.mutate<ApiSandbox>('PATCH', `/v1/sandboxes/${encodeURIComponent(this.id)}`, body, options)
    return this
  }

  async pause(control: RequestControl = {}): Promise<this> {
    this.raw = await this.transport.mutate<ApiSandbox>('POST', `/v1/sandboxes/${encodeURIComponent(this.id)}:pause`, undefined, control)
    return this
  }

  async resume(control: RequestControl = {}): Promise<this> {
    this.raw = await this.transport.mutate<ApiSandbox>('POST', `/v1/sandboxes/${encodeURIComponent(this.id)}:resume`, undefined, control)
    return this
  }

  async terminate(control: RequestControl = {}): Promise<void> {
    await this.transport.mutate<void>('DELETE', `/v1/sandboxes/${encodeURIComponent(this.id)}`, undefined, control)
  }

  /** @deprecated Use pause(). */
  async hibernate(control: RequestControl = {}): Promise<this> { return this.pause(control) }
  /** @deprecated Use terminate(). */
  async kill(control: RequestControl = {}): Promise<void> { return this.terminate(control) }

  async createSnapshot(options: SnapshotOptions = {}): Promise<SnapshotResource> {
    const body: { name?: string; retention_seconds?: number } = {}
    if (options.name !== undefined) body.name = options.name
    if (options.retentionMs !== undefined) body.retention_seconds = millisecondsToSeconds(options.retentionMs, 'retentionMs')
    const raw = await this.transport.mutate<ApiSnapshot>(
      'POST', `/v1/sandboxes/${encodeURIComponent(this.id)}/snapshots`, body, {
        ...options,
        timeoutMs: CREATE_REQUEST_TIMEOUT_MS,
      },
    )
    return snapshotFromApi(raw)
  }

  async createPortForward(
    guestPort: number, opts: PortForwardCreateOptions = {},
  ): Promise<PortForwardResource> {
    const raw = await this.transport.mutate<ApiPortForward>(
      'POST', `/v1/sandboxes/${encodeURIComponent(this.id)}/port-forwards`,
      portForwardBody(guestPort, opts), opts,
    )
    return portForwardFromApi(raw)
  }

  /** Allocates a durable public TCP address for a non-HTTP service. SSH is CLI-owned. */
  async createRawPortForward(
    guestPort: number, control: RequestControl = {},
  ): Promise<RawPortForwardResource> {
    const raw = await this.transport.mutate<ApiPortForward>(
      'POST', `/v1/sandboxes/${encodeURIComponent(this.id)}/port-forwards`,
      { guest_port: guestPort, mode: 'raw' }, control,
    )
    return rawPortForwardFromApi(raw)
  }

  async listPortForwards(signal?: AbortSignal): Promise<PortForwardResource[]> {
    const raw = await this.transport.get<{ port_forwards: ApiPortForward[] }>(
      `/v1/sandboxes/${encodeURIComponent(this.id)}/port-forwards`, {}, signal,
    )
    return raw.port_forwards.map(portForwardFromApi)
  }

  /**
   * This sandbox's billable usage. Available while the sandbox exists; after
   * termination, read it from `client.usage.report({ sandboxId })`.
   */
  async usage(options: Omit<UsageQueryOptions, 'sandboxId'> = {}): Promise<UsageReport> {
    return usageReportFromApi(await this.transport.get<ApiUsageReport>(
      `/v1/sandboxes/${encodeURIComponent(this.id)}/usage`, usageQuery(options), options.signal,
    ))
  }
}

export class Operation<T> {
  private raw: ApiOperation

  constructor(
    private readonly transport: V1Transport,
    raw: ApiOperation,
    private readonly mapValue: (raw: ApiSandbox) => T,
  ) { this.raw = raw }

  get id(): string { return this.raw.id }
  get done(): boolean { return this.raw.completed_at !== undefined }
  get state(): OperationState<T> { return operationFromApi(this.raw, this.mapValue) }

  async refresh(signal?: AbortSignal): Promise<this> {
    this.raw = await this.transport.get<ApiOperation>(`/v1/operations/${encodeURIComponent(this.id)}`, {}, signal)
    return this
  }

  async wait(options: WaitOptions = {}): Promise<OperationState<T>> {
    const pollIntervalMs = options.pollIntervalMs ?? 500
    const timeoutMs = options.timeoutMs ?? CREATE_REQUEST_TIMEOUT_MS
    const started = Date.now()
    while (!this.done) {
      options.signal?.throwIfAborted()
      const remaining = timeoutMs - (Date.now() - started)
      if (remaining <= 0) throw new TimeoutError(`Operation ${this.id} did not complete within ${timeoutMs} ms`)
      await wait(Math.min(pollIntervalMs, remaining), options.signal)
      const requestTimeoutMs = timeoutMs - (Date.now() - started)
      if (requestTimeoutMs <= 0) throw new TimeoutError(`Operation ${this.id} did not complete within ${timeoutMs} ms`)
      this.raw = await this.transport.get<ApiOperation>(
        `/v1/operations/${encodeURIComponent(this.id)}`, {}, options.signal, requestTimeoutMs,
      )
    }
    return this.state
  }
}

export class SandboxClient {
  readonly sandboxes: SandboxesCollection
  readonly snapshots: SnapshotsCollection
  readonly templates: TemplatesCollection
  readonly operations: OperationsCollection
  readonly portForwards: PortForwardsCollection
  readonly usage: UsageCollection
  private readonly transport: V1Transport

  constructor(options: SandboxClientOptions = {}) {
    this.transport = new V1Transport(options)
    this.sandboxes = new SandboxesCollection(this.transport)
    this.snapshots = new SnapshotsCollection(this.transport)
    this.templates = new TemplatesCollection(this.transport)
    this.operations = new OperationsCollection(this.transport)
    this.portForwards = new PortForwardsCollection(this.transport)
    this.usage = new UsageCollection(this.transport)
  }
}

/**
 * Billable usage.
 *
 * {@link report} is the primary call because totals belong with the rows: they
 * cover the whole selection, so paging through intervals alone would make the
 * amount owed look like it depends on the page size.
 */
export class UsageCollection {
  constructor(private readonly transport: V1Transport) {}

  /** One page of intervals plus totals for the entire selection. */
  async report(options: UsageQueryOptions = {}): Promise<UsageReport> {
    return usageReportFromApi(await this.transport.get<ApiUsageReport>('/v1/usage', usageQuery(options), options.signal))
  }

  /**
   * Usage for one sandbox. This routes to the sandbox's host, so it only
   * answers while the sandbox exists — for a deleted one, call
   * `report({ sandboxId })`, which asks every host.
   */
  async forSandbox(sandboxId: string, options: Omit<UsageQueryOptions, 'sandboxId'> = {}): Promise<UsageReport> {
    return usageReportFromApi(await this.transport.get<ApiUsageReport>(
      `/v1/sandboxes/${encodeURIComponent(sandboxId)}/usage`, usageQuery(options), options.signal,
    ))
  }

  /** Every interval in the selection, paging transparently. */
  list(options: UsageQueryOptions = {}): AsyncIterable<UsageIntervalResource> {
    return paginated(async (pageToken) => {
      const page = await this.report({ ...options, ...(pageToken === undefined ? {} : { pageToken }) })
      return { items: page.intervals, ...(page.nextPageToken === undefined ? {} : { nextPageToken: page.nextPageToken }) }
    })
  }
}

export class SandboxesCollection {
  constructor(private readonly transport: V1Transport) {}

  async create(options: CreateSandboxOptions = {}): Promise<ClientSandbox> {
    const raw = await this.transport.mutate<ApiSandbox>('POST', '/v1/sandboxes', createBody(options), {
      timeoutMs: options.requestTimeoutMs ?? CREATE_REQUEST_TIMEOUT_MS,
      signal: options.signal,
      idempotencyKey: options.idempotencyKey,
    })
    return new ClientSandbox(this.transport, raw)
  }

  async createAsync(options: CreateSandboxOptions = {}): Promise<Operation<ClientSandbox>> {
    const raw = await this.transport.createAsync(createBody(options), {
      timeoutMs: options.requestTimeoutMs ?? CREATE_REQUEST_TIMEOUT_MS,
      signal: options.signal,
      idempotencyKey: options.idempotencyKey,
    })
    return new Operation(this.transport, raw, (sandbox) => new ClientSandbox(this.transport, sandbox))
  }

  async get(id: string, signal?: AbortSignal): Promise<ClientSandbox> {
    return new ClientSandbox(this.transport, await this.transport.get<ApiSandbox>(`/v1/sandboxes/${encodeURIComponent(id)}`, {}, signal))
  }

  list(options: ListSandboxesOptions = {}): AsyncIterable<ClientSandbox> {
    const query: Record<string, string> = {}
    if (options.status) query.status = options.status
    if (options.sourceType) query.source_type = options.sourceType
    if (options.createdAfter) query.created_after = options.createdAfter.toISOString()
    if (options.createdBefore) query.created_before = options.createdBefore.toISOString()
    for (const [key, value] of Object.entries(options.metadata ?? {})) query[`metadata.${key}`] = value
    return paginated(async (pageToken) => {
      const page = await this.transport.get<{ sandboxes: ApiSandbox[]; next_page_token?: string }>(
        '/v1/sandboxes', paginationQuery(query, options.pageSize, pageToken), options.signal,
      )
      return { items: page.sandboxes.map((raw) => new ClientSandbox(this.transport, raw)), nextPageToken: page.next_page_token }
    })
  }

  async createMany(options: CreateManyOptions): Promise<Operation<ClientSandbox>> {
    if (!Number.isInteger(options.count) || options.count < 1) throw new Error('count must be a positive integer')
    const raw = await this.transport.mutate<ApiOperation>('POST', '/v1/sandbox-batches', {
      count: options.count,
      sandbox: createBody(options),
      ...(options.maxParallelism === undefined ? {} : { max_parallelism: options.maxParallelism }),
    }, {
      timeoutMs: options.requestTimeoutMs ?? CREATE_REQUEST_TIMEOUT_MS,
      signal: options.signal,
      idempotencyKey: options.idempotencyKey,
    })
    return new Operation(this.transport, raw, (sandbox) => new ClientSandbox(this.transport, sandbox))
  }
}

export class SnapshotsCollection {
  constructor(private readonly transport: V1Transport) {}
  async get(id: string, signal?: AbortSignal): Promise<SnapshotResource> {
    return snapshotFromApi(await this.transport.get<ApiSnapshot>(`/v1/snapshots/${encodeURIComponent(id)}`, {}, signal))
  }
  list(options: ListOptions = {}): AsyncIterable<SnapshotResource> {
    return paginated(async (pageToken) => {
      const page = await this.transport.get<{ snapshots: ApiSnapshot[]; next_page_token?: string }>(
        '/v1/snapshots', paginationQuery({}, options.pageSize, pageToken), options.signal,
      )
      return { items: page.snapshots.map(snapshotFromApi), nextPageToken: page.next_page_token }
    })
  }

  /**
   * Waits until the snapshot's artifacts are committed to durable storage.
   * A local snapshot without upload details remains waitable: it may be a
   * peer cache whose creator is still publishing elsewhere.
   */
  async waitForDurable(id: string, options: WaitOptions = {}): Promise<SnapshotResource> {
    const pollIntervalMs = options.pollIntervalMs ?? 500
    const timeoutMs = options.timeoutMs ?? CREATE_REQUEST_TIMEOUT_MS
    const started = Date.now()
    for (;;) {
      const elapsed = Date.now() - started
      const remaining = timeoutMs - elapsed
      if (remaining <= 0) throw new TimeoutError(`Snapshot ${id} did not become durable within ${timeoutMs} ms`)
      const raw = await this.transport.get<ApiSnapshot>(
        `/v1/snapshots/${encodeURIComponent(id)}`, {}, options.signal, remaining,
      )
      const snapshot = snapshotFromApi(raw)
      if (snapshot.state === 'durable') return snapshot
      if (snapshot.upload?.state === 'failed') {
        throw new SandboxError(
          `Snapshot ${id} upload failed${snapshot.upload.error ? `: ${snapshot.upload.error}` : ''}`,
          undefined,
          { code: 'snapshot_upload_failed' },
        )
      }
      const afterGet = Date.now() - started
      const waitMs = timeoutMs - afterGet
      if (waitMs <= 0) throw new TimeoutError(`Snapshot ${id} did not become durable within ${timeoutMs} ms`)
      await wait(Math.min(pollIntervalMs, waitMs), options.signal)
    }
  }
  async delete(id: string, control: RequestControl = {}): Promise<void> {
    await this.transport.mutate<void>('DELETE', `/v1/snapshots/${encodeURIComponent(id)}`, undefined, control)
  }
}

export class TemplatesCollection {
  constructor(private readonly transport: V1Transport) {}
  async get(id: string, signal?: AbortSignal): Promise<TemplateResource> {
    return templateFromApi(await this.transport.get<ApiTemplate>(`/v1/templates/${encodeURIComponent(id)}`, {}, signal))
  }
  list(options: ListOptions = {}): AsyncIterable<TemplateResource> {
    return paginated(async (pageToken) => {
      const page = await this.transport.get<{ templates: ApiTemplate[]; next_page_token?: string }>(
        '/v1/templates', paginationQuery({}, options.pageSize, pageToken), options.signal,
      )
      return { items: page.templates.map(templateFromApi), nextPageToken: page.next_page_token }
    })
  }
  async updateWarmTarget(id: string, warmTarget: number, control: RequestControl = {}): Promise<TemplateResource> {
    if (!Number.isInteger(warmTarget) || warmTarget < 0) throw new Error('warmTarget must be a non-negative integer')
    const raw = await this.transport.mutate<ApiTemplate>(
      'PATCH', `/v1/templates/${encodeURIComponent(id)}`, { warm_target: warmTarget }, control,
    )
    return templateFromApi(raw)
  }
}

export class OperationsCollection {
  constructor(private readonly transport: V1Transport) {}
  async get(id: string, signal?: AbortSignal): Promise<Operation<ClientSandbox>> {
    const raw = await this.transport.get<ApiOperation>(`/v1/operations/${encodeURIComponent(id)}`, {}, signal)
    return new Operation(this.transport, raw, (sandbox) => new ClientSandbox(this.transport, sandbox))
  }
  list(options: ListOptions = {}): AsyncIterable<Operation<ClientSandbox>> {
    return paginated(async (pageToken) => {
      const page = await this.transport.get<{ operations: ApiOperation[]; next_page_token?: string }>(
        '/v1/operations', paginationQuery({}, options.pageSize, pageToken), options.signal,
      )
      return {
        items: page.operations.map((raw) => new Operation(this.transport, raw, (sandbox) => new ClientSandbox(this.transport, sandbox))),
        nextPageToken: page.next_page_token,
      }
    })
  }
}

export class PortForwardsCollection {
  constructor(private readonly transport: V1Transport) {}
  async create(
    sandboxId: string, guestPort: number, opts: PortForwardCreateOptions = {},
  ): Promise<PortForwardResource> {
    const raw = await this.transport.mutate<ApiPortForward>(
      'POST', `/v1/sandboxes/${encodeURIComponent(sandboxId)}/port-forwards`,
      portForwardBody(guestPort, opts), opts,
    )
    return portForwardFromApi(raw)
  }
  /** Allocates a durable public TCP address for a non-HTTP service. SSH is CLI-owned. */
  async createRaw(
    sandboxId: string, guestPort: number, control: RequestControl = {},
  ): Promise<RawPortForwardResource> {
    const raw = await this.transport.mutate<ApiPortForward>(
      'POST', `/v1/sandboxes/${encodeURIComponent(sandboxId)}/port-forwards`,
      { guest_port: guestPort, mode: 'raw' }, control,
    )
    return rawPortForwardFromApi(raw)
  }
  async list(sandboxId: string, signal?: AbortSignal): Promise<PortForwardResource[]> {
    const page = await this.transport.get<{ port_forwards: ApiPortForward[] }>(
      `/v1/sandboxes/${encodeURIComponent(sandboxId)}/port-forwards`, {}, signal,
    )
    return page.port_forwards.map(portForwardFromApi)
  }
}

function createBody(options: CreateSandboxOptions): ApiCreate {
  const body: ApiCreate = {}
  if (options.name !== undefined) body.name = options.name
  if (options.source !== undefined) body.source = sourceToApi(options.source)
  if (options.metadata !== undefined) body.metadata = options.metadata
  if (options.resources !== undefined) {
    const { vcpus, memoryMib } = options.resources
    if (vcpus !== undefined && (!Number.isInteger(vcpus) || vcpus < 0)) {
      throw new Error('resources.vcpus must be a non-negative finite integer')
    }
    if (memoryMib !== undefined && (!Number.isInteger(memoryMib) || memoryMib < 0 || (memoryMib > 0 && memoryMib < 128))) {
      throw new Error('resources.memoryMib must be zero or a finite integer of at least 128')
    }
    const fromSnapshot = body.source?.type === 'snapshot' || (body.source?.type === 'template' && body.source.id !== 'default')
    if (fromSnapshot && ((vcpus ?? 0) > 0 || (memoryMib ?? 0) > 0)) {
      throw new Error('snapshot resources cannot be overridden')
    }
    body.resources = { vcpu: vcpus, memory_mib: memoryMib }
  }
  if (options.ttlMs !== undefined || options.idleTimeoutMs !== undefined) {
    body.lifecycle = {}
    if (options.ttlMs !== undefined) body.lifecycle.ttl_seconds = millisecondsToSeconds(options.ttlMs, 'ttlMs')
    if (options.idleTimeoutMs !== undefined) body.lifecycle.idle_timeout_seconds = idleMillisecondsToSeconds(options.idleTimeoutMs)
  }
  return body
}

function sourceToApi(source: SandboxSource): components['schemas']['Source'] {
  if ('snapshotId' in source) return { type: 'snapshot', id: source.snapshotId }
  if ('templateId' in source) return { type: 'template', id: source.templateId }
  if ('id' in source) return { type: source.type, id: source.id }
  return { type: 'default' }
}

function snapshotFromApi(raw: ApiSnapshot): SnapshotResource {
  const snapshot: SnapshotResource = {
    id: raw.id,
    ...(raw.name === undefined ? {} : { name: raw.name }),
    sourceSandboxId: raw.source_sandbox_id,
    state: raw.state,
    createdAt: new Date(raw.created_at),
    ...(raw.expires_at === undefined ? {} : { expiresAt: new Date(raw.expires_at) }),
  }
  if (raw.upload !== undefined) {
    snapshot.upload = {
      state: raw.upload.state,
      attempts: raw.upload.attempts,
      ...(raw.upload.next_attempt_at === undefined ? {} : { nextAttemptAt: new Date(raw.upload.next_attempt_at) }),
      ...(raw.upload.error === undefined ? {} : { error: raw.upload.error }),
    }
  }
  return snapshot
}

function usageQuery(options: UsageQueryOptions & { pageToken?: string }): Record<string, string> {
  const query: Record<string, string> = {}
  if (options.sandboxId) query.sandbox_id = options.sandboxId
  if (options.from) query.from = options.from.toISOString()
  if (options.to) query.to = options.to.toISOString()
  return paginationQuery(query, options.pageSize, options.pageToken)
}

function usageReportFromApi(raw: ApiUsageReport): UsageReport {
  const report: UsageReport = {
    intervals: raw.intervals.map(usageIntervalFromApi),
    totals: {
      intervals: raw.totals.intervals,
      openIntervals: raw.totals.open_intervals,
      durationSeconds: raw.totals.duration_seconds,
      vcpuSeconds: raw.totals.vcpu_seconds,
      memoryMibSeconds: raw.totals.memory_mib_seconds,
      cpuSeconds: raw.totals.cpu_seconds,
    },
    window: {
      selection: raw.window.selection,
      ...(raw.window.from === undefined ? {} : { from: new Date(raw.window.from) }),
      ...(raw.window.to === undefined ? {} : { to: new Date(raw.window.to) }),
    },
    coverage: {
      hostsReporting: raw.coverage.hosts_reporting,
      scope: raw.coverage.scope,
      truncated: raw.coverage.truncated,
    },
  }
  if (raw.next_page_token !== undefined) report.nextPageToken = raw.next_page_token
  return report
}

function usageIntervalFromApi(raw: ApiUsageInterval): UsageIntervalResource {
  const interval: UsageIntervalResource = {
    id: raw.id,
    sandboxId: raw.sandbox_id,
    sequence: raw.sequence,
    state: raw.state,
    resources: { vcpus: raw.resources.vcpu, memoryMib: raw.resources.memory_mib },
    startedAt: new Date(raw.started_at),
    durationSeconds: raw.duration_seconds,
    vcpuSeconds: raw.vcpu_seconds,
    memoryMibSeconds: raw.memory_mib_seconds,
    cpuSeconds: raw.cpu_seconds,
    metadata: raw.metadata ?? {},
  }
  if (raw.ended_at !== undefined) interval.endedAt = new Date(raw.ended_at)
  if (raw.end_reason !== undefined) interval.endReason = raw.end_reason
  return interval
}

function templateFromApi(raw: ApiTemplate): TemplateResource {
  return { id: raw.id, revision: raw.revision, warmTarget: raw.warm_target, resources: { vcpus: raw.resources.vcpu, memoryMib: raw.resources.memory_mib } }
}

function portForwardFromApi(raw: ApiPortForward): PortForwardResource {
  const out: PortForwardResource = {
    id: raw.id, sandboxId: raw.sandbox_id, guestPort: raw.guest_port,
    mode: raw.mode, status: raw.status,
  }
  if (raw.host_port !== undefined) out.hostPort = raw.host_port
  if (raw.url !== undefined) out.url = raw.url
  if (raw.public_port !== undefined) out.publicPort = raw.public_port
  if (raw.public_host !== undefined) out.publicHost = raw.public_host
  if (raw.public_host !== undefined && raw.public_port !== undefined) {
    out.address = `${raw.public_host}:${raw.public_port}`
  }
  return out
}

function rawPortForwardFromApi(raw: ApiPortForward): RawPortForwardResource {
  const out = portForwardFromApi(raw)
  if (out.mode !== 'raw' || out.publicHost === undefined || out.publicPort === undefined || out.address === undefined) {
    throw new Error('Server returned an incomplete raw port-forward mapping')
  }
  return out as RawPortForwardResource
}

/** Builds an ordinary or raw port-forward request. */
function portForwardBody(guestPort: number, opts: PortForwardCreateOptions): unknown {
  if (opts.mode === 'raw') {
    if (opts.hostPort !== undefined) throw new TypeError('mode raw cannot be combined with hostPort')
    return { guest_port: guestPort, mode: 'raw' }
  }
  if (opts.hostPort === undefined) return { guest_port: guestPort }
  return { guest_port: guestPort, host_port: opts.hostPort }
}

function stageMarkFromApi(raw: ApiCreateStageMark): CreateStageMark {
  return {
    stage: raw.stage,
    attempt: raw.attempt,
    startedAt: new Date(raw.started_at),
    ...(raw.completed_at === undefined ? {} : { completedAt: new Date(raw.completed_at) }),
  }
}

function createProgressFromApi(raw: ApiCreateProgress): CreateProgress {
  return {
    coordination: {
      phase: raw.coordination.phase,
      ...(raw.coordination.updated_at === undefined ? {} : { updatedAt: new Date(raw.coordination.updated_at) }),
    },
    ...(raw.worker === undefined ? {} : {
      worker: {
        attempt: raw.worker.attempt,
        sequence: raw.worker.sequence,
        condition: raw.worker.condition,
        current: stageMarkFromApi(raw.worker.current),
        ...(raw.worker.last_completed === undefined ? {} : {
          lastCompleted: {
            ...stageMarkFromApi(raw.worker.last_completed),
            completedAt: new Date(raw.worker.last_completed.completed_at),
          },
        }),
        observedAt: new Date(raw.worker.observed_at),
      },
    }),
  }
}

function parseCreateAcceptance(value: unknown): ApiOperation {
  if (typeof value !== 'object' || value === null ||
    !('id' in value) || typeof value.id !== 'string' || value.id.trim() === '' ||
    !('type' in value) || value.type !== 'sandbox_create' ||
    !('status' in value) || value.status !== 'pending' ||
    !('requested' in value) || value.requested !== 1 ||
    !('succeeded' in value) || value.succeeded !== 0 ||
    !('failed' in value) || value.failed !== 0 ||
    !('created_at' in value) || typeof value.created_at !== 'string' || !Number.isFinite(Date.parse(value.created_at)) ||
    ('completed_at' in value && value.completed_at !== undefined) ||
    ('results' in value && (!Array.isArray(value.results) || value.results.length !== 0)) ||
    ('request_id' in value && typeof value.request_id !== 'string')) {
    throw new SandboxError('Invalid single-create operation acceptance')
  }
  return {
    id: value.id, type: value.type, status: value.status,
    requested: value.requested, succeeded: value.succeeded, failed: value.failed,
    created_at: value.created_at,
    ...('request_id' in value && typeof value.request_id === 'string' ? { request_id: value.request_id } : {}),
  }
}

function operationFromApi<T>(raw: ApiOperation, mapValue: (raw: ApiSandbox) => T): OperationState<T> {
  return {
    id: raw.id,
    type: raw.type,
    ...(raw.request_id === undefined ? {} : { requestId: raw.request_id }),
    status: raw.status,
    requested: raw.requested,
    succeeded: raw.succeeded,
    failed: raw.failed,
    results: (raw.results ?? []).map((item) => ({
      index: item.index,
      ...(item.sandbox === undefined ? {} : { value: mapValue(item.sandbox) }),
      ...(item.error === undefined ? {} : { error: item.error }),
      ...(item.progress === undefined ? {} : { progress: createProgressFromApi(item.progress) }),
    })),
    createdAt: new Date(raw.created_at),
    ...(raw.completed_at === undefined ? {} : { completedAt: new Date(raw.completed_at) }),
  }
}

function idleMillisecondsToSeconds(value: number): number {
  if (value === -1) return -1
  return millisecondsToSeconds(value, 'idleTimeoutMs')
}

function millisecondsToSeconds(value: number, name: string): number {
  if (!Number.isFinite(value) || value < 0) throw new Error(`${name} must be a non-negative finite number`)
  return Math.ceil(value / 1000)
}

function paginationQuery(base: Record<string, string>, pageSize?: number, pageToken?: string): Record<string, string> {
  return {
    ...base,
    ...(pageSize === undefined ? {} : { page_size: String(pageSize) }),
    ...(pageToken === undefined ? {} : { page_token: pageToken }),
  }
}

async function* paginated<T>(load: (pageToken?: string) => Promise<Page<T>>): AsyncGenerator<T> {
  let token: string | undefined
  do {
    const page = await load(token)
    for (const item of page.items) yield item
    token = page.nextPageToken
  } while (token)
}

function wait(ms: number, signal?: AbortSignal): Promise<void> {
  if (signal?.aborted) return Promise.reject(signal.reason ?? new Error('Operation wait aborted'))
  return new Promise((resolve, reject) => {
    const onAbort = () => {
      clearTimeout(timer)
      reject(signal?.reason ?? new Error('Operation wait aborted'))
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    signal?.addEventListener('abort', onAbort, { once: true })
  })
}

// Response.json() has no signal parameter. Read the stream ourselves so an
// enclosing durable-wait deadline can also interrupt a body that stalls after
// HTTP headers, then cancel the reader to release the underlying transport.
async function readJSONWithinDeadline<T>(response: Response, signal: AbortSignal): Promise<T> {
  signal.throwIfAborted()
  const reader = response.body?.getReader()
  if (reader === undefined) return response.json() as Promise<T>

  let rejectAbort: ((reason: Error) => void) | undefined
  const aborted = new Promise<never>((_, reject) => {
    rejectAbort = reject
  })
  const onAbort = () => {
    rejectAbort?.(signal.reason instanceof Error ? signal.reason : new SandboxError('Request aborted'))
    void reader.cancel().catch(() => {})
  }
  signal.addEventListener('abort', onAbort, { once: true })

  const decoder = new TextDecoder()
  let text = ''
  try {
    for (;;) {
      const chunk = await Promise.race([reader.read(), aborted])
      signal.throwIfAborted()
      if (chunk.done) break
      text += decoder.decode(chunk.value, { stream: true })
    }
    text += decoder.decode()
    return JSON.parse(text)
  } finally {
    signal.removeEventListener('abort', onAbort)
    void reader.cancel().catch(() => {})
    reader.releaseLock()
  }
}
