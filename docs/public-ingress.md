# Public ingress design — URL-addressable sandbox ports

Status: **implemented on the `public-ingress` branch; not yet rolled to the
fleet.** The design below is what shipped; §Implemented surface records the
concrete contracts, and [the E4/E5 plan](public-ingress-e4-e5-plan.md) carries
the remaining HA-edge and durable-raw-port work. Prereqs, all shipped: the
userspace port proxy (`internal/server/portproxy.go`), gateway routing from
heartbeats (`internal/gateway/gateway.go`), and cross-host wake (B4,
`docs/uffd-b4-design.md`).

## Implemented surface

Each explicitly exposed port gets a stable URL; raw protocols such as SSH get a
durable fleet-wide address:

```text
https://<guest-port>-<sandbox-id>.<ingress-domain>
<raw-public-host>:<public-port>
```

The public data plane is isolated in `services/sandbox-edge`. It imports no
gateway or worker implementation package and speaks only the HTTP contracts
below, so extracting it into its own module and deployment is mechanical.

```text
browser/SSH
    │
regional external passthrough Network Load Balancer
    │
sandbox-edge regional MIG
    ├── GET /route/{sandbox} ───────────────▶ gateway
    ├── GET /raw-route/{public-port} ───────▶ gateway
    └── CONNECT /sandboxes/{id}/connect/{port} ─▶ worker ─▶ guest
```

URL-only exposure consumes no worker host-port pool entry; raw TCP allocation is
gateway-only and idempotent per sandbox/guest-port; unexpose releases every URL,
worker-local listener, and raw lease:

```http
POST   /sandboxes/{id}/ports        {"guest_port":3000,"host_port":false}
POST   /sandboxes/{id}/raw-ports    {"guest_port":22}
       → 200 {"guest_port":22,"mode":"raw","public_host":"tcp.example.com","public_port":20000}
DELETE /sandboxes/{id}/ports/{guestPort}
```

CLI: `sandbox expose --url-only`, `sandbox expose --raw`, `sandbox ports`,
`sandbox unexpose`. TypeScript SDK: `exposePort`, `exposeRawPort`, `listPorts`,
`unexposePort`.

Durable raw leases live in `ingress/raw/index.json` in the configured GCS
bucket, every mutation an object-generation CAS, moving
`free → pending → active → releasing → free`. `pending` makes allocation
crash-safe across the worker commit and `releasing` makes unexpose/destroy
crash-safe; a background reconciler converges stale transitional leases against
worker mappings, and lease IDs keep stale cleanup from releasing a newly reused
public port. Workers persist `public_port` in SQLite and in durable hibernation
records and advertise it in heartbeats, so the address survives
hibernation, adoption, worker drains, gateway restarts, and edge rollouts.

## The gap

A sandbox's exposed port is reachable today only from inside the VPC/tailnet.
`POST /sandboxes/{id}/ports` allocates a host port and the client pairs it with
the owning host's address — the gateway injects that as `sb.HostAddr` on
`GET /sandboxes/{id}` (`hostOnly()`, gateway.go) precisely so callers can do
this. Workers have VPC IPs and a tailnet address, no public address. And the
gateway is an HTTP reverse proxy for the *API*; it does not forward raw TCP,
which is why fleet SSH is still an open item in CLAUDE.md.

So: a sandbox running a dev server on :3000 can't be opened in a browser by
anyone outside the tailnet, and `ssh -p <host_port> sandbox@<worker>` only works
from a machine that can route to the worker.

**Goal:** `https://3000-<id>.sb.example.com` reachable from the public internet,
preserving every property the port proxy has today — wake-on-connect, activity
pinning while a connection is open, guest-IP re-read after a clone-path wake,
and cross-host wake.

## Decisions

1. **A separate service (`sandbox edge`), not a gateway feature.** The gateway is
   a control plane: small requests, holds every host's bearer token, and holds no
   durable state so it self-heals from heartbeats. Port traffic is the opposite —
   raw TCP and WebSockets, untrusted user content, arbitrary bandwidth, public
   exposure. Folding it in makes user bandwidth compete with placement and
   heartbeat latency, and puts a public listener in the same process as the host
   tokens. Separate blast radius, separate scaling axis.
