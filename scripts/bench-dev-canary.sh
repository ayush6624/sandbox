#!/usr/bin/env bash
# Run a measured development-fleet case from the control VM.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"
bash infra/gcp/require-control-vm.sh bench-dev-canary
SUITE="${1:?usage: bench-dev-canary.sh <suite> <output.json> [benchmark arguments]}"
OUTPUT="${2:?provide a result path relative to sdk/typescript}"
shift 2
case "$SUITE" in lifecycle|snapshot-working-set|snapshot-peer-transfer|template-warm) ;;
  *) echo "unsupported suite: $SUITE" >&2; exit 2 ;;
esac
source "${SANDBOX_GCP_CONFIG:?select a fleet config}"
source "${FLEET_SECRETS_FILE:-$REPO/infra/gcp/fleet-secrets.env}"
export SANDBOX_API_URL="http://${CONTROL_INTERNAL_IP}:${GW_PORT:-9090}"
export SANDBOX_API_KEY="$GATEWAY_TOKEN"
if [ "$SUITE" = snapshot-peer-transfer ]; then
  export SANDBOX_API_KEY="$HOST_TOKEN"
  export SANDBOX_CONTROL_KEY="$HOST_TOKEN"
  export SANDBOX_GATEWAY_API_KEY="$GATEWAY_TOKEN"
fi
: "${SANDBOX_RELEASE:?set the release observed on the deployed fleet}"
: "${BENCH_GUEST_IMAGE_SHA256:?hash the actual worker base rootfs before running}"
: "${BENCH_CACHE_STATE:?declare the cache conditions}"
export SANDBOX_RELEASE BENCH_GUEST_IMAGE_SHA256 BENCH_CACHE_STATE
export BENCH_RUNNER_REGION="${ZONE%-*}"
node -e 'if (Number(process.versions.node.split(".")[0]) < 22) throw Error("Node 22 or later required")'
cd sdk/typescript
status=0
node --import tsx "benchmarks/${SUITE}-bench.ts" "$@" --output "$OUTPUT" || status=$?
node --import tsx benchmarks/validate-report.ts "$OUTPUT" || status=1
exit "$status"
