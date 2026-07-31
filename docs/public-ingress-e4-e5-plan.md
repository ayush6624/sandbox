# Public ingress E4–E5 implementation plan

Status: **implementation-ready design**. E1–E3 exist on branch
`public-ingress`. This document resolves the missing durable raw-port allocator
and turns E5 into concrete service and GCP work.

## Priority

- **E5 is required before production launch.** A single edge VM is a fleet-wide
  SPOF and bandwidth funnel. Production also needs certificate renewal,
  draining, health checks, metrics discovery, alerts, and a tested rollout.
- **E4 is conditional.** Build it now only if public SSH or another non-TLS
  protocol is a launch requirement. HTTPS/WebSockets/SSE do not depend on it.
  E4 adds a scarce public address space and an abuse surface, so it should not
  block an HTTP-only launch.

Recommended order: E5 service hardening → E4 control-plane/data-plane work →
E5 GCP rollout and load testing. The hardening is useful even if E4 is deferred.

## E4: durable public TCP ports

### Ownership model

The edge process stays stateless. Raw allocations live in a small,
generation-CASed object in the existing GCS durability bucket:

```
ingress/raw/index.json
```

The gateway owns the allocator but not the storage: it loads the index on
startup and updates it with GCS `ifGenerationMatch`. This preserves the
gateway's restartability and prevents two gateway replicas from allocating the
same port.

Each lease contains:

```json
{
  "public_port": 20042,
  "sandbox_id": "uuid",
  "guest_port": 22,
  "lease_id": "uuid",
  "state": "pending|active",
  "created_at": "RFC3339",
  "updated_at": "RFC3339"
}
```

The worker also stores `public_port INTEGER NULL` on `sandbox_ports`. That copy
is the sandbox-owned durable metadata carried through hibernation/adoption. The
GCS index is the fleet-wide uniqueness authority and keeps ports reserved while
the owning worker is offline.

### Allocation transaction

`POST /sandboxes/{id}/raw-ports`

```json
{"guest_port":22}
```

1. Gateway authenticates the client and resolves the current owner.
2. Under the allocator mutex, return the existing `(sandbox,guest)` lease when
   present; otherwise choose a free port from the configured range.
3. CAS a `pending` lease into the GCS index. The durable reservation happens
   before touching the worker, so a crash cannot double-allocate the port.
4. Call the owning worker's internal endpoint to expose the guest port
   URL-only and store the chosen `public_port`.
5. CAS the lease to `active`, publish it in the gateway's in-memory raw-route
   map, and return:

```json
{
  "guest_port":22,
  "mode":"raw",
  "public_host":"tcp.sb.example.com",
  "public_port":20042
}
```

If step 4 fails, remove the still-owned pending lease with a generation CAS. A
startup/background reconciler examines pending leases older than two minutes:
query the sandbox when it has a route, activate if the worker committed the
same lease, otherwise release it. Every mutation carries `lease_id`, so cleanup
can never delete a later allocation that reused the same port.

### Release and lifecycle

Add `DELETE /sandboxes/{id}/ports/{guestPort}` for all exposure modes.

- Worker closes any worker host-port listener and deletes the mapping.
- Gateway intercepts a successful raw unexpose/destroy and CAS-removes its
  lease. If cleanup fails, the lease stays reserved and the reconciler retries;
  leaking temporarily is safer than retargeting a public port.
- Durable hibernation records include `public_port`; cross-host adoption must
  restore it exactly, never allocate a replacement.
- Gateway startup loads the GCS index before advertising raw allocation
  readiness. `GET /raw-route` can serve active leases immediately, resolving
  the current worker through the normal sandbox route/adopt path.
- Heartbeats include raw mappings as a consistency signal. A mismatch with the
  GCS index is logged and counted, but never silently overwrites the durable
  authority.

### Routing API

`GET /raw-route/{publicPort}` (gateway-token authenticated):

```json
{
  "sandbox_id":"uuid",
  "guest_port":22,
  "host_addr":"10.0.0.8:8080",
  "token":"worker-token",
  "ttl":5
}
```

Unknown, pending, or conflicted leases return `404`; an allocation index that
has not loaded returns `503`. The edge caches positive results for at most five
seconds, negative results for one second, and invalidates/retries once on worker
dial failure or stale-worker `404`.

### Edge raw listener