2. **The edge is stateless.** It resolves `id → host` from the gateway, caches
   with a short TTL, and re-resolves on dial failure. No DB, no heartbeat feed of
   its own — the gateway stays the single routing authority.
3. **The data hop is a raw-TCP tunnel on the worker API, not `edge → worker:hostPort`.**
   See §Why not route to the host port. This is the one decision that materially
   changes the shape of the thing.
4. **`expose` stays the authorization gate.** e94c9f9 made explicit exposure
   deliberate; a public URL must not undo it. What gets decoupled is exposure
   from *host-port allocation* — a mapping may be URL-only, consuming no port
   from the pool.
5. **Flat single-label subdomain: `<port>-<id>`, not `<port>.<id>`.** Let's
   Encrypt won't issue `*.*.sb.example.com`, so nested labels force a cert per
   sandbox. One DNS-01 wildcard covers the flat scheme.
6. **Raw TCP (SSH) is a separate listener, not an HTTP concern.** SSH isn't TLS,
   so there's no SNI to route on. Same binary, different port strategy — §SSH.
7. **A dedicated domain, on the Public Suffix List.** Not a subdomain of anything
   you care about — §Security.

## Why not route to the host port

`edge → worker:hostPort` is the obvious wiring and it needs zero worker changes:
the connection lands on the existing `portForwarder` listener and inherits
wake-on-connect, `act.begin` pinning, and the post-wake guest-IP re-read for
free. It's a legitimate Phase 1 if we want something on a URL this week.

It has two costs that get worse over time:

- **Two-part routing.** The edge must resolve `id → (host, hostPort)`, and *both*
  halves change when a sandbox is adopted onto another host (B4 cross-host wake,
  or a drain). Cache invalidation now spans a tuple that can change underneath a
  live connection attempt.
- **Every public URL burns a host port.** Ports are drawn from the configured
  pool and stay hard-reserved through hibernation (`uniq_port_held` covers
  hibernated rows; `loadUsed` counts them) — which CLAUDE.md already identifies
  as the real ceiling on *total* sandboxes per host, running plus hibernated.
  Making public URLs the primary access path would put user-facing addressability
  in direct competition with hibernated-sandbox density.

The alternative is a tunnel endpoint on the worker's existing API:

- the edge only ever needs `id → host` — one cache key, trivially invalidated
- no port-pool consumption, so public URLs stop competing with capacity
- it's `dialGuest` behind a different front door: `ensureRunning` (wakes if
  hibernated), re-read the row for the current guest IP, dial, bidirectional copy
  with TCP half-close — all of it already written and fleet-proven
- auth is the host bearer token the edge already needs, over the same TCP
  listener, with no new trust boundary

## Architecture

```
browser                      edge (public IP, MIG)          gateway            worker
  │                                │                           │                 │
  │─ TLS ClientHello ─────────────▶│                           │                 │
  │   SNI 3000-<id>.sb.example.com │                           │                 │
  │                                │─ resolve id (cached) ────▶│                 │
  │                                │◀─ host addr + token ──────│                 │
  │                                │                                             │
  │                                │─ CONNECT /sandboxes/<id>/connect/3000 ─────▶│
  │                                │   Authorization: Bearer <host token>        │
  │                                │◀─ 200 ─────────────────────────── wake if ──│
  │                                │                             hibernated, pin │
  │◀═══════ opaque bytes ═════════▶│◀════════ opaque bytes ════════════════════▶ guest:3000
```

Routing is L7 (hostname), the payload is L4 (opaque bytes). The edge terminates
TLS, extracts the sandbox id and port from SNI, and from there copies bytes — it
does **not** parse HTTP. WebSockets, SSE, gRPC, long-lived streams and
`Upgrade`-based protocols all work with no per-protocol handling, and there's no
Host-rewriting or hop-by-hop-header surface to get wrong.

