#!/usr/bin/env bash
# Extensive benchmark matrix for the sandbox fleet: latency (cold vs restore),
# fan-out scaling, fleet throughput across workload modes, and memory density.
# Runs SEQUENTIALLY — every run shares the per-host IP/tap pools, so parallel
# runs would collide on allocation. Collects one JSON per run for reporting.
#
# Usage (from repo root, fleet already up and bootstrapped):
#   HOST_IP=<tv1-tailnet-ip> SSH_HOST=<tv1-tailnet-ip> \
#     HOST_TOKEN=... GATEWAY_TOKEN=... bash scripts/bench-extensive.sh
#
# HOST_TOKEN / GATEWAY_TOKEN come from infra/gcp/fleet-secrets.env. HOST_IP is the
# host that owns single-host runs (latency/fanout/memory are host-local); the
# gateway is assumed on the same box at :9090.
set -uo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH="${BENCH:-$REPO/sdk/typescript/benchmarks/results/extensive}"
: "${HOST_IP:?set HOST_IP}"; : "${SSH_HOST:=$HOST_IP}"
: "${HOST_TOKEN:?}"; : "${GATEWAY_TOKEN:?}"
mkdir -p "$BENCH"
cd "$REPO/sdk/typescript"

HOST="http://$HOST_IP:8080"; GW="http://$HOST_IP:9090"; TSX=node_modules/.bin/tsx
banner(){ echo -e "\n########## $* ##########"; }

banner "1/4 LATENCY: cold boot vs restore (25 iters)"
SANDBOX_API_URL="$HOST" SANDBOX_API_KEY="$HOST_TOKEN" \
  $TSX benchmarks/restore-bench.ts --iterations 25 --output "$BENCH/latency.json"

banner "2/4 FAN-OUT SCALING: N=1..64 + cold-boot baseline"
SANDBOX_API_URL="$HOST" SANDBOX_API_KEY="$HOST_TOKEN" \
  $TSX benchmarks/fanout-bench.ts --counts 1,2,4,8,16,32,48,64 --baseline --output "$BENCH/fanout.json"

banner "3/4 FLEET THROUGHPUT: via gateway, several N + workload modes"
for spec in "32 default" "64 default" "128 default" "64 fsync" "64 large"; do
  set -- $spec
  echo "--- fleet count=$1 mode=$2 ---"
  SANDBOX_API_URL="$GW" SANDBOX_API_KEY="$GATEWAY_TOKEN" \
    $TSX benchmarks/fleet-bench.ts --count "$1" --mode "$2" --create-concurrency 12 --run-concurrency "$1" \
      --output "$BENCH/fleet_${1}_${2}.json" | tail -20
done

banner "4/4 MEMORY DENSITY: 64 fan-out clones vs 64 cold boots"
API="$HOST" TOK="$HOST_TOKEN" HOST="$HOST_IP" SSH_HOST="$SSH_HOST" N=64 OUT="$BENCH/mem.json" \
  bash "$REPO/scripts/mem-density.sh"

banner "DONE — JSON in $BENCH"
ls -la "$BENCH"
