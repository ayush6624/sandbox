#!/usr/bin/env bash
# Submit (or update) the sandbox-serve Nomad system job on the control VM.
#
#   ./deploy-job.sh [<git-sha>]
#
# Defaults the release to the current git short sha (what `make gcs-release`
# just uploaded). Copies the job file + config to the control VM and runs
# `nomad job run` there, passing tokens + the config JSON as HCL2 vars. Changing
# the sha rolls the job across every worker.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$DIR/../.." && pwd)"
# shellcheck source=config.env
source "$DIR/config.env"
[ -f "$DIR/fleet-secrets.env" ] && source "$DIR/fleet-secrets.env"
: "${GATEWAY_TOKEN:?run control.sh deploy first (populates fleet-secrets.env)}"
: "${HOST_TOKEN:?run control.sh deploy first}"

RELEASE="${1:-$(git -C "$REPO" rev-parse --short HEAD)}"
CONTROL_NAME="${CONTROL_NAME:-sandbox-control}"
CONTROL_IP="${CONTROL_INTERNAL_IP:?}"
GW_URL="http://${CONTROL_IP}:${GW_PORT:-9090}"

sshc() { ssh -o BatchMode=yes "${SSH_USER}@${CONTROL_NAME}" "$@"; }

# --- derive per-host capacity from SLOTS_PER_HOST (the single source of truth) ---
# The pools in devbox-gcp.json are GENERATED here, not hand-maintained, so the
# autoscaler math (SLOTS_PER_HOST) and the hosts' actual pools cannot drift:
#   taps  = N                          (fc0..fcN-1)
#   IPs   = N                          (172.16.0.10 .. 172.16.0.(9+N))
#   ports = 4N                         (hibernated sandboxes hold their port and
#                                       extra exposed ports drain the same pool;
#                                       4x keeps ports from ever binding Slots())
SLOTS="${SLOTS_PER_HOST:?set SLOTS_PER_HOST in config.env}"
command -v jq >/dev/null || { echo "error: deploy-job.sh needs jq"; exit 1; }
if [ "$SLOTS" -lt 1 ] || [ "$SLOTS" -gt 200 ]; then
  # 200 slots ~= 209 IPs ending at 172.16.0.209 — comfortably inside the /24
  # guest subnet (which the host code hard-codes). Beyond that widen the subnet
  # first (internal/server GuestCIDR + provisioner GatewayCIDR).
  echo "error: SLOTS_PER_HOST=$SLOTS out of range [1,200] (the /24 guest subnet is the wall)"
  exit 1
fi
GEN_CONFIG="$(mktemp)"
# mem_budget_mib = SLOTS x 1180 = TASK_MEMORY - 2000 (the Nomad cgroup minus
# serve's own reserve): the sum-of-guest-memory admission ceiling. Must be set
# explicitly on Nomad hosts — serve's /proc/meminfo fallback sees the machine
# total, not the cgroup limit.
jq --argjson n "$SLOTS" '
  .pools.TapMax     = $n |
  .pools.GuestIPMax = ("172.16.0." + (9 + $n | tostring)) |
  .pools.PortMax    = (.pools.PortMin + 4 * $n - 1) |
  .mem_budget_mib   = ($n * 1180)
' "$REPO/configs/devbox-gcp.json" > "$GEN_CONFIG"

# Size the Nomad task cgroup to the host: ~1.18 GiB per slot (1 GiB guest +
# firecracker overhead) + 2 GiB for serve itself; CPU shares near the machine's
# core count (parsed from WORKER_MACHINE_TYPE, e.g. n2-standard-16 -> 16).
TASK_MEMORY="$(( SLOTS * 1180 + 2000 ))"
CORES="$(echo "${WORKER_MACHINE_TYPE:-n2-standard-16}" | grep -oE '[0-9]+$' || echo 16)"
TASK_CPU="$(( (CORES - 1) * 1000 ))"

echo ">> copy job + generated config to $CONTROL_NAME (slots=$SLOTS mem=${TASK_MEMORY}MiB cpu=${TASK_CPU})"
scp -o BatchMode=yes -q "$DIR/nomad/serve.nomad.hcl" "${SSH_USER}@${CONTROL_NAME}:/tmp/serve.nomad.hcl"
scp -o BatchMode=yes -q "$GEN_CONFIG" "${SSH_USER}@${CONTROL_NAME}:/tmp/devbox-gcp.json"
rm -f "$GEN_CONFIG"

echo ">> nomad job run sandbox-serve (release=$RELEASE)"
# Values are expanded locally into single-quoted -var args (tokens are hex, the
# URL/paths have no quotes) — NOT passed via a remote env prefix, which wouldn't
# be visible to the remote shell's own $VAR expansion on the same command line.
sshc "nomad job run \
        -var=gateway_url='$GW_URL' \
        -var=gateway_token='$GATEWAY_TOKEN' \
        -var=host_token='$HOST_TOKEN' \
        -var=release='$RELEASE' \
        -var=bucket='$RELEASE_BUCKET' \
        -var=config_path=/tmp/devbox-gcp.json \
        -var=task_cpu='$TASK_CPU' \
        -var=task_memory='$TASK_MEMORY' \
        /tmp/serve.nomad.hcl"
echo ">> submitted. Watch: ssh ${SSH_USER}@${CONTROL_NAME} 'nomad job status sandbox-serve'"