**h2 coalescing hazard.** A wildcard cert covers every sandbox hostname, so a
browser may reuse one HTTP/2 connection for requests to *different* sandboxes
(same cert, same IP → connection coalescing). Since routing is per-connection,
that would deliver sandbox A's requests to sandbox B. Mitigation: advertise
**ALPN `http/1.1` only** at the edge. Supporting h2 later means parsing frames
and routing per-stream on `:authority` — don't, unless a measurement demands it.

## Worker endpoint

```
CONNECT /sandboxes/{id}/connect/{guestPort}
Authorization: Bearer <host token>
```

`200` then the connection becomes an opaque bidirectional byte stream to
`guestIP:guestPort`. Implementation is `dialGuest(ctx, id, guestPort)` plus the
existing `pipe()`/`closeWrite()` helpers, hijacking the `http.ResponseWriter`.

- **Auth**: host bearer token, same middleware as the rest of the TCP listener.
- **Authorization**: `404` unless the guest port has an exposed mapping for that
  sandbox. This is the enforcement point for decision 4 — the edge does not get
  to reach an unexposed port.
- **Wake**: `ensureRunning` first, so a cold URL hit wakes a hibernated sandbox
  exactly like a forwarded-port connection does. Bound it with the same
  `portDialTimeout` (90 s) budget; the caller sees a slow first byte, not an
  error.
- **Pinning**: `act.begin(id)` for the life of the tunnel, so an open browser tab
  keeps the sandbox out of the reaper — identical to the port-proxy contract.
- **Errors before the 200**: plain HTTP statuses (`401`, `404`, `503` on a
  memory-admission-rejected wake, `502` agent unreachable). The edge maps these
  onto an HTTP error page for the user, since it hasn't sent bytes yet.
- **A WebSocket variant** (`GET …/connect/{port}` with `Upgrade: websocket`,
  binary frames as raw bytes) is worth adding for the same reason `/shell` is a
  WS endpoint: it traverses proxies that mangle `CONNECT`, and it gives browsers
  a direct path. Errors then follow the `wsutil.Reject` convention already used
  by `/shell` — post-handshake close frame, code `4000 + status`.

Gateway side: it already proxies `/sandboxes/{id}/…` to the owning host, so the
endpoint is reachable through the gateway with no gateway change. The edge should
still resolve-then-dial-the-host-directly for the data path, so bulk traffic
never transits the gateway process.

## Registry change

`sandbox_ports` currently requires a host port (`host_port INTEGER NOT NULL`,
`uniq_host_port`). URL-only exposure needs a mapping row with no host port:

- add a nullable `host_port` (or an explicit `mode` column: `host_port` | `url` |
  `both`), keeping `uniq_host_port` on non-null values
- `POST /sandboxes/{id}/ports` takes `{"guest_port": N, "host_port": bool}` —
  default per config, so today's behavior stays available
- `syncSandboxPorts` / `reopenPortListeners` skip URL-only rows (no listener)
- `Ports()` and the API response report the mode, so the SDK can build the right
  address; `GET /sandboxes/{id}` gains a `url` per exposed port when the server
  is configured with an ingress domain
- pool accounting (`loadUsed`, `FreeSlots`) must not count URL-only rows

## Resolution and caching

The edge needs `id → (host addr, host token)`. Two options:

- **Reuse `GET /sandboxes/{id}`** through the gateway with the gateway token: the
  response already carries `host_addr`. But not the host *token*, so the edge
  would have to hold the per-host tokens out of band or proxy the data path
  through the gateway (rejected above).
- **A dedicated gateway endpoint**, `GET /route/{id}` → `{host_addr, token,
  ttl}`, gateway-token-authed. Explicit, cheap, and it keeps the gateway the sole
  owner of "which host, which credential". Preferred.

Cache with a short TTL (~5 s, i.e. roughly one heartbeat) plus:

- **single-flight** per id, so a burst of connections to a cold URL makes one
  resolve call
- **negative caching** (~1 s) for unknown ids, so scanners can't turn wildcard
  DNS into a gateway amplifier
