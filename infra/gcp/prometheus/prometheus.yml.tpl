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