Every replica binds the complete configured range (initially
`:20000–:29999`) at startup. Ten thousand idle TCP listeners are acceptable
with `LimitNOFILE=1048576` and avoid a synchronization race where the load
balancer selects a replica that has not yet learned a new allocation.

On accept:

1. The listener closure supplies the destination public port.
2. Resolve `/raw-route/{port}` through the raw single-flight cache.
3. Open the existing authenticated worker `CONNECT` tunnel.
4. Copy opaque bytes with the existing half-close behavior.

There is no TLS, HTTP error page, or protocol parsing. Unknown ports close
immediately. Add per-source connection-rate limiting before resolution so
internet scans cannot amplify into gateway requests.

### E4 configuration and metrics

Gateway:

```
--ingress-bucket
--raw-public-host
--raw-port-min=20000
--raw-port-max=29999
```

Edge:

```
--raw-listen-ip=0.0.0.0
--raw-port-min=20000
--raw-port-max=29999
--raw-first-hit-rate=...
```

Metrics:

- `sandbox_edge_conns_open{kind="https|raw"}`
- `sandbox_edge_conns_total{kind,result}`
- `sandbox_edge_bytes_total{kind,dir}`
- `sandbox_edge_raw_resolve_total{result}`
- `sandbox_raw_leases{state}`
- `sandbox_raw_allocations_total{result}`
- `sandbox_raw_reconcile_total{result}`
- `sandbox_raw_index_generation`

Do not label metrics by sandbox or public port; log those per connection.

### E4 tests and completion gate

- GCS fake-server tests: create-only CAS race, stale generation retry,
  pending→active, rollback, and lease-id-safe cleanup.
- Registry migration and hibernation/adoption tests preserve `public_port`.
- Gateway tests allocate the whole range exactly once under concurrency,
  remain idempotent, recover pending leases, and never route pending leases.
- Edge `net.Pipe` tests verify SSH-like opaque bytes, half-close, cache retry,
  rate limiting, and unknown-port behavior.
- End-to-end: expose port 22, SSH through the public port, hibernate and wake,
  drain/adopt to another worker without changing the public address, restart
  gateway and all edge replicas, then reconnect at the same address.
- Destructive test: kill the gateway between pending reservation and worker
  commit; restart must converge without a duplicate or permanent leak.

## E5: HA edge and operations

### Service hardening first

Before creating more replicas:

1. Add `/healthz` on the private metrics listener. It is healthy only after all
   configured public listeners are bound and a non-expired certificate is
   loaded. Do not make gateway reachability part of health; a gateway incident
   must not cause the load balancer to remove every edge simultaneously.
2. Add certificate hot reload through `tls.Config.GetCertificate`. Reload
   atomically when the certificate/key files change; keep serving the last good
   pair on a bad partial update. Export certificate expiry and reload results.
3. Add graceful drain: on SIGTERM, fail readiness, close public listeners, and
   wait up to `--drain-timeout` for existing tunnels before exit.
4. Bound handshakes, request-header reads in development mode, resolver calls,
   and worker CONNECT setup. Add per-IP new-connection limits and global
   connection ceilings; established streams remain unthrottled initially.
5. Change wake latency from a summary to a fixed-bucket histogram so fleet
   quantiles aggregate correctly across replicas.

### GCP topology

Use a **regional external passthrough Network Load Balancer** in the workers'
region. It preserves the original destination port and supports explicit ports
and ranges.

```
wildcard DNS ──▶ reserved regional IPv4
                    │
         forwarding rule TCP 80,443
         forwarding rule TCP 20000-29999 (when E4 enabled)
                    │
          regional backend service
          connection draining: 300 s
                    │
       regional MIG, minimum 2 replicas,
       spread across at least 2 zones
```

The passthrough load balancer does not terminate TLS. Each edge VM terminates
the wildcard certificate and binds the raw destination ports directly.

Create `infra/gcp/edge.sh` with `init|up|roll|status|down`, mirroring `mig.sh`:

- reserve the regional public IPv4;
- create the edge service account and least-privilege IAM;
- create firewall rules for public TCP `80,443` and, conditionally, the raw
  range; create a separate health-check rule for Google probe ranges to the
  private health port;
- create the regional health check, backend service, forwarding rules,
  instance template, and regional MIG;
- roll with zero avoidable downtime and a five-minute drain budget;
- never delete the reserved IP, DNS, secrets, or certificate material in
  `down` unless an explicit `purge` command is used.