- **invalidate on dial failure and retry once.** This is what makes cross-host
  wake work: a sandbox adopted onto another host resolves to the stale host,
  the dial fails, the edge re-resolves and hits the new owner — the same
  resolve-on-miss shape `handleProxyByID` already uses (`resolveViaAdopt`).

Unknown or destroyed sandbox → a real HTML error page, not a bare `404`; this is
a user-facing surface.

## TLS and DNS

- Wildcard `*.sb.example.com`, DNS-01 (the sandbox subdomain has no public HTTP
  to answer HTTP-01 for), auto-renewed. Caddy does this in a few lines; a Go edge
  can use `autocert` with a DNS solver.
- One `A`/`AAAA` for `*.sb.example.com` at the edge. No per-sandbox DNS record —
  that's the whole point of the wildcard, and it's what keeps expose from
  touching a DNS API (rate limits, propagation delay, cleanup on destroy).
- Certs must be **shared across edge replicas** (a shared store, or a single
  ACME-holding replica distributing them) — N replicas each solving DNS-01 for
  the same name will trip rate limits.
- `http://` on :80 redirects to `https://`. Leave a plain-HTTP mode behind a flag
  for local/dev domains.

## SSH and raw TCP

The HTTP edge does nothing for SSH: no TLS, no SNI, nothing to route on before
the client speaks. Two options:

- **A public TCP port range on the edge** (e.g. `:20000–29999`), allocated per
  exposed raw port and returned as `{host, port}`. Pure L4, works for SSH and any
  other protocol, no pretty URL. The edge's own port pool is then a scaling
  ceiling, but it's a *fleet-wide* pool at the edge rather than a per-worker one.
  This closes the `ssh -J` gap in CLAUDE.md with the least machinery, and it
  reuses the same tunnel endpoint for the worker hop.
- **An SSH-username router** (`ssh sandbox-<id>@sb.example.com`, à la Coder):
  one port, nice UX, but the proxy must terminate SSH auth and hold host keys —
  a real MITM with real key management. Not worth it until SSH is a primary
  interface.

Recommendation: the port range. Note that the base rootfs already bakes
`openssh-server` with key-only login as the unprivileged `sandbox` user and
`ssh.service` enabled, and
`POST /sandboxes` takes `ssh_pubkey` — so the guest side is done; this is purely
the missing public path.

## Security

- **Cookie and origin isolation.** Public sandbox URLs put arbitrary user content
  on the domain. Sibling sandboxes are same-site to each other, so cookies set
  with `Domain=.sb.example.com` (or `__Host-`-less cookies at all) leak between
  sandboxes, and `document.domain`-adjacent tricks apply. Use a **dedicated
  domain** — never a subdomain of the marketing site or the API — and get it onto
  the **Public Suffix List** so browsers treat each sandbox hostname as its own
  registrable site.
- **Abuse.** Anything reachable at a URL will be used to host phishing. Expect
  Safe Browsing hits against the domain (another reason it must be dedicated),
  and plan for: an abuse contact, per-sandbox kill from the gateway, and rate
  limits on new-hostname first-hits.
- **Unguessable by default.** Sandbox ids are UUIDs, so a URL is unguessable
  without being secret — that's the e2b model and it's a reasonable default.
  Add an opt-in `require_token` mode (signed cookie set via a
  `?access_token=`-style handshake at the edge) for sandboxes that shouldn't be
  world-open. Decide the default before shipping; changing it later is a breaking
  change for users' links.
- **Egress-side headers.** The edge should strip nothing and add nothing to the
  guest's bytes (it can't — it doesn't parse HTTP), which means no
  `X-Forwarded-For`. If a guest app needs the client IP, that's an argument for
  an optional HTTP-parsing mode later, not for parsing by default.
- The edge holds host bearer tokens in memory. It's public-facing, so it's the
  most exposed component in the fleet — keep it minimal, no shell, no debug
  endpoints on the public listener.

## Capacity and operations

