#!/bin/bash
# M0 spike guest thaw-agent: poll Firecracker MMDS for this VM's identity and
# reconfigure eth0 whenever the generation token changes. Baked in as a systemd
# service before snapshot, so it's already running in guest memory on resume and
# picks up the clone's fresh identity with zero host contact on the old IP.
# (Production: this logic moves into sandboxd's resume hook.)
STATE=/run/thaw.gen
J() { python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('$1',''))"; }
ensure_mmds_route() { ip route show 169.254.169.254 2>/dev/null | grep -q eth0 || ip route add 169.254.169.254/32 dev eth0 2>/dev/null; }
# MMDS V2: every request needs a session token, and GET must ask for JSON.
mmds_get() { local t; t=$(curl -s -m1 -X PUT "http://169.254.169.254/latest/api/token" -H "X-metadata-token-ttl-seconds: 60") || return 1
  curl -s -m1 -H "X-metadata-token: $t" -H "Accept: application/json" "http://169.254.169.254/"; }

while true; do
  ensure_mmds_route
  DOC=$(mmds_get 2>/dev/null) || { sleep 0.2; continue; }
  GEN=$(printf '%s' "$DOC" | J gen); [ -z "$GEN" ] && { sleep 0.2; continue; }
  LAST=$(cat "$STATE" 2>/dev/null)
  if [ "$GEN" != "$LAST" ]; then
    IP=$(printf '%s' "$DOC" | J ip)
    GW=$(printf '%s' "$DOC" | J gw)
    MAC=$(printf '%s' "$DOC" | J mac)
    PFX=$(printf '%s' "$DOC" | J prefix); [ -z "$PFX" ] && PFX=24
    ip link set eth0 down
    ip addr flush dev eth0
    ip link set eth0 address "$MAC"
    ip link set eth0 up
    ip addr add "$IP/$PFX" dev eth0
    ip route replace default via "$GW"
    ensure_mmds_route
    echo "$GEN" > "$STATE"
    logger -t thaw "reconfigured eth0 -> ip=$IP mac=$MAC gw=$GW gen=$GEN"
  fi
  sleep 0.2
done
