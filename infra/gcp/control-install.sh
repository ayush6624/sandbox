#!/usr/bin/env bash
# Runs as root ON the control VM (piped in by control.sh deploy). Installs and
# starts the four control-plane services from the rsync'd assets under
# $REMOTE_DIR. Idempotent. Expects env: GW_TOKEN GATEWAY_CONTROL_TOKEN HOST_TOKEN CONTROL_IP GW_PORT
# GATEWAY_EDGE_TOKEN (optional: gates /route + /raw-route for the ingress edge)
# GW_TOKEN_PREV / GATEWAY_EDGE_TOKEN_PREV (optional: written as a second line of
#   the matching token file so an overlap rotation survives a deploy)
# PROM_PORT PROM_VERSION NOMAD_VERSION AUTOSCALER_VERSION SANDBOX_RELEASE SLOTS_PER_HOST
# HEADROOM_SLOTS SCALE_DOWN_WINDOW PROJECT ZONE MIG_NAME MIG_MIN MIG_MAX
# QUEUE_WAIT QUEUE_MAX INGRESS_BUCKET RAW_PUBLIC_HOST RAW_PORT_MIN RAW_PORT_MAX
# REMOTE_DIR GRAFANA_VERSION GRAFANA_PORT
# GRAFANA_ADMIN_PASSWORD
#
# SECTIONS=gateway installs ONLY the gateway (binary + tokens + unit + restart)
# and exits. That is the whole cost of shipping a new `sandbox` build to the
# control plane; the rest — nomad server, prometheus, autoscaler, grafana — is
# pinned by version and unchanged by a code deploy, so reinstalling and
# restarting it on every rollout is pure latency. Used by `control.sh gateway`
# and `rollout.sh --fast`. Default (unset/all) installs everything.
set -euo pipefail

SECTIONS="${SECTIONS:-all}"

