#!/usr/bin/env bash
# Runs as root ON the control VM (piped in by control.sh deploy). Installs and
# starts the four control-plane services from the rsync'd assets under
# $REMOTE_DIR. Idempotent. Expects env: GW_TOKEN HOST_TOKEN CONTROL_IP GW_PORT
# PROM_PORT PROM_VERSION NOMAD_VERSION AUTOSCALER_VERSION SLOTS_PER_HOST
# HEADROOM_SLOTS SCALE_DOWN_WINDOW PROJECT ZONE MIG_NAME MIG_MIN MIG_MAX
# QUEUE_WAIT QUEUE_MAX REMOTE_DIR GRAFANA_VERSION GRAFANA_PORT
# GRAFANA_ADMIN_PASSWORD
set -euo pipefail

need() { command -v "$1" >/dev/null || DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$1"; }
apt-get update -qq || true
need unzip; need curl; need gettext-base   # gettext-base provides envsubst

fetch_unzip() { # url dest-binary
  local url="$1" dst="$2" tmp; tmp="$(mktemp -d)"
  curl -fsSL -o "$tmp/a.zip" "$url"; unzip -o "$tmp/a.zip" -d "$tmp" >/dev/null
  install -m 0755 "$tmp/$(basename "$dst")" "$dst"; rm -rf "$tmp"
}

# --- 1. Nomad server ---
command -v nomad >/dev/null || \
  fetch_unzip "https://releases.hashicorp.com/nomad/${NOMAD_VERSION}/nomad_${NOMAD_VERSION}_linux_amd64.zip" /usr/local/bin/nomad
mkdir -p /etc/nomad.d /opt/nomad/data
cat >/etc/nomad.d/server.hcl <<HCL
datacenter = "dc1"
data_dir   = "/opt/nomad/data"
bind_addr  = "0.0.0.0"
advertise { http = "${CONTROL_IP}" rpc = "${CONTROL_IP}" serf = "${CONTROL_IP}" }
server { enabled = true bootstrap_expect = 1 }
HCL
cat >/etc/systemd/system/nomad-server.service <<UNIT
[Unit]
Description=Nomad server
After=network-online.target
Wants=network-online.target
[Service]
ExecStart=/usr/local/bin/nomad agent -config /etc/nomad.d/server.hcl
Restart=always
RestartSec=2
LimitNOFILE=65536
[Install]
WantedBy=multi-user.target
UNIT

# --- 2. sandbox gateway ---
install -m 0755 "${REMOTE_DIR}/sandbox" /usr/local/bin/sandbox
cat >/etc/systemd/system/sandbox-gateway.service <<UNIT
[Unit]
Description=sandbox multi-host gateway (control plane)
After=network-online.target
Wants=network-online.target
[Service]
ExecStart=/usr/local/bin/sandbox gateway --listen 0.0.0.0:${GW_PORT} --token ${GW_TOKEN} \
  --queue-wait ${QUEUE_WAIT:-180s} --queue-max ${QUEUE_MAX:-4096}
Restart=always
RestartSec=2
LimitNOFILE=1048576
[Install]
WantedBy=multi-user.target
UNIT

# --- 3. Prometheus ---
if [ ! -x /usr/local/bin/prometheus ]; then
  tmp="$(mktemp -d)"
  curl -fsSL -o "$tmp/p.tgz" "https://github.com/prometheus/prometheus/releases/download/v${PROM_VERSION}/prometheus-${PROM_VERSION}.linux-amd64.tar.gz"
  tar xzf "$tmp/p.tgz" -C "$tmp" --strip-components=1
  install -m 0755 "$tmp/prometheus" /usr/local/bin/prometheus
  rm -rf "$tmp"
fi
mkdir -p /etc/prometheus /var/lib/prometheus
GATEWAY_TOKEN="$GW_TOKEN" GW_PORT="$GW_PORT" \
  envsubst < "${REMOTE_DIR}/prometheus/prometheus.yml.tpl" > /etc/prometheus/prometheus.yml
SLOTS_PER_HOST="$SLOTS_PER_HOST" HEADROOM_SLOTS="$HEADROOM_SLOTS" \
  envsubst < "${REMOTE_DIR}/prometheus/rules.yml.tpl" > /etc/prometheus/rules.yml
cat >/etc/systemd/system/prometheus.service <<UNIT
[Unit]
Description=Prometheus
After=network-online.target
Wants=network-online.target
[Service]
ExecStart=/usr/local/bin/prometheus --config.file=/etc/prometheus/prometheus.yml \
  --storage.tsdb.path=/var/lib/prometheus --web.listen-address=0.0.0.0:${PROM_PORT}
Restart=always
RestartSec=2
[Install]
WantedBy=multi-user.target
UNIT

# --- 4. Nomad autoscaler ---
# VERSION-AWARE (was `command -v nomad-autoscaler ||`, which never re-fetched, so
# bumping AUTOSCALER_VERSION silently left the old binary installed forever).
# Compares the running binary's reported version and re-fetches on mismatch,
# mirroring the Grafana block below.
installed_autoscaler_version() {
  /usr/local/bin/nomad-autoscaler --version 2>/dev/null |
    grep -oE 'v?[0-9]+\.[0-9]+\.[0-9]+' | head -1 | tr -d v
}
have_autoscaler="$(installed_autoscaler_version)"
if [ "$have_autoscaler" != "$AUTOSCALER_VERSION" ]; then
  echo ">> installing nomad-autoscaler ${AUTOSCALER_VERSION} (was ${have_autoscaler:-none})"
  fetch_unzip "https://releases.hashicorp.com/nomad-autoscaler/${AUTOSCALER_VERSION}/nomad-autoscaler_${AUTOSCALER_VERSION}_linux_amd64.zip" /usr/local/bin/nomad-autoscaler
fi

# retry_attempts in the gce-mig target block needs >= 0.4.8; older builds ignore
# it and keep the hard-coded 15 attempts (150s of post-action scale-up blackout).
# Warn loudly rather than fail — an old pin still autoscales, just slower to
# react to a second burst wave.
case "$AUTOSCALER_VERSION" in
  0.4.[0-7]|0.[0-3].*) echo "WARNING: autoscaler ${AUTOSCALER_VERSION} < 0.4.8 ignores retry_attempts; scale-up blackout stays at 150s per action" >&2 ;;
