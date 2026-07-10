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

echo ">> copy job + config to $CONTROL_NAME"
scp -o BatchMode=yes -q "$DIR/nomad/serve.nomad.hcl" "$REPO/configs/devbox-gcp.json" \
  "${SSH_USER}@${CONTROL_NAME}:/tmp/"

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
        /tmp/serve.nomad.hcl"
echo ">> submitted. Watch: ssh ${SSH_USER}@${CONTROL_NAME} 'nomad job status sandbox-serve'"
