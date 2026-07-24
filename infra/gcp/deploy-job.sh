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
#   IPs   = N                          (GuestIPMin .. GuestIPMin+N-1, octet-spanning)
#   ports = PORTS_PER_HOST, default 4N (see below)
# SLOTS bounds concurrently RUNNING sandboxes: each holds a tap, an IP, and
# committed memory. It is NO LONGER capped at 200 — the wall is the guest
# subnet, whose width GUEST_SUBNET_BITS now controls (see below).
SLOTS="${SLOTS_PER_HOST:?set SLOTS_PER_HOST in config.env}"
command -v jq >/dev/null || { echo "error: deploy-job.sh needs jq"; exit 1; }

# GUEST_SUBNET_BITS: the prefix length of the guest subnet (bridge + every guest
# NIC). It is the hard ceiling on concurrently running sandboxes per host: a /24
# holds ~253 usable IPs, /22 ~1021, /20 ~4093. Default 24 (unchanged from the
# fleet's original single-/24). Widen it to run more than ~250 small sandboxes
# at once. The host reads it as guest_subnet_bits and applies it to the gateway
# CIDR, the cold-boot guest CIDR, AND the clone-path MMDS reidentify prefix.
BITS="${GUEST_SUBNET_BITS:-24}"
if [ "$BITS" -lt 8 ] || [ "$BITS" -gt 30 ]; then
  echo "error: GUEST_SUBNET_BITS=$BITS out of range [8,30]"; exit 1
fi
GW_IP="$(jq -r '.gateway_ip'      "$REPO/configs/devbox-gcp.json")"
GIP_MIN="$(jq -r '.pools.GuestIPMin' "$REPO/configs/devbox-gcp.json")"
ip2int() { local a b c d; IFS=. read -r a b c d <<<"$1"; echo $(( (a<<24)+(b<<16)+(c<<8)+d )); }
int2ip() { local i=$1; echo "$(( (i>>24)&255 )).$(( (i>>16)&255 )).$(( (i>>8)&255 )).$(( i&255 ))"; }
MASK=$(( (0xFFFFFFFF << (32-BITS)) & 0xFFFFFFFF ))
NET=$(( $(ip2int "$GW_IP") & MASK ))
BCAST=$(( NET | (~MASK & 0xFFFFFFFF) ))
GIP_MIN_INT=$(ip2int "$GIP_MIN")
GIP_MAX_INT=$(( GIP_MIN_INT + SLOTS - 1 ))
# Usable host IPs strictly between network and broadcast, above the pool start.
USABLE=$(( BCAST - GIP_MIN_INT ))   # broadcast excluded, min inclusive
if [ "$SLOTS" -lt 1 ] || [ "$SLOTS" -gt "$USABLE" ]; then
  echo "error: SLOTS_PER_HOST=$SLOTS exceeds the /$BITS guest subnet's usable IPs from $GIP_MIN ($USABLE). Widen GUEST_SUBNET_BITS."
  exit 1
fi
GIP_MAX="$(int2ip "$GIP_MAX_INT")"

# Ports are sized separately from VM capacity and are allocated only for
# explicit guest-port mappings. Hibernated sandboxes retain those mappings for
# wake-on-connect. Default to 4x SLOTS to leave room for multiple services.
PORTS="${PORTS_PER_HOST:-$((4 * SLOTS))}"
if [ "$PORTS" -lt 1 ]; then
  echo "error: PORTS_PER_HOST=$PORTS must be >= 1"
  exit 1
fi

# MEM_PER_SLOT_MIB: committed memory charged per running slot = the average
# sandbox's guest mem_mib + ~156 MiB firecracker/VMM overhead. Default 1180
# (1 GiB guest + overhead), matching the original template. LOWER it for a
# small-sandbox fleet (e.g. 128 MiB guests -> ~300) so the same host RAM admits
# far more running sandboxes; mem_budget_mib and the Nomad cgroup both scale
# from it, keeping the admission ceiling honest to what the host can actually
# hold. Must be set explicitly on Nomad hosts — serve's /proc/meminfo fallback
# sees the machine total, not the cgroup limit.
MEM_PER_SLOT="${MEM_PER_SLOT_MIB:-1180}"
GEN_CONFIG="$(mktemp)"
jq --argjson n "$SLOTS" --argjson p "$PORTS" --argjson bits "$BITS" \
   --arg gipmax "$GIP_MAX" --argjson mps "$MEM_PER_SLOT" '
  .guest_subnet_bits = $bits |
  .pools.TapMax      = $n |
  .pools.GuestIPMax  = $gipmax |
  .pools.PortMax     = (.pools.PortMin + $p - 1) |
  .mem_budget_mib    = ($n * $mps)
' "$REPO/configs/devbox-gcp.json" > "$GEN_CONFIG"

# Size the Nomad task cgroup to the host: MEM_PER_SLOT_MIB per slot + 2 GiB for
# serve itself; CPU shares near the machine's core count (parsed from
# WORKER_MACHINE_TYPE, e.g. n2-standard-16 -> 16).
TASK_MEMORY="$(( SLOTS * MEM_PER_SLOT + 2000 ))"
CORES="$(echo "${WORKER_MACHINE_TYPE:-n2-standard-16}" | grep -oE '[0-9]+$' || echo 16)"
TASK_CPU="$(( (CORES - 1) * 1000 ))"

echo ">> copy job + generated config to $CONTROL_NAME (slots=$SLOTS /$BITS IPs=$GIP_MIN..$GIP_MAX ports=$PORTS mem/slot=${MEM_PER_SLOT} budget/cgroup=$(( SLOTS * MEM_PER_SLOT ))/${TASK_MEMORY}MiB cpu=${TASK_CPU})"
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