esac

mkdir -p /etc/nomad-autoscaler/policies
PROM_PORT="$PROM_PORT" envsubst < "${REMOTE_DIR}/nomad/autoscaler.hcl.tpl" > /etc/nomad-autoscaler/autoscaler.hcl
PROJECT="$PROJECT" ZONE="$ZONE" MIG_NAME="$MIG_NAME" MIG_MIN="$MIG_MIN" MIG_MAX="$MIG_MAX" \
  SCALE_DOWN_WINDOW="$SCALE_DOWN_WINDOW" \
  AUTOSCALER_RETRY_ATTEMPTS="${AUTOSCALER_RETRY_ATTEMPTS:-3}" \
  envsubst < "${REMOTE_DIR}/nomad/policies/workers.hcl.tpl" > /etc/nomad-autoscaler/policies/workers.hcl
cat >/etc/systemd/system/nomad-autoscaler.service <<UNIT
[Unit]
Description=Nomad Autoscaler
After=network-online.target nomad-server.service
Wants=network-online.target
[Service]
ExecStart=/usr/local/bin/nomad-autoscaler agent -config /etc/nomad-autoscaler/autoscaler.hcl
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
UNIT

# --- 5. Grafana ---
# Runs alongside Prometheus on the control VM; view over the tailnet at
# http://<control-tailnet-ip>:${GRAFANA_PORT}. Anonymous viewing is on (private
# tailnet, internal tool); the admin login still exists for editing.
GRAFANA_VERSION="${GRAFANA_VERSION:-11.1.0}"
GRAFANA_PORT="${GRAFANA_PORT:-3000}"
if [ ! -f "/opt/grafana/VERSION" ] || [ "$(cat /opt/grafana/VERSION 2>/dev/null)" != "$GRAFANA_VERSION" ]; then
  tmp="$(mktemp -d)"
  curl -fsSL -o "$tmp/g.tgz" "https://dl.grafana.com/oss/release/grafana-${GRAFANA_VERSION}.linux-amd64.tar.gz"
  rm -rf /opt/grafana && mkdir -p /opt/grafana
  tar xzf "$tmp/g.tgz" -C /opt/grafana --strip-components=1
  echo "$GRAFANA_VERSION" > /opt/grafana/VERSION
  rm -rf "$tmp"
fi
# Provisioning: datasource (envsubst PROM_PORT), dashboard provider, dashboards.
mkdir -p /etc/grafana/provisioning/datasources /etc/grafana/provisioning/dashboards /var/lib/grafana/dashboards
PROM_PORT="$PROM_PORT" envsubst < "${REMOTE_DIR}/grafana/provisioning/datasources/datasource.yml" \
  > /etc/grafana/provisioning/datasources/datasource.yml
install -m 0644 "${REMOTE_DIR}/grafana/provisioning/dashboards/provider.yml" /etc/grafana/provisioning/dashboards/provider.yml
install -m 0644 "${REMOTE_DIR}/grafana/dashboards/"*.json /var/lib/grafana/dashboards/
cat >/etc/systemd/system/grafana.service <<UNIT
[Unit]
Description=Grafana
After=network-online.target prometheus.service
Wants=network-online.target
[Service]
ExecStart=/opt/grafana/bin/grafana server --homepath /opt/grafana
Environment=GF_PATHS_PROVISIONING=/etc/grafana/provisioning
Environment=GF_PATHS_DATA=/var/lib/grafana
Environment=GF_SERVER_HTTP_PORT=${GRAFANA_PORT}
Environment=GF_SECURITY_ADMIN_PASSWORD=${GRAFANA_ADMIN_PASSWORD:-sandbox}
Environment=GF_AUTH_ANONYMOUS_ENABLED=true
Environment=GF_AUTH_ANONYMOUS_ORG_ROLE=Admin
Environment=GF_ANALYTICS_REPORTING_ENABLED=false
Environment=GF_LOG_MODE=console
Restart=always
RestartSec=2
[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable nomad-server sandbox-gateway prometheus nomad-autoscaler grafana
# restart (not enable --now): a redeploy must pick up new binaries/config on
# already-running services. Gateway routes rebuild from heartbeats in <=5s.
systemctl restart nomad-server sandbox-gateway prometheus nomad-autoscaler grafana
echo ">> control-install done"
