#!/usr/bin/env bash
# Extensive benchmark matrix for the sandbox fleet: default- vs snapshot-source
# latency, snapshot-source batch scaling, fleet throughput across workload
# modes, and memory density.
# Runs SEQUENTIALLY — every run shares the per-host IP/tap pools, so parallel
# runs would collide on allocation. Collects one JSON per run for reporting.
#
# Usage (from repo root, fleet already up and bootstrapped):
#   HOST_URL=https://<worker-ip>:8080 GATEWAY_URL=https://<gateway-ip>:9090 \
#     SANDBOX_HOST_KEY=... SANDBOX_API_KEY=... SSH_HOST=<worker-ip> \
#     SANDBOX_RELEASE=<release> bash scripts/bench-extensive.sh
#
# CURL_CA_BUNDLE and NODE_EXTRA_CA_CERTS may point at a private management CA.
# HOST_TOKEN and GATEWAY_TOKEN remain compatibility credential inputs.
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
BENCH_RUN_ID="${BENCH_RUN_ID:-extensive-$RUN_STAMP}"
BENCH="${BENCH:-$REPO/sdk/typescript/benchmarks/results/$BENCH_RUN_ID}"
HOST_URL="${HOST_URL:-}"
GATEWAY_URL="${GATEWAY_URL:-}"
SANDBOX_HOST_KEY="${SANDBOX_HOST_KEY:-${HOST_TOKEN:-}}"
SANDBOX_API_KEY="${SANDBOX_API_KEY:-${GATEWAY_TOKEN:-}}"
SINGLE_HOST_COUNT="${SINGLE_HOST_COUNT:-48}"
: "${HOST_URL:?set HOST_URL to the direct worker API}"
: "${GATEWAY_URL:?set GATEWAY_URL to the fleet gateway}"
: "${SANDBOX_HOST_KEY:?set SANDBOX_HOST_KEY to the worker client credential}"
: "${SANDBOX_API_KEY:?set SANDBOX_API_KEY to the gateway client credential}"
: "${SSH_HOST:?set SSH_HOST to the direct worker SSH target}"
: "${SANDBOX_RELEASE:?set SANDBOX_RELEASE to the deployed release}"
[[ "$SINGLE_HOST_COUNT" =~ ^[1-9][0-9]*$ ]] || {
  echo "error: SINGLE_HOST_COUNT must be a positive integer" >&2
  exit 1
}
for url in "$HOST_URL" "$GATEWAY_URL"; do
  case "$url" in
    https://*) ;;
    http://127.0.0.1:*|http://localhost:*|http://\[::1\]:*) ;;
    http://*)
      [ "${PRIVATE_MANAGEMENT_PROXY_ACK:-}" = "I_VERIFIED_PRIVATE_AUTHENTICATED_PROXY" ] || {
        echo "error: refusing non-loopback plaintext management URL $url" >&2
        echo "set PRIVATE_MANAGEMENT_PROXY_ACK=I_VERIFIED_PRIVATE_AUTHENTICATED_PROXY only after verifying the private authenticated boundary" >&2
        exit 1
      }
      ;;
    *)
      echo "error: management URL must use http:// or https://: $url" >&2
      exit 1
      ;;
  esac
done
mkdir -p "$BENCH"
cd "$REPO/sdk/typescript"

TSX=node_modules/.bin/tsx
[ -x "$TSX" ] || {
  echo "error: run npm install in $REPO/sdk/typescript first" >&2
  exit 1
}
banner(){ echo -e "\n########## $* ##########"; }

export BENCH_RUN_ID SANDBOX_RELEASE

banner "1/4 LATENCY: default vs snapshot source (25 iterations)"
SANDBOX_API_URL="$HOST_URL" SANDBOX_API_KEY="$SANDBOX_HOST_KEY" \
  "$TSX" benchmarks/snapshot-source-bench.ts \
  --iterations 25 --output "$BENCH/snapshot-source.json"

SNAPSHOT_COUNTS=""
for count in 1 2 4 8 16 32 "$SINGLE_HOST_COUNT"; do
  [ "$count" -le "$SINGLE_HOST_COUNT" ] || continue
  case ",$SNAPSHOT_COUNTS," in
    *",$count,"*) ;;
    *) SNAPSHOT_COUNTS="${SNAPSHOT_COUNTS:+$SNAPSHOT_COUNTS,}$count" ;;
  esac
done
banner "2/4 SNAPSHOT-SOURCE BATCH: N=$SNAPSHOT_COUNTS + default-source baseline"
SANDBOX_API_URL="$HOST_URL" SANDBOX_API_KEY="$SANDBOX_HOST_KEY" \
  "$TSX" benchmarks/snapshot-batch-bench.ts \
  --counts "$SNAPSHOT_COUNTS" --baseline \
  --output "$BENCH/snapshot-batch.json"

banner "3/4 FLEET THROUGHPUT: several sizes and workload modes"
for spec in "32 default" "64 default" "128 default" "64 fsync" "64 large"; do
  set -- $spec
  echo "--- fleet count=$1 mode=$2 ---"
  SANDBOX_API_URL="$GATEWAY_URL" SANDBOX_API_KEY="$SANDBOX_API_KEY" \
    "$TSX" benchmarks/fleet-bench.ts \
    --count "$1" --mode "$2" --create-concurrency 12 --run-concurrency "$1" \
    --output "$BENCH/fleet_${1}_${2}.json"
done

banner "4/4 MEMORY DENSITY: $SINGLE_HOST_COUNT snapshot-source vs default-source sandboxes"
API="$HOST_URL" TOK="$SANDBOX_HOST_KEY" HOST="$SSH_HOST" \
  SSH_HOST="$SSH_HOST" N="$SINGLE_HOST_COUNT" OUT="$BENCH/memory-density.json" \
  BENCH_RUN_ID="$BENCH_RUN_ID" SANDBOX_RELEASE="$SANDBOX_RELEASE" \
  bash "$REPO/scripts/mem-density.sh"

banner "DONE — JSON in $BENCH"
ls -la "$BENCH"
