# Changelog

## 2.3.0 - 2026-08-03

### Changed

- **Breaking at compile time (type-level only, no runtime behavior change):**
  removed `sshPublicKey` / `sshPubkey` from SDK create options. Code passing
  either field stops type-checking and must drop it; nothing silently changes
  behavior at runtime. Applications no longer manage user SSH keys or build
  `ssh -p ...` commands.
- SSH is now CLI-owned. `ClientSandbox.sshInstructions` returns the command a
  web UI or SDK integration should show (`sandbox ssh <id>`); the CLI handles
  key authorization, wake-on-connect, host-key safety, and the authenticated
  byte stream without allocating a public SSH port.
- Raw TCP forwarding remains available for non-HTTP services, independent of
  the SSH workflow.

Supported server contract: `/v1` (`api/openapi.yaml` version `1.3.0`).

## 2.2.0 - 2026-08-02

### Added

- **Raw TCP is now first-class on the primary `/v1` client.**
  `sandbox.createRawPortForward(22)` and
  `client.portForwards.createRaw(sandboxId, 22)` allocate a durable public TCP
  endpoint suitable for SSH. The returned `RawPortForwardResource` has
  non-optional `publicHost`, `publicPort`, and ready-to-dial `address` fields.
- `createPortForward(port, { mode: 'raw' })` exposes the same v1 contract for
  callers that select modes dynamically. It rejects combinations with
  `hostPort` before making a request.

Supported server contract: `/v1` (`api/openapi.yaml` version `1.2.0`).

## 2.1.0 - 2026-08-01

### Added

- **Public ingress URLs on the facade are now documented and covered.** They
  shipped in the code but appeared in no README, changelog entry, or test:
  - `sbx.getUrl(port)` — synchronous public URL for an exposed port, served
    from a cache filled by `create`/`connect`/`refresh`/`exposePort`/`listPorts`.
  - `sbx.exposePort(port, { hostPort: false })` — URL-only exposure, which
    reserves no worker host port; `{ hostPort: true }` forces one. Omitted
    follows the worker default.
  - `PortMapping.mode` (`host_port` | `url` | `both` | `raw`), `.url`, and
    `.publicPort`.
  - `sbx.exposeRawPort(port)` — fleet-wide raw TCP address, for SSH and
    anything else ingress's HTTP termination cannot carry.
- **The `/v1` contract and typed client learned the same thing.** `PortForward`
  gained `mode`, `url`, and `public_port`; `createPortForward` accepts
  `{ hostPort }`. Previously `internal/apiv1` dropped the URL entirely, so a
  `/v1` client on a URL-only worker received a mapping it could not reach.

### Changed

- **`PortForwardResource.hostPort` is now optional**, because a URL-only
  exposure genuinely has no worker-local host port. The `/v1` contract no
  longer requires `host_port`, and the field is omitted rather than reported
  as `0` — a value nothing can dial, and one that violated the contract's own
  `minimum: 1`. Read `mode` first:

  ```ts
  const addr = forward.mode === 'url' ? forward.url! : `${host}:${forward.hostPort}`
  ```

  Only the `/v1` `SandboxClient` surface is affected; the facade's
  `PortMapping.hostPort` was already optional. **Heads-up for a minor
  release:** code that reads `hostPort` as a plain `number` becomes a compile
  error under `strict` (`TS2322`/`TS18048`). That is deliberate — the
  alternative is a silent `":0"` in a URL at runtime — but it does mean `tsc`
  may fail on upgrade. The fix is the one-line guard above.

Supported server contract: `/v1` (`api/openapi.yaml` version `1.1.0`).

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
