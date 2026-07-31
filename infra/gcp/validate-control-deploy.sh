#!/usr/bin/env bash
set -euo pipefail

# Gateway-only deploys are the fast rollout path. They must pass every setting
# that changes the generated gateway unit, or a normal code rollout can
# silently remove a production feature even though the binary still supports
# it. Raw ingress was disabled this way when cmd_gateway omitted the settings
# that cmd_deploy already supplied.

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
  "RAW_PORT_MAX='\${EDGE_RAW_PORT_MAX:-29999}'"
do
  grep -Fq "$assignment" <<<"$gateway_fn" ||
    fail "gateway-only deploy does not pass $assignment"
done

echo "PASS: gateway-only deploy preserves raw ingress configuration"
