#!/usr/bin/env bash
# Create / tear down GCP test VMs with gcloud.
#
#   ./vms.sh up        # create all VMs in config.env
#   ./vms.sh down      # delete them (prompts unless -y)
#   ./vms.sh list      # show status + IPs
#   ./vms.sh ssh NAME  # gcloud ssh into one (e.g. ./vms.sh ssh testvm-1)
#
# Edit config.env first. Needs gcloud authenticated (gcloud auth login) and
# the Compute API enabled (gcloud services enable compute.googleapis.com).
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=config.env
source "$DIR/config.env"

need() { command -v "$1" >/dev/null || { echo "error: '$1' not found on PATH" >&2; exit 1; }; }
need gcloud

GC=(gcloud --project="$PROJECT")

cmd_up() {
  echo ">> Creating ${#NAMES[@]} VM(s) in $PROJECT / $ZONE: ${NAMES[*]}"

  local meta="ssh-user=${SSH_USER}"
  [ -n "${TAILSCALE_AUTHKEY:-}" ] && meta="${meta},tailscale-authkey=${TAILSCALE_AUTHKEY}"
  [ -n "${SSH_PUBLIC_KEY:-}" ]    && meta="${meta},ssh-pubkey=${SSH_PUBLIC_KEY}"

  # Spot / preemptible flags.
  local spot_args=()
  if [ "${SPOT:-false}" = "true" ]; then
    spot_args=(--provisioning-model=SPOT --instance-termination-action="${TERMINATION_ACTION:-STOP}")
    echo "   (Spot VMs — reclaimable; on reclaim: ${TERMINATION_ACTION:-STOP})"
  fi

  # Nested virtualization: needed so the guest exposes /dev/kvm to Firecracker.
  local virt_args=()
  if [ "${ENABLE_NESTED_VIRT:-false}" = "true" ]; then
    virt_args=(--enable-nested-virtualization)
    echo "   (nested virtualization ON — required for Firecracker/KVM)"
  fi

  # Service account: when SNAPSHOT_BUCKET is set, attach the fleet SA so hosts
  # can read/write GCS snapshot artifacts (create it first: snapshot-store.sh
  # init). Otherwise the VMs get no service account at all, as before.
  local sa_args=(--no-service-account --no-scopes)
  if [ -n "${SNAPSHOT_BUCKET:-}" ]; then
    sa_args=(--service-account="sandbox-fleet-sa@${PROJECT}.iam.gserviceaccount.com" --scopes=storage-rw)
    echo "   (service account sandbox-fleet-sa attached — GCS snapshot store)"
  fi

  # One call creates all instances with identical config.
  "${GC[@]}" compute instances create "${NAMES[@]}" \
    --zone="$ZONE" \
    --machine-type="$MACHINE_TYPE" \
    --image-family="$IMAGE_FAMILY" \
    --image-project="$IMAGE_PROJECT" \
    --boot-disk-size="$DISK_SIZE" \
    --boot-disk-type="$DISK_TYPE" \
    --create-disk="device-name=sandbox-xfs,size=${DATA_DISK_SIZE},type=${DATA_DISK_TYPE},auto-delete=yes" \
    "${sa_args[@]}" \
    "${spot_args[@]}" \
    "${virt_args[@]}" \
    --metadata="$meta" \
    --metadata-from-file=startup-script="$DIR/startup.sh"

  echo
  cmd_list
  echo
  echo ">> Provisioning (user + Tailscale) runs on first boot via startup.sh."
  echo "   Check progress on a box: ./vms.sh ssh ${NAMES[0]} -- sudo tail -f /var/log/startup-script.log"
  [ -n "${TAILSCALE_AUTHKEY:-}" ] && echo "   Then SSH over Tailscale: ssh ${SSH_USER}@${NAMES[0]}"
}

cmd_down() {
  local yes=""
  [ "${1:-}" = "-y" ] && yes="--quiet"
  echo ">> Deleting: ${NAMES[*]}"
  "${GC[@]}" compute instances delete "${NAMES[@]}" --zone="$ZONE" $yes
  echo ">> Done. Tailscale nodes are ephemeral and auto-remove once offline."
}

cmd_list() {
  "${GC[@]}" compute instances list \
    --filter="name=($(IFS=,; echo "${NAMES[*]}"))" \
    --format="table(name, zone.basename(), machineType.basename(), status, networkInterfaces[0].accessConfigs[0].natIP:label=EXTERNAL_IP, networkInterfaces[0].networkIP:label=INTERNAL_IP)"
}

cmd_ssh() {
  local name="${1:-}"; shift || true
  [ -z "$name" ] && { echo "usage: ./vms.sh ssh NAME [-- cmd...]" >&2; exit 1; }
  "${GC[@]}" compute ssh "${SSH_USER}@${name}" --zone="$ZONE" "$@"
}

case "${1:-}" in
  up)   shift; cmd_up "$@" ;;
  down) shift; cmd_down "$@" ;;
  list) shift; cmd_list "$@" ;;
  ssh)  shift; cmd_ssh "$@" ;;
  *)    echo "usage: $0 {up|down [-y]|list|ssh NAME}" >&2; exit 1 ;;
esac