- The edge is a **bandwidth funnel** and, as one instance, a SPOF for all
  sandbox traffic. Start with one VM (a `sandbox-edge` MIG of size 1) in the same
  VPC as the workers so the second hop is internal — the edge does not need the
  tailnet; only the laptop does. Grow to a regional external L4 LB in front of an
  N-instance MIG when it saturates. The service is stateless apart from certs,
  which is what makes that trivial.
- **Metrics** (same `/metrics` convention as host + gateway):
  `sandbox_edge_conns_open`, `…_conns_total{result}`, `…_bytes_total{dir}`,
  `…_resolve_total{result}` (hit/miss/stale/unknown), `…_wake_seconds`,
  `…_tls_handshakes_total{result}`. Per-sandbox byte counters are useful for
  abuse and billing but cardinality-hostile — aggregate, and log per-connection.
- **Grafana**: add an edge row to the existing *Sandbox Fleet* dashboard.

## Phases

| Phase | Scope | Proves |
| --- | --- | --- |
| **E1** | Worker `CONNECT /sandboxes/{id}/connect/{port}` + gateway `GET /route/{id}`. No edge yet — test with curl from the control VM. | The tunnel, wake-on-connect through it, and pinning |
| **E2** | `sandbox edge`: TLS termination, SNI parse, resolve+cache, tunnel. Single VM, wildcard DNS, staging domain. | End-to-end `https://3000-<id>.…` in a browser |
| **E3** | URL-only exposure (registry mode column, ports API flag, `url` in responses, SDK support). | Public URLs stop consuming the port pool |
| **E4** | Raw-TCP port range on the edge → public SSH. | Closes the `ssh -J` gap |
| **E5** | Edge MIG behind an L4 LB + shared cert store; metrics and dashboard row. | HA, and a known bandwidth ceiling |

E1+E2 is the smallest thing that's actually useful; E3 can be deferred if the
port pool has headroom, and E1 can be skipped entirely (route to `worker:hostPort`)
if we want a demo before the tunnel exists.

## Non-goals

- HTTP parsing at the edge (no header injection, no path routing, no caching, no
  request logging beyond connection-level).
- Per-sandbox DNS records or per-sandbox certs.
- HTTP/2 or HTTP/3 to the client (ALPN `http/1.1` only — see the coalescing
  hazard).
- Custom domains per sandbox (`CNAME` + per-name cert issuance). Later, if ever.
- Outbound/egress control. This doc is ingress only; guest egress stays
  NAT+MASQUERADE as today.

## Alternatives considered

| Alternative | Why not |
| --- | --- |
| **Public IP + wildcard cert per worker**, URL embeds the worker (`3000-<id>.worker7.sb.example.com`). No edge, one hop, no bandwidth funnel. | Killed by cross-host wake: B4 can adopt a hibernated sandbox onto a different host, and a drain can move one off a live host — the URL would break. Also exposes every worker publicly, needs per-worker DNS, and distributes certs to every host. The indirection is worth its cost *because* of B4. |
| **Route the data path through the gateway.** | Mixes a public data plane into the control plane: user bandwidth against heartbeat/placement latency, and the process holding every host token gets a public listener. Rejected in decision 1. |
| **Tailscale Funnel.** | Per-node, not per-sandbox; the nodes are workers. Doesn't address thousands of sandbox hostnames, and puts sandbox traffic through Tailscale's relays. |
| **GCLB with per-sandbox URL map entries.** | URL map updates take minutes and are quota-limited; sandbox lifetimes are seconds. Wrong control-plane timescale. |
| **Cloudflare Tunnel (`cloudflared`) per worker.** | Genuinely attractive *in front of* this design for TLS, DDoS, and no public IP. But CF can't know which worker owns a sandbox, so it doesn't remove the resolver — and keeping CF routes in sync per expose is a control-plane write per sandbox against an external rate-limited API. An add-on, not a replacement. |
| **Per-sandbox `cloudflared`/`ngrok` inside the guest.** | A process and an outbound tunnel per sandbox: memory in the guest (where it's admission-charged), a third-party dependency in the data path, and it breaks across hibernation. |
