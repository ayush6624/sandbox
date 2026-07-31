#!/usr/bin/env bash
set -euxo pipefail
exec > >(tee -a /var/log/startup-script.log) 2>&1

meta() {
  curl -fsS -H "Metadata-Flavor: Google" \
    "http://metadata.google.internal/computeMetadata/v1/instance/attributes/$1"
}
token() {
  curl -fsS -H "Metadata-Flavor: Google" \
    "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])'
}
secret() {
  local name="$1" out="$2" tok
  tok="$(token)"
  curl -fsS -H "Authorization: Bearer $tok" \
    "https://secretmanager.googleapis.com/v1/projects/$(meta project)/secrets/${name}/versions/latest:access" |
    python3 -c 'import base64,json,sys; sys.stdout.buffer.write(base64.b64decode(json.load(sys.stdin)["payload"]["data"]))' \
    > "$out"
}

id sandbox-edge >/dev/null 2>&1 || useradd --system --home /var/lib/sandbox-edge --shell /usr/sbin/nologin sandbox-edge
install -d -m 0750 -o root -g sandbox-edge /var/lib/sandbox-edge/tls /etc/sandbox-edge

tok="$(token)"
curl -fsSL -H "Authorization: Bearer $tok" \
  "https://storage.googleapis.com/storage/v1/b/$(meta release-bucket)/o/releases%2F$(meta release-sha)%2Fsandbox-edge?alt=media" \
  -o /usr/local/bin/sandbox-edge
chmod 0755 /usr/local/bin/sandbox-edge

cat >/usr/local/sbin/refresh-sandbox-edge-secrets <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
meta() { curl -fsS -H "Metadata-Flavor: Google" "http://metadata.google.internal/computeMetadata/v1/instance/attributes/$1"; }
token() { curl -fsS -H "Metadata-Flavor: Google" "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])'; }
fetch() {
  local name="$1" out="$2" tok
  tok="$(token)"
  curl -fsS -H "Authorization: Bearer $tok" \
    "https://secretmanager.googleapis.com/v1/projects/$(meta project)/secrets/${name}/versions/latest:access" |
    python3 -c 'import base64,json,sys; sys.stdout.buffer.write(base64.b64decode(json.load(sys.stdin)["payload"]["data"]))' >"$out"
}
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
fetch "$(meta cert-secret)" "$work/fullchain.pem"
fetch "$(meta key-secret)" "$work/privkey.pem"
fetch "$(meta gateway-token-secret)" "$work/gateway-token"
openssl x509 -in "$work/fullchain.pem" -noout -checkend 86400
openssl pkey -in "$work/privkey.pem" -pubout -outform DER 2>/dev/null | sha256sum >"$work/key.pub"
openssl x509 -in "$work/fullchain.pem" -pubkey -noout | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum >"$work/cert.pub"
cmp -s "$work/key.pub" "$work/cert.pub"
token_changed=0
if [ -f /etc/sandbox-edge/gateway-token ] && ! cmp -s "$work/gateway-token" /etc/sandbox-edge/gateway-token; then
  token_changed=1
fi
for file in fullchain.pem privkey.pem gateway-token; do
  chown root:sandbox-edge "$work/$file"
  chmod 0440 "$work/$file"
done
mv -f "$work/fullchain.pem" /var/lib/sandbox-edge/tls/fullchain.pem
mv -f "$work/privkey.pem" /var/lib/sandbox-edge/tls/privkey.pem
mv -f "$work/gateway-token" /etc/sandbox-edge/gateway-token
# Certificates reload in-process. A token is read at startup, so rotate it by
# restarting only after the replacement file is installed.
if [ "$token_changed" = 1 ] && systemctl is-active --quiet sandbox-edge.service; then
  systemctl restart sandbox-edge.service
fi
SCRIPT
chmod 0755 /usr/local/sbin/refresh-sandbox-edge-secrets
/usr/local/sbin/refresh-sandbox-edge-secrets

cat >/usr/local/sbin/run-sandbox-edge <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
exec /usr/local/bin/sandbox-edge \
  --listen=:443 --http-listen=:80 --metrics-listen=0.0.0.0:9091 \
  --domain="$(meta edge-domain)" \
  --cert-file=/var/lib/sandbox-edge/tls/fullchain.pem \
  --key-file=/var/lib/sandbox-edge/tls/privkey.pem \
  --gateway="http://$(meta gateway-addr)" \
  --gateway-token="$(cat /etc/sandbox-edge/gateway-token)" \
  --raw-listen-ip=0.0.0.0 \
  --raw-port-min="$(meta raw-port-min)" --raw-port-max="$(meta raw-port-max)" \
  --drain-timeout=5m
SCRIPT
# run script needs the metadata helper.
sed -i '2a meta() { curl -fsS -H "Metadata-Flavor: Google" "http://metadata.google.internal/computeMetadata/v1/instance/attributes/$1"; }' /usr/local/sbin/run-sandbox-edge
chmod 0755 /usr/local/sbin/run-sandbox-edge

cat >/etc/systemd/system/sandbox-edge.service <<'UNIT'
[Unit]
Description=Sandbox public ingress edge
After=network-online.target
Wants=network-online.target
[Service]
User=sandbox-edge
Group=sandbox-edge
ExecStart=/usr/local/sbin/run-sandbox-edge
Restart=always
RestartSec=2
LimitNOFILE=1048576
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
TimeoutStopSec=330
[Install]
WantedBy=multi-user.target
UNIT
cat >/etc/systemd/system/sandbox-edge-secrets.service <<'UNIT'
[Unit]
Description=Refresh sandbox edge certificate and gateway token
[Service]
Type=oneshot
ExecStart=/usr/local/sbin/refresh-sandbox-edge-secrets
UNIT
cat >/etc/systemd/system/sandbox-edge-secrets.timer <<'UNIT'
[Unit]
Description=Refresh sandbox edge secrets periodically
[Timer]
OnBootSec=2m
OnUnitActiveSec=5m
RandomizedDelaySec=30s
[Install]
WantedBy=timers.target
UNIT
systemctl daemon-reload
systemctl enable --now sandbox-edge sandbox-edge-secrets.timer
