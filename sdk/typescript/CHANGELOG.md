# Changelog

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
