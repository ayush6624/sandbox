#!/usr/bin/env bash
# Runs on the control VM under systemd. lego uses the VM service account's ADC
# for Cloud DNS, then publishes validated PEMs as new Secret Manager versions.
set -euo pipefail
source /etc/sandbox-edge-acme.env

state=/var/lib/sandbox-edge-acme
install -d -m 0700 "$state"

access_token() {
  curl -fsS -H "Metadata-Flavor: Google" \
    http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])'
}
fetch_secret() {
  local secret="$1" out="$2" token
  token="$(access_token)"
  curl -fsS -H "Authorization: Bearer $token" \
    "https://secretmanager.googleapis.com/v1/projects/${PROJECT}/secrets/${secret}/versions/latest:access" |
    python3 -c 'import base64,json,sys; sys.stdout.buffer.write(base64.b64decode(json.load(sys.stdin)["payload"]["data"]))' >"$out"
  chmod 0600 "$out"
}

case "$DNS_PROVIDER" in
  gcloud)
    export GCE_PROJECT="$PROJECT"
    ;;
  cloudflare)
    fetch_secret "$DNS_TOKEN_SECRET" "$state/cloudflare-token"
    export CLOUDFLARE_DNS_API_TOKEN_FILE="$state/cloudflare-token"
    ;;
  *)
    echo "unsupported ACME DNS provider: $DNS_PROVIDER" >&2
    exit 2
    ;;
esac

args=(--accept-tos --email "$ACME_EMAIL" --dns "$DNS_PROVIDER" --path "$state"
  --domains "$EDGE_DOMAIN" --domains "*.${EDGE_DOMAIN}")
cert="$state/certificates/${EDGE_DOMAIN}.crt"
key="$state/certificates/${EDGE_DOMAIN}.key"
if [ -s "$cert" ] && [ -s "$key" ]; then
  /usr/local/bin/lego "${args[@]}" renew --days 30
else
  /usr/local/bin/lego "${args[@]}" run
fi
openssl x509 -in "$cert" -noout -checkend 86400
openssl pkey -in "$key" -pubout -outform DER 2>/dev/null | sha256sum >"$state/key.pub"
openssl x509 -in "$cert" -pubkey -noout |
  openssl pkey -pubin -outform DER 2>/dev/null | sha256sum >"$state/cert.pub"
cmp -s "$state/key.pub" "$state/cert.pub"

publish() {
  local secret="$1" file="$2" token payload
  token="$(access_token)"
  payload="$(base64 -w0 "$file")"
  curl -fsS -X POST -H "Authorization: Bearer $token" -H "Content-Type: application/json" \
    "https://secretmanager.googleapis.com/v1/projects/${PROJECT}/secrets/${secret}:addVersion" \
    --data "{\"payload\":{\"data\":\"${payload}\"}}" >/dev/null
}
publish "$CERT_SECRET" "$cert"
publish "$KEY_SECRET" "$key"
