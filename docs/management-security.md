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

Fleet deployments use four unrelated bearer credentials:

1. The gateway client credential (`gateway --token` or `--token-file`) is used
   by SDKs, CLIs, Prometheus, and tenant calls to the public API.
2. The gateway worker-control credential (`gateway --worker-token` or
   `--worker-token-file`) authenticates worker registration, `/internal/v1`,
   and the legacy fleet-control aliases (`/hosts`, `/hosts/{host}/drain`,
   `/worker-release`).
3. The gateway edge credential (`gateway --edge-token` or `--edge-token-file`)
   authenticates the public ingress edge's `GET /route/{id}` and
   `GET /raw-route/{port}`.
4. The worker callback credential (`serve --worker-token` or
   `worker_token_file`) is sent in the registration heartbeat and is used only
   when the gateway calls that worker.

A client credential cannot call `/internal/v1`, and a worker registration
credential cannot call the gateway's public API. Outside `development`, the
gateway refuses equal client and worker-control credentials, and an edge
credential equal to either. Every check compares *all* active tokens in each
file, so an overlap introduced by a later rotation also fails closed at request
time rather than silently merging two domains. Workers registered with a gateway
require a callback credential distinct from their optional direct-client
credential.

### Why the edge is its own domain

`GET /route/{id}` and `GET /raw-route/{port}` return the owning worker's
control token to the caller — that is their whole purpose, since the edge dials
the worker directly and bulk bytes never traverse the gateway. Authenticating
them with the client credential therefore lets any API-key holder obtain
host-wide worker authority: every sandbox on that worker (`/exec`, `/files`,
`/shell`, `/connect/{port}`) plus its `/internal/v1` control routes. Fleet
control is separated for the same reason in the other direction: `GET /hosts`
discloses per-host addresses and capacity, `POST /hosts/{host}/drain` migrates
every sandbox off a host, and `PUT /worker-release` can force `slots_free=0`
fleet-wide and persists that across gateway restarts.

Route classification is by path, ahead of the mux, in
`internal/gateway.routeDomain`. `POST /sandboxes/{id}/raw-ports` stays a client
route: it acts on a sandbox the caller already owns and returns no worker
credential.

**Compatibility.** With no edge credential configured, `/route` and `/raw-route`
keep accepting the client credential and the gateway prints a startup `WARNING`
naming the disclosure. This exists so that shipping the gateway binary can never
take public ingress down before the edge has its own token; it is not a
supported production state. Once an edge credential *is* configured, those two
routes accept it and nothing else.

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
credential file is missing, empty, or group/world-accessible, and
`--edge-token`/`--edge-token-file` additionally fail closed when *passed empty*
(an unset shell variable expanded into a unit file) rather than silently
selecting the client-credential fallback. Omit both flags to select that
fallback deliberately.

On the GCP fleet, do not hand-edit the files: `control.sh` writes them on every
deploy, so a hand-made two-line file is reset to one line by the next rollout.
Put the outgoing value in `GATEWAY_TOKEN_PREV` or `GATEWAY_EDGE_TOKEN_PREV` in
`fleet-secrets.env` instead — `control-install.sh` writes it as the second line,
and clearing it completes the rotation. `validate-control-deploy.sh` asserts that
a gateway-only (`--fast`) rollout still passes both.

For TLS rotation, write a validated certificate/key pair to temporary files and
atomically replace the configured paths. The listener retains the last
known-good certificate if it observes an incomplete replacement.

## Migration

The GCP deployment scripts generate and pass distinct
`GATEWAY_TOKEN`, `GATEWAY_CONTROL_TOKEN`, `GATEWAY_EDGE_TOKEN`, and `HOST_TOKEN`
values. Management listeners bind concrete VPC or Tailscale addresses with
`private_proxy`.

### Introducing the edge credential on a running fleet

A greenfield fleet needs none of this: `control.sh` generates three distinct
gateway credentials and `edge.sh init` publishes the edge one. The migration
below is only for a fleet whose edge is already live on the client credential.

Two constraints drive the sequence, and together they rule out doing it in one
step:

- The edge instances read their credential from Secret Manager
  (`sandbox-edge-gateway-token`) **at boot**, so their presented token changes
  only when the MIG rolls.
- The old client token has to leave the client set in the *same* deploy it
  enters the edge set. It cannot be in both: `bearerAuth` requires
  `edgeMatch && !clientMatch`, so an overlap makes `/route` reject the edge, and
  the startup disjointness check refuses to boot that configuration at all.
  Nor can it leave the client set *first*: fallback-mode `/route` requires
  `clientMatch`, so removing it there rejects the edge immediately.

So consumer migration must complete **before** the demotion deploy. Phases 1 and
3 are therefore separate deploys and must not be collapsed — a single deploy
that both retires the old client token and installs it as the edge credential
has no instant at which the live edge is authenticated.

**Phase 1 — widen the client set.** In `infra/gcp/fleet-secrets.env` set
`GATEWAY_TOKEN` to a freshly generated `C_new` and `GATEWAY_TOKEN_PREV` to the
current `C_old`, then `control.sh gateway`. `client.tokens` becomes two lines and
both are accepted. No edge credential yet, so `/route` is still in fallback and
accepts the edge's `C_old`. Nothing is degraded.

**Phase 2 — migrate consumers.** Distribute `C_new`; confirm `C_old` is unused.
This is the long phase, and everything works throughout it. `GATEWAY_TOKEN_PREV`
lives in `fleet-secrets.env` precisely so a code rollout during this phase does
not rewrite `client.tokens` back to one line.

**Phase 3 — demote, and roll the edge onto a fresh credential.** One deploy plus
one MIG roll:

1. Clear `GATEWAY_TOKEN_PREV` (retiring `C_old` as a client credential), set
   `GATEWAY_EDGE_TOKEN` to a fresh `E_new`, and set `GATEWAY_EDGE_TOKEN_PREV` to
   `C_old`. Run `control.sh gateway`. `edge.tokens` is now `[E_new, C_old]` and
   `client.tokens` is `[C_new]` — disjoint, so the startup check passes, and the
   still-unrolled edges keep working on `C_old` via the edge domain.
2. `edge.sh init` publishes `E_new`; `edge.sh roll` restarts the instances. Both
   tokens are valid during the roll, so it costs no ingress.
3. Clear `GATEWAY_EDGE_TOKEN_PREV` and re-run `control.sh gateway` (or just
   atomically rewrite `/etc/sandbox-gateway/edge.tokens` to the single line —
   token files are hot-reloaded, no restart needed).

Do not stop after 3.1. `C_old` was handed to every API consumer, so for as long
as it remains in `edge.tokens` any past holder can still call `/route` and
harvest worker control tokens — the disclosure is only actually closed at 3.3.
`C_old` must be treated as compromised regardless, and `HOST_TOKEN` /
`GATEWAY_CONTROL_TOKEN` rotated too, since anyone who held it could already have
read the worker tokens out of `/route`.

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