Start with a fixed size of two. Add CPU/network autoscaling only after measured
bandwidth and connection limits identify a safe target; scale-to-zero is never
valid for this fleet-wide entry point.

### Certificate ownership

Because passthrough load balancing cannot use Google-managed TLS certificates,
use one issuer and versioned distribution:

1. A control-plane systemd timer runs an ACME client using Cloud DNS DNS-01 for
   `*.sb.example.com`.
2. On successful renewal it creates new Secret Manager versions for the full
   chain and private key.
3. Edge service accounts have `secretAccessor` only for those two secrets.
4. An edge-side timer fetches both into a temporary directory, verifies that
   the key matches the certificate and that the name is covered, then atomically
   renames them into place.
5. `sandbox-edge` hot-reloads the pair. Alert at 21 and 14 days to expiry.

Do not issue independently on every replica; that risks ACME rate limits and
inconsistent certificates.

### Artifact and secret delivery

- Publish `bin/sandbox-edge` beside the existing release artifacts in GCS.
- The edge template receives only release object/version identifiers and
  Secret Manager resource names as metadata, never bearer tokens or private
  keys.
- Store the gateway token in Secret Manager and fetch it at boot into a
  root-readable environment file. Rotate by rolling the gateway and edge with
  an overlap strategy before removing the old token.
- Give edge VMs VPC-only access to the gateway and worker API ports. Workers
  remain non-public.

### Prometheus, dashboard, and alerts

Expose metrics/health on `0.0.0.0:9091`, firewalled to the control VM and health
probers. Add Prometheus GCE service discovery filtered by the edge instance
label, rather than a static replica list.

Add a *Public ingress edge* dashboard row:

- healthy/desired replicas and certificate days remaining;
- open connections and new connections/s by kind/result;
- ingress/egress bytes/s per replica and aggregate;
- route cache hit/miss/stale/unknown rates;
- worker tunnel setup error rate;
- wake/tunnel-setup latency p50/p95/p99;
- first-hit rate-limit drops and global-capacity rejects.

Alerts:

- fewer than two healthy replicas for five minutes;
- certificate expiry below 21 days (warning) or 14 days (critical);
- error rate >5% excluding unknown-hostname scans;
- sustained bandwidth, CPU, file-descriptor, or open-connection saturation;
- raw allocator pending leases older than two minutes or any lease conflict;
- no successful gateway resolution from any edge for one minute.

### Rollout and completion gate

1. Deploy two replicas on a staging domain and reserved staging IP.
2. Run protocol coverage: HTTP/1.1, WebSocket, SSE, long upload/download,
   gRPC-web/Upgrade where applicable, and E4 SSH.
3. Exercise cold wake and cross-host adoption while traffic is active.
4. Rolling-replace one replica; existing long connections must get the drain
   window and new connections must continue through the other replica.
5. Kill one zone's replicas and then the gateway; verify expected isolation and
   recovery.
6. Load test to the planned concurrent-connection and bandwidth ceiling with
   file descriptors, memory, CPU, and packet loss recorded.
7. Cut wildcard DNS with a low TTL, canary traffic, then raise TTL after 24
   hours. Keep the prior endpoint available for rollback.

E5 is complete when the edge survives replica/zone replacement without a
public outage, renews and reloads a staged certificate automatically, has a
measured capacity ceiling, and pages before TLS expiry or capacity exhaustion.

## Implementation slices

| Slice | Scope | Depends on |
| --- | --- | --- |
| **H1** | Health/readiness, certificate hot reload/expiry metric, graceful drain, aggregateable latency histogram | current E2 |
| **R1** | `public_port` registry migration, durable-record propagation, unexpose API | current E3 |
| **R2** | GCS raw-index CAS package, gateway allocator/reconciler and raw-route API | R1 |
| **R3** | Edge raw listeners/cache/rate limiting, CLI/SDK support | R2 |
| **H2** | `infra/gcp/edge.sh`, regional MIG/NLB/firewalls, artifact and Secret Manager delivery | H1; R3 only if raw ships |
| **H3** | Prometheus discovery, Grafana row, alerts, staging/load/failure tests | H2 |

H1 and R1 can be implemented in parallel. The first deployable milestone is
H1+H2 for HA HTTPS ingress. Add R2+R3 before H2 only when public SSH is a launch
requirement.