need() { command -v "$1" >/dev/null || DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$1"; }
if [ "$SECTIONS" = all ]; then
  apt-get update -qq || true
  need unzip; need curl; need gettext-base   # gettext-base provides envsubst
fi

fetch_unzip() { # url dest-binary
  local url="$1" dst="$2" tmp; tmp="$(mktemp -d)"
  curl -fsSL -o "$tmp/a.zip" "$url"; unzip -o "$tmp/a.zip" -d "$tmp" >/dev/null
  install -m 0755 "$tmp/$(basename "$dst")" "$dst"; rm -rf "$tmp"
}

# --- 1. Nomad server ---
if [ "$SECTIONS" = all ]; then
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
fi

# --- 2. sandbox gateway --- (always: this is the only part a code deploy changes)
install -m 0755 "${REMOTE_DIR}/sandbox" /usr/local/bin/sandbox
install -d -m 0700 /etc/sandbox-gateway
# write_tokens <file> <primary> [predecessor]
# A token file holds one credential per line: the first is used for outbound
# calls, every line is accepted inbound. The optional second line is what makes
# an overlap rotation survive a deploy — WITHOUT it, every rollout resets the
# file to a single token and 401s whichever side has already moved, which is why
# a rotation must be expressible through these scripts and not only by hand.
write_tokens() {
  local file="$1" primary="$2" prev="${3:-}" content="$2"
  if [ -n "$prev" ] && [ "$prev" != "$primary" ]; then
    content="$primary
$prev"
  fi
  # umask in a subshell: the gateway REFUSES a group/world-readable credential
  # file, and leaking global umask into the rest of this script would silently
  # re-mode the prometheus/grafana assets written further down.
  ( umask 077; printf '%s\n' "$content" > "$file" )
  chmod 0600 "$file"
}
write_tokens /etc/sandbox-gateway/client.tokens "$GW_TOKEN" "${GW_TOKEN_PREV:-}"
write_tokens /etc/sandbox-gateway/worker-control.tokens "$GATEWAY_CONTROL_TOKEN"
# Third trust domain: the public ingress edge. GET /route and GET /raw-route
# return a worker's control token, so they must not be reachable with the client
# credential (which IS the users' SANDBOX_API_KEY). Optional so an older
# control.sh that doesn't export it still installs a working gateway — the
# gateway then falls back to the client credential there and warns at startup.
# GATEWAY_EDGE_TOKEN_PREV holds the outgoing edge credential while the edge MIG
# rolls onto the new one; the gateway accepts both, so the roll costs no ingress.
EDGE_ARGS=""
if [ -n "${GATEWAY_EDGE_TOKEN:-}" ]; then
  write_tokens /etc/sandbox-gateway/edge.tokens \
    "$GATEWAY_EDGE_TOKEN" "${GATEWAY_EDGE_TOKEN_PREV:-}"
  EDGE_ARGS="--edge-token-file /etc/sandbox-gateway/edge.tokens"
fi
# Durable raw TCP allocation (E4) is opt-in: no INGRESS_BUCKET, no raw flags.
RAW_ARGS=""
if [ -n "${INGRESS_BUCKET:-}" ]; then
  RAW_ARGS="--ingress-bucket ${INGRESS_BUCKET} --raw-public-host ${RAW_PUBLIC_HOST:?} --raw-port-min ${RAW_PORT_MIN:-20000} --raw-port-max ${RAW_PORT_MAX:-29999}"
fi
cat >/etc/systemd/system/sandbox-gateway.service <<UNIT
[Unit]
Description=sandbox multi-host gateway (control plane)
After=network-online.target
Wants=network-online.target
[Service]
StateDirectory=sandbox-gateway
Environment=SANDBOX_RELEASE=${SANDBOX_RELEASE:-unknown}
ExecStart=/usr/local/bin/sandbox gateway --listen ${CONTROL_IP}:${GW_PORT} \
  --management-transport private_proxy \
  --token-file /etc/sandbox-gateway/client.tokens \
  --worker-token-file /etc/sandbox-gateway/worker-control.tokens ${EDGE_ARGS} \
  --queue-wait ${QUEUE_WAIT:-240s} --queue-max ${QUEUE_MAX:-4096} \
  --worker-release-file /var/lib/sandbox-gateway/worker-release \
  --direct-scale-project ${PROJECT} \
  --direct-scale-zone ${ZONE} \
  --direct-scale-mig ${MIG_NAME} \
  --direct-scale-max ${MIG_MAX} \
  --direct-scale-slots-per-host ${SLOTS_PER_HOST} \
  --direct-scale-headroom ${HEADROOM_SLOTS:-0} ${RAW_ARGS}
# Single-writer invariant: the GATEWAY is the sole process allowed to GROW the
# production MIG, and the Nomad Autoscaler policy is capped to scale-IN only
# (see nomad/policies/workers.hcl.tpl and the sandbox:workers_scale_in_ceiling
# recording rule). Two independent writers that both grow can ratchet the target
# far above demand, so do not remove that cap while these flags are set.
# The gateway owns scale-out because its queue-triggered path decides in ~1s
# where the Prometheus/autoscaler loop needs ~10s — and ~189s when the request
# lands inside the gce-mig confirmation blackout. Measured: that difference is
# ~10s of held-burst p95.
Restart=always
RestartSec=2
LimitNOFILE=1048576
[Install]
WantedBy=multi-user.target
UNIT

# Gateway-only deploy stops here: sections 3-5 are version-pinned services that
# a code rollout cannot change. The gateway holds no durable state (it rebuilds
# its routing table from host heartbeats), so restarting it costs one heartbeat
# interval of routing, not correctness.
if [ "$SECTIONS" = gateway ]; then
  systemctl daemon-reload
  systemctl enable --now sandbox-gateway >/dev/null 2>&1 || true
  systemctl restart sandbox-gateway
  echo ">> gateway-only install done (release ${SANDBOX_RELEASE:-unknown})"
  exit 0
fi

# --- 3. Prometheus ---
# promtool ships in the same tarball and gates the rendered config below, so a
# host that already has prometheus but not promtool must still re-fetch.
if [ ! -x /usr/local/bin/prometheus ] || [ ! -x /usr/local/bin/promtool ]; then
  tmp="$(mktemp -d)"
  curl -fsSL -o "$tmp/p.tgz" "https://github.com/prometheus/prometheus/releases/download/v${PROM_VERSION}/prometheus-${PROM_VERSION}.linux-amd64.tar.gz"
  tar xzf "$tmp/p.tgz" -C "$tmp" --strip-components=1
  install -m 0755 "$tmp/prometheus" /usr/local/bin/prometheus
  install -m 0755 "$tmp/promtool" /usr/local/bin/promtool
  rm -rf "$tmp"
fi
mkdir -p /etc/prometheus /var/lib/prometheus

# The edge job discovers a REGIONAL MIG, but gce_sd_config is zone-scoped
# (`zone` is required and single-valued), so it needs one entry per zone in the
# region. Enumerate the region's LIVE zone list rather than assuming the -a/-b/-c
# suffixes, because regions disagree — us-central1 has an -f and no -d. gcloud
# here runs on the control VM against the metadata server, so it never prompts.
# No pipelines: this script runs under `set -o pipefail`, where `| head` can
# SIGPIPE the producer and abort the install with no error (see the autoscaler
# note below).
#
# The filter uses `:` (the "has" operator), NOT `=`. `tags` is a REPEATED field
# and the GCE list-filter grammar rejects `=` against one: every `=` spelling
# ('tags.items = sandbox-edge', quoted, or parenthesized) returns
# 400 "Invalid list filter expression", which is only a per-job discovery error,
# so the edge job silently discovers ZERO targets while Prometheus looks healthy.
# This is NOT gcloud's --filter language, which does accept `=` here — that
# difference is what made the original expression look correct when tested by
# hand. Verified against compute.googleapis.com directly.
edge_zone="${ZONE:-}"
if [ -z "$edge_zone" ]; then
  edge_zone="$(curl -sf -H 'Metadata-Flavor: Google' \
    http://metadata.google.internal/computeMetadata/v1/instance/zone 2>/dev/null)" || edge_zone=""
  edge_zone="${edge_zone##*/}"   # projects/<num>/zones/<zone> -> <zone>
fi
EDGE_REGION="${edge_zone%-*}"
edge_zones=""
if [ -n "$EDGE_REGION" ]; then
  edge_zones="$(gcloud compute zones list --project="$PROJECT" \
    --filter="region:${EDGE_REGION}" --format='value(name)' 2>/dev/null)" || edge_zones=""
  if [ -z "$edge_zones" ]; then
    # No credentials or no network: fall back to the conventional suffixes. A
    # zone that does not exist is only a per-job DISCOVERY error (logged, job
    # shows unhealthy), never a fatal parse error — so the worst case is
    # possibly-missing edge targets, not a dead Prometheus.
    edge_zones="${EDGE_REGION}-a
${EDGE_REGION}-b
${EDGE_REGION}-c"
  fi
fi
EDGE_GCE_SD=""
while IFS= read -r z; do
  [ -n "$z" ] || continue
  EDGE_GCE_SD="${EDGE_GCE_SD}      - project: ${PROJECT}
        zone: ${z}
        port: 9091
        filter: 'tags.items:"sandbox-edge"'
"
done <<EOF
$edge_zones
EOF
if [ -z "$EDGE_GCE_SD" ]; then
  # Never emit an empty `gce_sd_configs:` list — that is the zone-less config's
  # failure mode all over again (fatal parse error, crash-loop). Drop the job.
  echo ">> WARNING: could not determine edge zones; sandbox-edge scrape job disabled" >&2
  EDGE_GCE_SD="      []"
fi
EDGE_GCE_SD="${EDGE_GCE_SD%$'\n'}"

GATEWAY_TOKEN="$GW_TOKEN" CONTROL_IP="$CONTROL_IP" GW_PORT="$GW_PORT" PROJECT="$PROJECT" \
  EDGE_GCE_SD="$EDGE_GCE_SD" \
  envsubst < "${REMOTE_DIR}/prometheus/prometheus.yml.tpl" > /etc/prometheus/prometheus.yml
SLOTS_PER_HOST="$SLOTS_PER_HOST" HEADROOM_SLOTS="$HEADROOM_SLOTS" \
  LEAD_SECONDS="${LEAD_SECONDS:-90}" \
  RAW_PORT_CAPACITY="$((${RAW_PORT_MAX:-29999} - ${RAW_PORT_MIN:-20000} + 1))" \
  envsubst < "${REMOTE_DIR}/prometheus/rules.yml.tpl" > /etc/prometheus/rules.yml

# Validate BEFORE the restart at the end of this script, and FAIL THE DEPLOY on a
# bad render. `systemctl restart` is not a check: for a Type=simple unit it
# returns 0 the moment the process forks, so a config prometheus rejects at
# startup exits 2 milliseconds later and Restart=always crash-loops it while the
# deploy reports success. A zone-less gce_sd_config did exactly that for three
# days (restart counter 66773) with Grafana blank fleet-wide. `check config` also
# validates the rule_files it references, so rules.yml is covered too.
promtool check config /etc/prometheus/prometheus.yml
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
# NB pipeline-free on purpose: this script runs under `set -euo pipefail`, where
# `... | grep | head -1` can SIGPIPE grep once head exits, making the whole
# command substitution exit 141 and aborting the install with NO error message.
# Bash regex matching has no such failure mode.
installed_autoscaler_version() {
  local out=""
  # 2>&1, NOT 2>/dev/null: nomad-autoscaler prints its version banner on STDERR,
  # so discarding stderr made this always report "none" and re-download on every
  # deploy — the exact staleness the version check exists to avoid.
  out="$(/usr/local/bin/nomad-autoscaler --version 2>&1)" || out=""
  if [[ $out =~ ([0-9]+\.[0-9]+\.[0-9]+) ]]; then
    printf '%s' "${BASH_REMATCH[1]}"
  fi
  return 0
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

# --- 6. Wildcard certificate renewal ---
# DNS-01 is required for the wildcard. The timer publishes new Secret Manager
# versions; edge replicas poll and hot-reload the pair without a fleet restart.
if [ -n "${EDGE_ACME_EMAIL:-}" ] && [ -n "${EDGE_DOMAIN:-}" ]; then
  if [ ! -x /usr/local/bin/lego ]; then
    tmp="$(mktemp -d)"
    curl -fsSL -o "$tmp/lego.tgz" \
      "https://github.com/go-acme/lego/releases/download/v${LEGO_VERSION:-4.21.0}/lego_v${LEGO_VERSION:-4.21.0}_linux_amd64.tar.gz"
    tar xzf "$tmp/lego.tgz" -C "$tmp"
    install -m 0755 "$tmp/lego" /usr/local/bin/lego
    rm -rf "$tmp"
  fi
  install -m 0755 "${REMOTE_DIR}/edge-cert-renew.sh" /usr/local/sbin/sandbox-edge-cert-renew
  cat >/etc/sandbox-edge-acme.env <<ENV
PROJECT=${PROJECT}
EDGE_DOMAIN=${EDGE_DOMAIN}
ACME_EMAIL=${EDGE_ACME_EMAIL}
DNS_PROVIDER=${EDGE_DNS_PROVIDER:-gcloud}
DNS_TOKEN_SECRET=${EDGE_DNS_TOKEN_SECRET:-sandbox-edge-dns-token}
CERT_SECRET=${EDGE_CERT_SECRET:-sandbox-edge-cert}
KEY_SECRET=${EDGE_KEY_SECRET:-sandbox-edge-key}
ENV
  chmod 0600 /etc/sandbox-edge-acme.env
  cat >/etc/systemd/system/sandbox-edge-cert-renew.service <<UNIT
[Unit]
Description=Renew sandbox wildcard certificate with ACME DNS-01
[Service]
Type=oneshot
ExecStart=/usr/local/sbin/sandbox-edge-cert-renew
UNIT
  cat >/etc/systemd/system/sandbox-edge-cert-renew.timer <<UNIT
[Unit]
Description=Daily sandbox wildcard certificate renewal check
[Timer]
OnBootSec=5m
OnCalendar=daily
RandomizedDelaySec=1h
Persistent=true
[Install]
WantedBy=timers.target
UNIT
fi

systemctl daemon-reload
systemctl enable nomad-server sandbox-gateway prometheus nomad-autoscaler grafana
if [ -f /etc/systemd/system/sandbox-edge-cert-renew.timer ]; then
  systemctl enable --now sandbox-edge-cert-renew.timer
fi
# restart (not enable --now): a redeploy must pick up new binaries/config on
# already-running services. Gateway routes rebuild from heartbeats in <=5s.
systemctl restart nomad-server sandbox-gateway prometheus nomad-autoscaler grafana

# `systemctl restart` proves nothing about a Type=simple unit: it returns 0 as
# soon as the process forks. A service that rejects its config and exits, or
# fails to bind, then crash-loops behind a green deploy — which is how a broken
# prometheus stayed broken for three days. So settle briefly and assert each unit
# is STILL up. Cheap, and it converts a silent control-plane outage into a failed
# deploy.
sleep 5
install_failed=""
for unit in nomad-server sandbox-gateway prometheus nomad-autoscaler grafana; do
  if ! systemctl is-active --quiet "$unit"; then
    echo "error: $unit is not active after restart" >&2
    systemctl --no-pager --lines=15 status "$unit" >&2 || true
    install_failed=1
  fi
done
[ -z "$install_failed" ] || exit 1
echo ">> control-install done"
