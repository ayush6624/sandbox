#!/usr/bin/env bash
# Run the SDK snapshot-path benchmark against the production fleet without
# copying credentials into the shell history.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$DIR/../.." && pwd)"
# shellcheck source=config.env
source "$DIR/config.env"
# shellcheck source=fleet-secrets.env
source "${FLEET_SECRETS_FILE:-$DIR/fleet-secrets.env}"

CONTROL_NAME="${CONTROL_NAME:-sandbox-control}"
CONTROL_SSH_HOST="${CONTROL_SSH_HOST:-$CONTROL_NAME}"
GW_PORT="${GW_PORT:-9090}"
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=15 -o StrictHostKeyChecking=accept-new)
sshc() { ssh "${SSH_OPTS[@]}" "${SSH_USER}@${CONTROL_SSH_HOST}" "$@"; }

probe=(-s -o /dev/null -m 6 -H "Authorization: Bearer ${GATEWAY_TOKEN}")
url="${SANDBOX_API_URL:-http://${CONTROL_INTERNAL_IP}:${GW_PORT}}"
if ! curl "${probe[@]}" "$url/sandboxes" 2>/dev/null; then
  tailnet_ip="$(sshc 'tailscale ip -4 2>/dev/null | head -1' | tr -d '[:space:]')"
  [ -n "$tailnet_ip" ] || {
    echo "no reachable gateway URL" >&2
    exit 1
  }
  url="http://${tailnet_ip}:${GW_PORT}"
  curl "${probe[@]}" "$url/sandboxes" 2>/dev/null || {
    echo "gateway is unreachable at both VPC and tailnet addresses" >&2
    exit 1
  }
fi

release="$(sshc 'systemctl show sandbox-gateway -p Environment --value 2>/dev/null' \
  | tr ' ' '\n' | sed -n 's/^SANDBOX_RELEASE=//p' | head -1)"
timestamp="$(date -u +%Y%m%d_%H%M%S)"
output="${SNAPSHOT_PATHS_OUTPUT:-$REPO/sdk/typescript/benchmarks/results/snapshot_paths_${release}_${timestamp}.json}"

echo "snapshot paths: release=${release:-unknown} url=${url}"
(
  cd "$REPO/sdk/typescript"
  SANDBOX_API_URL="$url" \
  SANDBOX_API_KEY="$GATEWAY_TOKEN" \
  SANDBOX_RELEASE="${release:-unknown}" \
  BENCH_RUN_ID="snapshot-paths-${release:-unknown}-${timestamp}" \
    npx --yes tsx benchmarks/snapshot-paths-bench.ts --output "$output" "$@"
)
