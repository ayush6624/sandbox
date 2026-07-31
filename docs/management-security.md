# Management transport security

Every TCP management listener must select one transport profile:

- `tls` serves TLS 1.3 using `--tls-cert` and `--tls-key`. Atomically replacing
  both files rotates the certificate on subsequent handshakes without
  interrupting established connections.
- `private_proxy` serves authenticated HTTP only on a concrete loopback,
  RFC 1918, or Tailscale CGNAT address. Wildcard, hostname, and public-IP binds
  fail closed. Use this when a private authenticated network or same-origin
  TLS proxy is the external boundary.
- `development` is the only plaintext/public compatibility mode. It must not
  be used in a production profile.

The local Unix socket remains the administrative path. `sandbox serve` refuses
to create it as a non-root process and sets owner `root:root`, mode `0600`.

## Credential trust domains

Fleet deployments use three unrelated bearer credentials:

1. The gateway client credential (`gateway --token` or `--token-file`) is used
   by SDKs, CLIs, Prometheus, and operator calls to the public API.
2. The gateway worker-control credential (`gateway --worker-token` or
   `--worker-token-file`) authenticates worker registration and `/internal/v1`.
3. The worker callback credential (`serve --worker-token` or
   `worker_token_file`) is sent in the registration heartbeat and is used only
   when the gateway calls that worker.

A client credential cannot call `/internal/v1`, and a worker registration
credential cannot call the gateway's public API. Outside `development`, the
gateway refuses equal client and worker-control credentials. Workers registered
with a gateway require a callback credential distinct from their optional
direct-client credential.

WebSocket handshakes use `Authorization: Bearer ...`, exactly like HTTP.
`access_token` query parameters are rejected and stripped before proxying;
secrets must not be embedded in URLs. Clients that cannot set headers (browsers)
authenticate with the `sandbox.bearer.<base64url(token)>` subprotocol instead —
accepted on public routes only, and stripped by the hop that consumes it so it
never reaches a worker or the guest. Request logs record `URL.Path`, never a raw
query string, credential header, or subprotocol list.

## Rotation procedure

Token files contain one active token per line; blank lines and `#` comments are
ignored. The first token is used for outbound control calls and every token is
accepted inbound. Files are reloaded after atomic replacement.

Rotate one trust domain at a time:

1. Replace the consumer's file with `new`, then `old` on separate lines.
2. Replace the producer's file with `new`, then `old`.
3. Confirm heartbeats and API probes.
4. Replace both files with only `new`.

Use a same-directory temporary file with mode `0600` and `rename(2)` it over
the live file. A missing, empty, or malformed intermediate replacement retains
the last known-good in-memory credentials. Initial startup fails if a configured
credential file is missing or empty.

For TLS rotation, write a validated certificate/key pair to temporary files and
atomically replace the configured paths. The listener retains the last
known-good certificate if it observes an incomplete replacement.

## Migration

The GCP deployment scripts generate and pass distinct
`GATEWAY_TOKEN`, `GATEWAY_CONTROL_TOKEN`, and `HOST_TOKEN` values. Management
listeners bind concrete VPC or Tailscale addresses with `private_proxy`.

For an existing custom deployment:

1. Generate the two new control credentials.
2. Upgrade the gateway with `--management-transport`, `--worker-token`, and
   optionally the reloadable token-file flags.
3. Upgrade workers with `--management-transport`, `--worker-token`, and the new
   gateway control credential in `--gateway-token`.
4. Set worker `--advertise` to an explicit `http://private-ip:port` or
   `https://host:port` URL.
5. Remove legacy shared credentials after every worker reports with
   `control_token`.

The old single-token behavior remains available only when both processes
explicitly select `development`.
