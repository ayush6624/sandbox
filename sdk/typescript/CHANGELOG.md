# Changelog

## 2.0.0 - 2026-08-01

### Breaking

- **Removed `Sandbox.hosts()`.** Fleet host inventory is an operator call, not
  a tenant one: it discloses per-host addresses and live capacity, so the
  gateway now authenticates `GET /hosts` (and `/hosts/{host}/drain`,
  `PUT /worker-release`) with its **worker-control** credential rather than the
  client credential a tenant holds as `SANDBOX_API_KEY`. Against a gateway at
  or after that change, `Sandbox.hosts()` could only ever return 401.

  **Your TypeScript build will fail on upgrade, by design.** Every
  `Sandbox.hosts(...)` call site is a compile error in 2.0.0 (`error TS2554:
  Expected 1 arguments, but got 0`, or `TS2345: ... not assignable to parameter
  of type 'never'`), so `tsc` and CI tell you at build time instead of a
  production stack trace. JavaScript consumers, who have no compile step, get a
  thrown `SandboxError` naming the replacement and the credential.

  Migration — one line, plus a different credential:

  ```ts
  // before
  const hosts = await Sandbox.hosts()
  // after
  const hosts = await new FleetClient({ controlKey: process.env.SANDBOX_CONTROL_KEY }).hosts.list()
  ```

  `SANDBOX_CONTROL_KEY` is the gateway's worker-control token
  (`gateway --worker-token`, `GATEWAY_CONTROL_TOKEN` in `infra/gcp`); with the
  variable set, `new FleetClient()` needs no arguments. The returned
  `FleetHostInfo[]` shape is unchanged, so code downstream of the call needs no
  edits. `Sandbox.hosts` survives only as an uncallable shim — a compile error
  at every call site, and a `SandboxError` naming `FleetClient` and the
  credential if reached from JavaScript — so the break is never an opaque 401.

  Requires a gateway at or after `32ae881`; the new client requests
  `/internal/v1/hosts`, which has been worker-gated since it existed, so it
  also works against older gateways.

### Added

- `FleetClient` — the operator surface for a gateway, with
  `fleet.hosts.list()`. Its own class and its own options
  (`gatewayUrl`/`controlKey`, defaulting to `SANDBOX_CONTROL_URL` →
  `SANDBOX_API_URL` and `SANDBOX_CONTROL_KEY`) so a tenant client cannot carry
  an operator credential, and it never falls back to `SANDBOX_API_KEY`.
  Constructing it without a control credential throws `AuthenticationError`
  before any request goes out.

Supported server contract: `/v1` (`api/openapi.yaml` version `1.0.0`).

## 1.0.1 - 2026-07-31

- Fix `sandbox.pty` being unable to connect at all. The server stopped
  accepting `?access_token=` WebSocket credentials, but the SDK still sent the
  key only that way and set no header, so every `pty.create()` failed the
  handshake and surfaced as `PTY connection closed unexpectedly (code 1006)`.
  The key now rides in the WebSocket subprotocol list
  (`sandbox.bearer.<base64url(token)>` + `sandbox.shell.v1`), which browsers can
  set. No API change; requires a server at or after the `6e4f1c0`
  management-security change.
- WebSocket URLs no longer carry the API key, so it can't leak into proxy
  traces or access logs.

Supported server contract: `/v1` (`api/openapi.yaml` version `1.0.0`).

## 1.0.0 - 2026-07-26

- Add the configured `SandboxClient` and versioned resource collections.
- Add snapshot-source creation and typed `createMany` operations.
- Add async cursor pagination, abortable operation waits, generated
  idempotency keys, and safe retry behavior.
- Surface RFC 9457 problem codes and request IDs on SDK errors.
- Add `pause`, `resume`, and `terminate`; retain `hibernate`, `kill`, `restore`,
  and `fanout` as deprecated migration aliases.
- Generate transport types from the OpenAPI 3.1 contract.

Supported server contract: `/v1` (`api/openapi.yaml` version `1.0.0`).
