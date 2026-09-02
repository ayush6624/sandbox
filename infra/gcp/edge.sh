#!/usr/bin/env bash
# Regional HA public edge: MIG + external passthrough Network Load Balancer.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SANDBOX_GCP_CONFIG:-$DIR/config.env}"

REGION="${ZONE%-*}"
NAME="${EDGE_MIG_NAME:-sandbox-edge}"
SA_NAME="${EDGE_SA_NAME:-sandbox-edge-sa}"
SA_EMAIL="${SA_NAME}@${PROJECT}.iam.gserviceaccount.com"
IP_NAME="${EDGE_IP_NAME:-sandbox-edge-ip}"
HC="${NAME}-health"
LB_HC="${NAME}-lb-tcp-health"
BACKEND="${NAME}-backend"
RELEASE_BUCKET="${RELEASE_BUCKET:?set RELEASE_BUCKET}"
RELEASE_SHA="${EDGE_RELEASE_SHA:-$(git -C "$DIR/../.." rev-parse --short HEAD)}"
RAW_MIN="${EDGE_RAW_PORT_MIN:-20000}"
RAW_MAX="${EDGE_RAW_PORT_MAX:-29999}"
EDGE_DOMAIN="${EDGE_DOMAIN:?set EDGE_DOMAIN}"
CERT_SECRET="${EDGE_CERT_SECRET:-sandbox-edge-cert}"
KEY_SECRET="${EDGE_KEY_SECRET:-sandbox-edge-key}"
TOKEN_SECRET="${EDGE_GATEWAY_TOKEN_SECRET:-sandbox-edge-gateway-token}"
CONTROL_SA="${CONTROL_SA_NAME:-sandbox-control-sa}@${PROJECT}.iam.gserviceaccount.com"
DNS_PROVIDER="${EDGE_DNS_PROVIDER:-gcloud}"
DNS_TOKEN_SECRET="${EDGE_DNS_TOKEN_SECRET:-sandbox-edge-dns-token}"
GC=(gcloud --project="$PROJECT")

ensure_secret() {
  "${GC[@]}" secrets describe "$1" >/dev/null 2>&1 || "${GC[@]}" secrets create "$1" --replication-policy=automatic
}
template_name() { echo "${NAME}-tpl-$(date +%Y%m%d-%H%M%S)"; }

create_template() {
  local tpl="$1"
  "${GC[@]}" compute instance-templates create "$tpl" \
    --machine-type="${EDGE_MACHINE_TYPE:-e2-standard-4}" \
    --image-family="${IMAGE_FAMILY:-ubuntu-2404-lts-amd64}" --image-project="${IMAGE_PROJECT:-ubuntu-os-cloud}" \
    --boot-disk-size=20GB --boot-disk-type=pd-balanced \
    --service-account="$SA_EMAIL" --scopes=cloud-platform \
    --tags=sandbox-edge \
    --metadata="project=${PROJECT},release-bucket=${RELEASE_BUCKET},release-sha=${RELEASE_SHA},edge-domain=${EDGE_DOMAIN},gateway-addr=${CONTROL_INTERNAL_IP}:${GW_PORT:-9090},cert-secret=${CERT_SECRET},key-secret=${KEY_SECRET},gateway-token-secret=${TOKEN_SECRET},raw-port-min=${RAW_MIN},raw-port-max=${RAW_MAX}" \
    --metadata-from-file=startup-script="$DIR/startup-edge.sh"
}

