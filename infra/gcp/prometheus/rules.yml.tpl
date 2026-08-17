# sandbox:workers_desired is the provider's actual MIG target, polled and
# exported by the gateway that exclusively owns scale-out and scale-in. Do not
# reconstruct it from Prometheus demand arithmetic: that created a second,
# observability-only implementation which drifted from memory-aware gateway
# demand and showed 5 desired workers while the gateway had correctly requested
# 16 for a 4 GiB workload. Missing gateway/provider data deliberately produces
# an absent series instead of a confident zero.
groups:
  - name: sandbox
    # Recompute on the 5s gateway scrape cadence. A 10s scrape + 10s rule
    # evaluation left too little of the 30s create-latency SLO for standby
    # resume and sandbox bring-up.
    interval: 5s
    rules:
      - record: sandbox:workers_desired
        expr: max(sandbox_mig_target_size{job="sandbox-gateway"})

  - name: public-ingress
    interval: 15s
    rules:
      - alert: SandboxEdgeReplicaQuorumLost
        expr: sum(up{job="sandbox-edge"}) < 2
        for: 2m
        labels: { severity: critical }
        annotations:
          summary: Fewer than two public edge replicas are healthy
      - alert: SandboxEdgeCertificateExpiring
        expr: min(sandbox_edge_certificate_expiry_timestamp_seconds{job="sandbox-edge"}) - time() < 7 * 24 * 60 * 60
        for: 15m
        labels: { severity: warning }
        annotations:
          summary: Public ingress certificate expires within seven days
      - alert: SandboxEdgeCertificateReloadFailure
        expr: sum(increase(sandbox_edge_certificate_reloads_total{job="sandbox-edge",result="error"}[15m])) > 0
        labels: { severity: warning }
        annotations:
          summary: An edge replica rejected a certificate update
      - alert: SandboxEdgeResolveFailures
        expr: sum(rate(sandbox_edge_conns_total{job="sandbox-edge",result="error"}[5m])) / clamp_min(sum(rate(sandbox_edge_conns_total{job="sandbox-edge"}[5m])), 0.01) > 0.05
        for: 5m
        labels: { severity: warning }
        annotations:
          summary: More than 5% of edge connections are failing
      - alert: SandboxRawPortPoolNearExhaustion
        expr: sum(sandbox_raw_leases{job="sandbox-gateway",state=~"active|pending|releasing"}) / ${RAW_PORT_CAPACITY} > 0.9
        for: 10m
        labels: { severity: warning }
        annotations:
          summary: Raw public port pool is over 90% allocated
      - alert: SandboxRawLeaseReconciliationStuck
        expr: sum(sandbox_raw_leases{job="sandbox-gateway",state=~"pending|releasing"}) > 0
        for: 10m
        labels: { severity: warning }
        annotations:
          summary: Durable raw port lease reconciliation is stuck
