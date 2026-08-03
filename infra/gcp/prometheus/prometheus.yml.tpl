# Rendered by control-install.sh (envsubst):
# ${GATEWAY_TOKEN}, ${CONTROL_IP}, ${GW_PORT}, ${PROJECT}, and EDGE_GCE_SD.
#
# Write EDGE_GCE_SD as a BARE name everywhere except its one substitution site
# below — never in the dollar-brace form, not even inside a comment. envsubst has
# no idea what a YAML comment is, so a dollar-brace mention up here expands the
# multi-line block into the middle of this very comment: the first line stays
# commented, every line after it becomes top-level YAML, and the file dies with
# "field zone not found in type config.plain". Single-line values like ${PROJECT}
# are safe to mention this way; a multi-line one is not.
# The gateway deliberately listens only on the private VPC address, so local
# control-plane consumers must use that address rather than loopback.
global:
  scrape_interval: 5s
  # The held-burst SLO includes worker resume and sandbox bring-up, so a 10s
  # scrape plus 10s rule evaluation consumed too much of the 30s p95 budget
  # before GCE even saw the resize. These gateway-only operations are cheap;
  # keep them at 5s while the fan-out worker scrape below remains at 30s.
  evaluation_interval: 5s

rule_files:
  - /etc/prometheus/rules.yml

scrape_configs:
  - job_name: sandbox-gateway
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials: ${GATEWAY_TOKEN}
    static_configs:
      - targets: ["${CONTROL_IP}:${GW_PORT}"]

  # Per-host detail, federated through the gateway: it scrapes each live
  # worker's /metrics (it holds their addr+token) and re-exports every series
  # with a host="<id>" label. This means Prometheus still scrapes only the
  # gateway on its private address — the dynamic Nomad worker fleet needs no service
  # discovery — while we get per-host pool/memory/lifecycle series. A
  # sandbox_host_scrape_ok{host} gauge flags any worker the gateway couldn't
  # reach. Slower interval than the gateway's own aggregate: this fans out to
  # every worker.
  - job_name: sandbox-hosts
    metrics_path: /metrics/hosts
    scrape_interval: 30s
    authorization:
      type: Bearer
      credentials: ${GATEWAY_TOKEN}
    static_configs:
      - targets: ["${CONTROL_IP}:${GW_PORT}"]

  # Edge instances are a regional MIG, so discover them by their network tag.
  # The edge metrics listener is VPC-only; public forwarding rules never expose
  # 9091.
  #
  # gce_sd_config is ZONE-scoped: `zone` is REQUIRED and holds exactly one value,
  # so a REGIONAL MIG needs one entry per zone in the region — an edge instance
  # can land in any of them. control-install.sh renders EDGE_GCE_SD as that
  # per-zone list from the region's live zone list.
  #
  # Omitting `zone` is NOT a soft failure that just yields no edge targets:
  # Prometheus rejects the WHOLE FILE with "GCE SD configuration requires a
  # zone" and exits 2, so the service crash-loops and every unrelated dashboard
  # goes blank. That shipped in 24ee226 and silently held the fleet's entire
  # metrics stack down for three days — nothing scraped, Grafana reading "No
  # data" fleet-wide — because `systemctl restart` returns 0 for a Type=simple
  # unit as soon as the process forks, long before it exits. `promtool check
  # config` now gates the deploy so a bad render can never reach a restart.
  - job_name: sandbox-edge
    metrics_path: /metrics
    gce_sd_configs:
${EDGE_GCE_SD}
    relabel_configs:
      - source_labels: [__meta_gce_instance_name]
        target_label: instance_name
