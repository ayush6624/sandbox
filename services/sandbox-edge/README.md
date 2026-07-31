# sandbox-edge

`sandbox-edge` is the isolated public data plane for sandbox ingress. It
terminates wildcard TLS, routes `<guest-port>-<sandbox-id>.<domain>` from SNI,
resolves the current worker through the gateway, and opens the worker's opaque
`CONNECT` tunnel. User traffic never passes through the gateway.

The subtree is deliberately self-contained:

```text
services/sandbox-edge/
  cmd/sandbox-edge/       process entry point and flags
  internal/edge/          TLS, routing cache, tunnel, redirect, metrics
  sandbox-edge.service    example systemd unit
  sandbox-edge.env.example
```

It imports no worker or gateway implementation packages. Its only integration
surface is HTTP, which keeps a future extraction into a separate Go module or
repository mechanical.

## Run

Provision one DNS wildcard (`*.sb.example.com`) to the edge IP and a matching
wildcard certificate. DNS-01 is required for normal wildcard issuance; manage
and renew the certificate outside the process, storing the shared files at the
paths passed below.

```bash
go build -o bin/sandbox-edge ./services/sandbox-edge/cmd/sandbox-edge

bin/sandbox-edge \
  --listen :443 \
  --http-listen :80 \
  --metrics-listen 127.0.0.1:9091 \
  --domain sb.example.com \
  --cert-file /var/lib/sandbox-edge/tls/fullchain.pem \
  --key-file /var/lib/sandbox-edge/tls/privkey.pem \
  --gateway http://10.0.0.2:9090 \
  --gateway-token "$GATEWAY_TOKEN"
```

For local development, `--plain-http` routes from the HTTP `Host` header on
`--listen` and does not require certificate files. Production TLS advertises
ALPN `http/1.1` only; enabling HTTP/2 would allow browser connection coalescing
across sandbox hostnames while routing remains per connection.

Raw TCP uses a pre-bound port range and the gateway's durable GCS-backed
allocator:

```bash
  --raw-listen-ip 0.0.0.0 --raw-port-min 20000 --raw-port-max 29999
```

The metrics listener is intentionally separate and defaults to loopback. It
exports `/healthz` plus connection, byte, resolver, TLS certificate, wake
histogram, and raw TCP metrics. Keep it VPC-only.

## Worker configuration

Set `ingress_domain` on every worker so port responses include the public URL.
`default_url_only` changes the default for callers that omit `host_port`; it is
`false` by default for compatibility.

```json
{
  "ingress_domain": "sb.example.com",
  "default_url_only": true
}
```

Explicit `{"guest_port":3000,"host_port":false}` is URL-only regardless of the
default and consumes no worker port-pool entry. `true` preserves a local
host-port listener and also returns the URL.

Use a dedicated ingress domain and submit it to the Public Suffix List before
production use. Arbitrary sandbox content must not share a registrable site
with the API, marketing site, or other sandboxes.

## HA deployment

`infra/gcp/edge.sh` provisions a regional external passthrough Network Load
Balancer, a multi-zone MIG, both forwarding rules, health checks, firewall
rules, Secret Manager access, rolling updates, and connection draining.
`infra/gcp/edge-cert-renew.sh` performs scheduled ACME DNS-01 renewal on the
control VM. Edge replicas poll Secret Manager and hot-reload only a validated
certificate/key pair, retaining the last good pair during partial rotation.
