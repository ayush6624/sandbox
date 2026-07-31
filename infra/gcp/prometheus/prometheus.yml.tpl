# Rendered by control-install.sh (envsubst):
# ${GATEWAY_TOKEN}, ${CONTROL_IP}, ${GW_PORT}, ${PROJECT}.
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
  - job_name: sandbox-edge
    metrics_path: /metrics
    gce_sd_configs:
      - project: ${PROJECT}
        port: 9091
        filter: 'tags.items = sandbox-edge'
    relabel_configs:
      - source_labels: [__meta_gce_instance_name]
        target_label: instance_name
