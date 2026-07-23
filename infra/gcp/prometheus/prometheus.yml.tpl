# Rendered by control-install.sh (envsubst): ${GATEWAY_TOKEN}, ${GW_PORT}.
# Prometheus scrapes the gateway's /metrics (behind bearer auth) on localhost —
# both run on the control VM.
global:
  scrape_interval: 10s
  evaluation_interval: 15s

rule_files:
  - /etc/prometheus/rules.yml

scrape_configs:
  - job_name: sandbox-gateway
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials: ${GATEWAY_TOKEN}
    static_configs:
      - targets: ["127.0.0.1:${GW_PORT}"]

  # Per-host detail, federated through the gateway: it scrapes each live
  # worker's /metrics (it holds their addr+token) and re-exports every series
  # with a host="<id>" label. This means Prometheus still scrapes only the
  # gateway on localhost — the dynamic Nomad worker fleet needs no service
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
      - targets: ["127.0.0.1:${GW_PORT}"]
