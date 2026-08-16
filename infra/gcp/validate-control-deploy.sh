#!/usr/bin/env bash
set -euo pipefail

# Gateway-only deploys are the fast rollout path. They must pass every setting
# that changes the generated gateway unit, or a normal code rollout can
# silently remove a production feature even though the binary still supports
# it, or make the installer fail under set -u. Raw ingress was disabled this
# way when cmd_gateway omitted the settings
# that cmd_deploy already supplied. GATEWAY_EDGE_TOKEN is in the list for the
# same reason and is worse than a missing feature: dropping it makes the gateway
# fall back to accepting the CLIENT credential on /route and /raw-route, which
# hand out worker control tokens — a security regression a --fast rollout would
# otherwise apply silently. The *_PREV pair is in the list because a rollout
# during a credential rotation would otherwise rewrite the token file back to a
# single line, 401ing whichever side of the rotation had already moved.

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTROL="$DIR/control.sh"

fail() {
  echo "error: $1" >&2
  exit 1
}

gateway_fn="$(sed -n '/^cmd_gateway()/,/^}/p' "$CONTROL")"
for assignment in \
  "INGRESS_BUCKET='\${INGRESS_BUCKET:-}'" \
  "RAW_PUBLIC_HOST='\${EDGE_DOMAIN:-}'" \
  "RAW_PORT_MIN='\${EDGE_RAW_PORT_MIN:-20000}'" \
  "RAW_PORT_MAX='\${EDGE_RAW_PORT_MAX:-29999}'" \
  "GATEWAY_EDGE_TOKEN='\$GATEWAY_EDGE_TOKEN'" \
  "GW_TOKEN_PREV='\${GATEWAY_TOKEN_PREV:-}'" \
  "GATEWAY_EDGE_TOKEN_PREV='\${GATEWAY_EDGE_TOKEN_PREV:-}'" \
  "MIG_MIN='\${MIG_MIN:-1}'" \
  "SCALE_IN_AFTER_SEC='\${SCALE_IN_AFTER_SEC:-600}'"
do
  grep -Fq "$assignment" <<<"$gateway_fn" ||
    fail "gateway-only deploy does not pass $assignment"
done

echo "PASS: gateway-only deploy preserves scaling, raw ingress, and edge-credential configuration"