cmd_init() {
  # Secret Manager carries the cert/key/gateway-token to edge VMs, so init
  # cannot proceed on a project where the API was never enabled. Enabling is
  # idempotent and cheap; discovering it half-way through leaves a partially
  # provisioned edge.
  "${GC[@]}" services enable secretmanager.googleapis.com >/dev/null
  "${GC[@]}" iam service-accounts describe "$SA_EMAIL" >/dev/null 2>&1 || \
    "${GC[@]}" iam service-accounts create "$SA_NAME" --display-name="Sandbox public ingress edge"
  ensure_secret "$CERT_SECRET"; ensure_secret "$KEY_SECRET"; ensure_secret "$TOKEN_SECRET"
  for secret in "$CERT_SECRET" "$KEY_SECRET" "$TOKEN_SECRET"; do
    "${GC[@]}" secrets add-iam-policy-binding "$secret" \
      --member="serviceAccount:${SA_EMAIL}" --role=roles/secretmanager.secretAccessor >/dev/null
  done
  for secret in "$CERT_SECRET" "$KEY_SECRET"; do
    "${GC[@]}" secrets add-iam-policy-binding "$secret" \
      --member="serviceAccount:${CONTROL_SA}" --role=roles/secretmanager.secretVersionAdder >/dev/null
  done
  if [ -n "${EDGE_ACME_EMAIL:-}" ] && [ "$DNS_PROVIDER" = gcloud ]; then
    "${GC[@]}" projects add-iam-policy-binding "$PROJECT" \
      --member="serviceAccount:${CONTROL_SA}" --role=roles/dns.admin --condition=None >/dev/null
  fi
  if [ -n "${EDGE_ACME_EMAIL:-}" ] && [ "$DNS_PROVIDER" = cloudflare ]; then
    ensure_secret "$DNS_TOKEN_SECRET"
    "${GC[@]}" secrets add-iam-policy-binding "$DNS_TOKEN_SECRET" \
      --member="serviceAccount:${CONTROL_SA}" --role=roles/secretmanager.secretAccessor >/dev/null
    if [ -n "${EDGE_DNS_TOKEN_FILE:-}" ]; then
      "${GC[@]}" secrets versions add "$DNS_TOKEN_SECRET" --data-file="$EDGE_DNS_TOKEN_FILE"
    fi
  fi
  "${GC[@]}" storage buckets add-iam-policy-binding "gs://${RELEASE_BUCKET}" \
    --member="serviceAccount:${SA_EMAIL}" --role=roles/storage.objectViewer >/dev/null
  fleet_secrets="${FLEET_SECRETS_FILE:-$DIR/fleet-secrets.env}"
  if [ -f "$fleet_secrets" ]; then
    source "$fleet_secrets"
    # The edge gets the EDGE credential, not the client one. GET /route and
    # GET /raw-route return a worker's control token, and GATEWAY_TOKEN is the
    # same token users hold as SANDBOX_API_KEY — shipping it here is what let any
    # API-key holder reach every sandbox on a worker directly. Falls back to
    # GATEWAY_TOKEN only for a fleet whose control.sh predates GATEWAY_EDGE_TOKEN
    # (the gateway then still accepts it, and warns at startup).
    printf '%s' "${GATEWAY_EDGE_TOKEN:-${GATEWAY_TOKEN:?}}" \
      | "${GC[@]}" secrets versions add "$TOKEN_SECRET" --data-file=-
  fi
  [ -n "${EDGE_CERT_FILE:-}" ] && "${GC[@]}" secrets versions add "$CERT_SECRET" --data-file="$EDGE_CERT_FILE"
  [ -n "${EDGE_KEY_FILE:-}" ] && "${GC[@]}" secrets versions add "$KEY_SECRET" --data-file="$EDGE_KEY_FILE"
  if [ -n "${EDGE_ACME_EMAIL:-}" ]; then
    "${GC[@]}" compute ssh "${CONTROL_NAME:-sandbox-control}" --zone="$ZONE" \
      --command="sudo systemctl start sandbox-edge-cert-renew.service"
  fi
  "${GC[@]}" compute addresses describe "$IP_NAME" --region="$REGION" >/dev/null 2>&1 || \
    "${GC[@]}" compute addresses create "$IP_NAME" --region="$REGION"
  "${GC[@]}" compute firewall-rules describe "${NAME}-public" >/dev/null 2>&1 || \
    "${GC[@]}" compute firewall-rules create "${NAME}-public" --target-tags=sandbox-edge \
      --source-ranges=0.0.0.0/0 --allow="tcp:80,tcp:443,tcp:${RAW_MIN}-${RAW_MAX}"
  "${GC[@]}" compute firewall-rules describe "${NAME}-health" >/dev/null 2>&1 || \
    "${GC[@]}" compute firewall-rules create "${NAME}-health" --target-tags=sandbox-edge \
      --source-ranges=35.191.0.0/16,130.211.0.0/22 --allow=tcp:9091
  "${GC[@]}" compute firewall-rules describe "${NAME}-metrics" >/dev/null 2>&1 || \
    "${GC[@]}" compute firewall-rules create "${NAME}-metrics" --target-tags=sandbox-edge \
      --source-ranges="${VPC_SUBNET_CIDR:-10.160.0.0/20}" --allow=tcp:9091
}

