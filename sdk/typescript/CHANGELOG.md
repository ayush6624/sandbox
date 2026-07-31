# Changelog

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
