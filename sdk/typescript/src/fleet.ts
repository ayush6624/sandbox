import { ApiClient } from './client.js'
import { AuthenticationError, SandboxError } from './errors.js'
import { toFleetHostInfo } from './types.js'
import type { ApiFleetHost, FleetHostInfo } from './types.js'

/**
 * Options for {@link FleetClient}. Deliberately separate from
 * {@link SandboxOpts}: the fleet API authenticates with the gateway's
 * **worker-control** credential, and mixing the two option shapes is what let
 * a tenant client express an operator call in the first place.
 */
export interface FleetClientOptions {
  /**
   * Base URL of the gateway, e.g. `http://10.160.0.100:9090`. Defaults to
   * `SANDBOX_CONTROL_URL`, then `SANDBOX_API_URL` — a URL is not a credential,
   * and the operator endpoints live on the same gateway as the tenant API.
   */
  gatewayUrl?: string
  /**
   * The gateway's worker-control credential (`gateway --worker-token`, i.e.
   * `GATEWAY_CONTROL_TOKEN` in the reference deployment). Defaults to
   * `SANDBOX_CONTROL_KEY`. A tenant `SANDBOX_API_KEY` is rejected by the
   * gateway with 401 and is never read from the environment here.
   */
  controlKey?: string
  /** Default per-request timeout in milliseconds (default 30 000). */
  requestTimeoutMs?: number
}

/**
 * Operator entry point for fleet control against a gateway — the surface a
 * fleet owner drives, not a tenant.
 *
 * It is a separate client with its own credential because the gateway keeps
 * three disjoint trust domains (client / worker-control / edge; see
 * `docs/management-security.md`). Host inventory discloses per-host addresses
 * and live capacity, so it is authenticated by the worker-control credential.
 * Giving it its own class means a tenant `Sandbox` client cannot express an
 * operator call at all, rather than expressing one that fails with 401.
 *
 * ```ts
 * const fleet = new FleetClient()          // SANDBOX_CONTROL_KEY
 * for (const host of await fleet.hosts.list()) {
 *   console.log(host.hostId, host.free, host.alive)
 * }
 * ```
 *
 * Gateway-only: a single host has no fleet view of itself and answers 404.
 */
export class FleetClient {
  /** Host inventory: the fleet view the gateway itself places against. */
  readonly hosts: FleetHostsCollection

  constructor(options: FleetClientOptions = {}) {
    const gatewayUrl =
      options.gatewayUrl ?? process.env.SANDBOX_CONTROL_URL ?? process.env.SANDBOX_API_URL
    if (!gatewayUrl) {
      throw new SandboxError(
        'Missing gateway URL: set the SANDBOX_CONTROL_URL (or SANDBOX_API_URL) environment variable (e.g. http://10.160.0.100:9090) or pass { gatewayUrl } explicitly.'
      )
    }
    const controlKey = options.controlKey ?? process.env.SANDBOX_CONTROL_KEY
    if (!controlKey) {
      throw new AuthenticationError(
        'Missing fleet control credential: set the SANDBOX_CONTROL_KEY environment variable to the gateway worker-control token (GATEWAY_CONTROL_TOKEN) or pass { controlKey } explicitly. A tenant SANDBOX_API_KEY cannot read fleet control routes.'
      )
    }
    const transport = new ApiClient({
      apiUrl: gatewayUrl,
      apiKey: controlKey,
      requestTimeoutMs: options.requestTimeoutMs,
    })
    this.hosts = new FleetHostsCollection(transport)
  }
}

/** The gateway's host inventory, reachable with the worker-control credential. */
export class FleetHostsCollection {
  constructor(private readonly transport: ApiClient) {}

  /**
   * Lists the hosts behind the gateway with their live capacity — what the
   * gateway itself places against. Useful for dashboards and for deciding
   * whether a {@link CapacityError} is worth retrying.
   *
   * Always requests the canonical `/internal/v1/hosts` path, never the legacy
   * `/hosts` alias: `/internal/v1` has been worker-gated since it existed, so
   * this works against gateways from both before and after the alias moved
   * into the worker-control domain.
   *
   * @throws {AuthenticationError} when the credential is not the gateway's
   *         worker-control token.
   * @throws {NotFoundError} when the URL points at a host, not a gateway.
   */
  async list(signal?: AbortSignal): Promise<FleetHostInfo[]> {
    const res = await this.transport.request('GET', '/internal/v1/hosts', { signal, retries: 2 })
    const raw = (await res.json()) as ApiFleetHost[] | null
    return (raw ?? []).map(toFleetHostInfo)
  }
}