# Every step is guarded so a partially applied `up` can be re-run to completion
# rather than aborting on the first "already exists" — the failure mode that
# leaves an edge with no forwarding rules.
cmd_up() {
  if ! "${GC[@]}" compute instance-groups managed describe "$NAME" --region="$REGION" >/dev/null 2>&1; then
    local tpl; tpl="$(template_name)"; create_template "$tpl"
    "${GC[@]}" compute instance-groups managed create "$NAME" --region="$REGION" \
      --template="$tpl" --size="${EDGE_MIN:-2}" --target-distribution-shape=EVEN
  fi
  "${GC[@]}" compute health-checks describe "$HC" --region="$REGION" >/dev/null 2>&1 || \
    "${GC[@]}" compute health-checks create http "$HC" --region="$REGION" --port=9091 --request-path=/healthz
  # Keep load-balancing eligibility separate from autohealing: a transient LB
  # probe failure should not replace a VM. The passthrough NLB's off-rule
  # :9091 probes did not reach the backends in production even though Google
  # documents fixed health ports as supported; checking the already-forwarded
  # TLS listener made backend health truthful without exposing another port.
  "${GC[@]}" compute health-checks describe "$LB_HC" --region="$REGION" >/dev/null 2>&1 || \
    "${GC[@]}" compute health-checks create tcp "$LB_HC" --region="$REGION" \
      --port=443 --enable-logging
  "${GC[@]}" compute backend-services describe "$BACKEND" --region="$REGION" >/dev/null 2>&1 || \
    "${GC[@]}" compute backend-services create "$BACKEND" --region="$REGION" \
      --load-balancing-scheme=EXTERNAL --protocol=TCP --health-checks="$LB_HC" \
      --health-checks-region="$REGION" --connection-draining-timeout=300
  # Converge an existing backend created before the dedicated LB check existed.
  "${GC[@]}" compute backend-services update "$BACKEND" --region="$REGION" \
    --health-checks="$LB_HC" --health-checks-region="$REGION" >/dev/null
  "${GC[@]}" compute backend-services describe "$BACKEND" --region="$REGION" \
    --format='value(backends[].group)' 2>/dev/null | grep -q "instanceGroups/${NAME}" || \
    "${GC[@]}" compute backend-services add-backend "$BACKEND" --region="$REGION" \
      --instance-group="$NAME" --instance-group-region="$REGION"
  # $HC is a REGIONAL health check, and this command has no --health-check-region
  # flag: a bare name resolves as global and fails with "does not exist", which
  # aborts `up` before the forwarding rules are created. Pass the full URI.
  "${GC[@]}" compute instance-groups managed update "$NAME" --region="$REGION" \
    --health-check="projects/${PROJECT}/regions/${REGION}/healthChecks/${HC}" \
    --initial-delay=120
  "${GC[@]}" compute forwarding-rules describe "${NAME}-web" --region="$REGION" >/dev/null 2>&1 || \
    "${GC[@]}" compute forwarding-rules create "${NAME}-web" --region="$REGION" \
      --load-balancing-scheme=EXTERNAL --address="$IP_NAME" --ip-protocol=TCP \
      --ports=80,443 --backend-service="$BACKEND"
  "${GC[@]}" compute forwarding-rules describe "${NAME}-raw" --region="$REGION" >/dev/null 2>&1 || \
    "${GC[@]}" compute forwarding-rules create "${NAME}-raw" --region="$REGION" \
      --load-balancing-scheme=EXTERNAL --address="$IP_NAME" --ip-protocol=TCP \
      --ports="${RAW_MIN}-${RAW_MAX}" --backend-service="$BACKEND"
  echo ">> edge up. Public IP: $("${GC[@]}" compute addresses describe "$IP_NAME" \
    --region="$REGION" --format='value(address)')"
}

cmd_roll() {
  local tpl; tpl="$(template_name)"; create_template "$tpl"
  "${GC[@]}" compute instance-groups managed set-instance-template "$NAME" --region="$REGION" --template="$tpl"
  "${GC[@]}" compute instance-groups managed rolling-action replace "$NAME" --region="$REGION" \
    --max-unavailable=0 --max-surge="${EDGE_MAX_SURGE:-3}"
}
cmd_status() {
  "${GC[@]}" compute addresses describe "$IP_NAME" --region="$REGION" --format='value(address)'
  "${GC[@]}" compute instance-groups managed list-instances "$NAME" --region="$REGION"
  "${GC[@]}" compute backend-services get-health "$BACKEND" --region="$REGION"
}
cmd_down() {
  "${GC[@]}" compute forwarding-rules delete "${NAME}-web" "${NAME}-raw" --region="$REGION" --quiet || true
  "${GC[@]}" compute backend-services delete "$BACKEND" --region="$REGION" --quiet || true
  "${GC[@]}" compute health-checks delete "$HC" "$LB_HC" --region="$REGION" --quiet || true
  "${GC[@]}" compute instance-groups managed delete "$NAME" --region="$REGION" --quiet || true
  echo "reserved IP, secrets, service account, and templates retained"
}
case "${1:-}" in
  init) cmd_init ;; up) cmd_up ;; roll) cmd_roll ;; status) cmd_status ;; down) cmd_down ;;
  *) echo "usage: $0 {init|up|roll|status|down}" >&2; exit 1 ;;
esac
