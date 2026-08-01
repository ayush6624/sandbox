import { randomUUID } from 'node:crypto'

import { ApiClient, CREATE_REQUEST_TIMEOUT_MS } from './client.js'
import { Commands } from './commands.js'
import type { ProblemDetails } from './errors.js'
import { Files } from './files.js'
import { Pty } from './pty.js'
import type { components } from './generated/api-v1.js'

type ApiSandbox = components['schemas']['Sandbox']
type ApiSnapshot = components['schemas']['Snapshot']
type ApiTemplate = components['schemas']['Template']
type ApiOperation = components['schemas']['Operation']
type ApiPortForward = components['schemas']['PortForward']
type ApiCreate = components['schemas']['CreateSandboxRequest']
type ApiUpdate = components['schemas']['UpdateSandboxRequest']

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

export interface CreateSandboxOptions {
  name?: string
  source?: SandboxSource
  ttlMs?: number
  idleTimeoutMs?: number
  resources?: SandboxResources
  metadata?: Record<string, string>
  sshPublicKey?: string
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
  createdAt: Date
  expiresAt?: Date
}

export interface TemplateResource {
  id: string
  revision: string
  resources: SandboxResources
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
  /** Fleet-wide public TCP port, for a `raw` exposure. */
  publicPort?: number
  status: 'active'
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
}

export interface BatchResult<T> {
  index: number
  value?: T
  error?: ProblemDetails
}

export interface OperationState<T> {
  id: string
  type: 'sandbox_batch_create'
  status: 'pending' | 'running' | 'succeeded' | 'partially_succeeded' | 'failed'
  requested: number
  succeeded: number
  failed: number
  results: Array<BatchResult<T>>
  createdAt: Date
  completedAt?: Date
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

  async get<T>(path: string, query: Record<string, string> = {}, signal?: AbortSignal): Promise<T> {
    const response = await this.http.request('GET', path, {
      query,
      signal,
      retries: this.maxRetries,
    })
    return response.json() as Promise<T>
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
    return this.raw.lifecycle.idle_timeout_seconds === undefined ? undefined : this.raw.lifecycle.idle_timeout_seconds * 1000
  }
  get createdAt(): Date { return new Date(this.raw.created_at) }
  get expiresAt(): Date | undefined { return this.raw.expires_at ? new Date(this.raw.expires_at) : undefined }

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
      if (options.idleTimeoutMs !== undefined) body.lifecycle.idle_timeout_seconds = millisecondsToSeconds(options.idleTimeoutMs, 'idleTimeoutMs')
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

  async listPortForwards(signal?: AbortSignal): Promise<PortForwardResource[]> {
    const raw = await this.transport.get<{ port_forwards: ApiPortForward[] }>(
      `/v1/sandboxes/${encodeURIComponent(this.id)}/port-forwards`, {}, signal,
    )
    return raw.port_forwards.map(portForwardFromApi)
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
      if (Date.now() - started >= timeoutMs) throw new Error(`Operation ${this.id} did not complete within ${timeoutMs} ms`)
      await wait(Math.min(pollIntervalMs, timeoutMs - (Date.now() - started)), options.signal)
      await this.refresh(options.signal)
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
  private readonly transport: V1Transport

  constructor(options: SandboxClientOptions = {}) {
    this.transport = new V1Transport(options)
    this.sandboxes = new SandboxesCollection(this.transport)
    this.snapshots = new SnapshotsCollection(this.transport)
    this.templates = new TemplatesCollection(this.transport)
    this.operations = new OperationsCollection(this.transport)
    this.portForwards = new PortForwardsCollection(this.transport)
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
  if (options.sshPublicKey !== undefined) body.ssh_public_key = options.sshPublicKey
  if (options.resources !== undefined) {
    body.resources = { vcpu: options.resources.vcpus, memory_mib: options.resources.memoryMib }
  }
  if (options.ttlMs !== undefined || options.idleTimeoutMs !== undefined) {
    body.lifecycle = {}
    if (options.ttlMs !== undefined) body.lifecycle.ttl_seconds = millisecondsToSeconds(options.ttlMs, 'ttlMs')
    if (options.idleTimeoutMs !== undefined) body.lifecycle.idle_timeout_seconds = millisecondsToSeconds(options.idleTimeoutMs, 'idleTimeoutMs')
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
  return {
    id: raw.id,
    ...(raw.name === undefined ? {} : { name: raw.name }),
    sourceSandboxId: raw.source_sandbox_id,
    state: raw.state,
    createdAt: new Date(raw.created_at),
    ...(raw.expires_at === undefined ? {} : { expiresAt: new Date(raw.expires_at) }),
  }
}

function templateFromApi(raw: ApiTemplate): TemplateResource {
  return { id: raw.id, revision: raw.revision, resources: { vcpus: raw.resources.vcpu, memoryMib: raw.resources.memory_mib } }
}

function portForwardFromApi(raw: ApiPortForward): PortForwardResource {
  const out: PortForwardResource = {
    id: raw.id, sandboxId: raw.sandbox_id, guestPort: raw.guest_port,
    mode: raw.mode, status: raw.status,
  }
  if (raw.host_port !== undefined) out.hostPort = raw.host_port
  if (raw.url !== undefined) out.url = raw.url
  if (raw.public_port !== undefined) out.publicPort = raw.public_port
  return out
}

/** Builds the create body, omitting `host_port` so the worker default applies. */
function portForwardBody(guestPort: number, opts: PortForwardCreateOptions): unknown {
  if (opts.hostPort === undefined) return { guest_port: guestPort }
  return { guest_port: guestPort, host_port: opts.hostPort }
}

function operationFromApi<T>(raw: ApiOperation, mapValue: (raw: ApiSandbox) => T): OperationState<T> {
  return {
    id: raw.id,
    type: raw.type,
    status: raw.status,
    requested: raw.requested,
    succeeded: raw.succeeded,
    failed: raw.failed,
    results: (raw.results ?? []).map((item) => ({
      index: item.index,
      ...(item.sandbox === undefined ? {} : { value: mapValue(item.sandbox) }),
      ...(item.error === undefined ? {} : { error: item.error }),
    })),
    createdAt: new Date(raw.created_at),
    ...(raw.completed_at === undefined ? {} : { completedAt: new Date(raw.completed_at) }),
  }
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
    const timer = setTimeout(resolve, ms)
    signal?.addEventListener('abort', () => {
      clearTimeout(timer)
      reject(signal.reason ?? new Error('Operation wait aborted'))
    }, { once: true })
  })
}
