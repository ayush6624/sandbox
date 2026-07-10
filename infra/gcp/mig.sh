#!/usr/bin/env bash
# The autoscaled worker fleet: a Managed Instance Group of Firecracker hosts
# built from the baked $WORKER_IMAGE_FAMILY image. The Nomad Autoscaler (on the
# control VM) owns the MIG's size — do NOT attach a GCE autoscaler.
#
#   ./mig.sh init      # release bucket + grant the fleet SA read on it; firewall check
#   ./mig.sh up        # create instance template + MIG at MIG_MIN
#   ./mig.sh roll       # new template from the current image + rolling replace
#   ./mig.sh status    # MIG + managed instances
#   ./mig.sh down       # delete the MIG (keeps templates)
#
# Workers self-register with Nomad (retry_join CONTROL_INTERNAL_IP) via
# startup-worker.sh; Nomad places the sandbox-serve system job on them.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=config.env
source "$DIR/config.env"

MIG_NAME="${MIG_NAME:-sandbox-workers}"
IMAGE_FAMILY="${WORKER_IMAGE_FAMILY:-sandbox-worker}"
MACHINE="${WORKER_MACHINE_TYPE:-n2-standard-8}"
DATA_DISK="${WORKER_DATA_DISK_SIZE:-256GB}"
CONTROL_IP="${CONTROL_INTERNAL_IP:?set CONTROL_INTERNAL_IP in config.env}"
SA_EMAIL="sandbox-fleet-sa@${PROJECT}.iam.gserviceaccount.com"   # reuse the snapshot SA
RELEASE_BUCKET="${RELEASE_BUCKET:?set RELEASE_BUCKET in config.env}"
REGION="${ZONE%-*}"
GC=(gcloud --project="$PROJECT")

# Non-spot by default (running sandboxes must not be preempted); WORKER_SPOT=true
# opts a dev fleet into cheap, evictable instances.
spot_args() {
  if [ "${WORKER_SPOT:-false}" = "true" ]; then
    echo "--provisioning-model=SPOT --instance-termination-action=DELETE"
  else
    echo "--provisioning-model=STANDARD"
  fi
}

cmd_init() {
  echo ">> Release bucket gs://${RELEASE_BUCKET}"
  "${GC[@]}" storage buckets describe "gs://${RELEASE_BUCKET}" >/dev/null 2>&1 || \
    "${GC[@]}" storage buckets create "gs://${RELEASE_BUCKET}" --location="$REGION" --uniform-bucket-level-access
  echo ">> Grant ${SA_EMAIL} objectViewer on the release bucket"
  "${GC[@]}" storage buckets add-iam-policy-binding "gs://${RELEASE_BUCKET}" \
    --member="serviceAccount:${SA_EMAIL}" --role="roles/storage.objectViewer" >/dev/null
  echo ">> Firewall: internal traffic must be allowed (heartbeat 9090, host 8080,"
  echo "   sandbox ports 5200-5223, nomad 4646-4648). Default VPC rules:"
  "${GC[@]}" compute firewall-rules list --filter="network=default AND direction=INGRESS" \
    --format="table(name,sourceRanges.list(),allowed[].map().firewall_rule().list())" 2>/dev/null || true
  echo "   If no default-allow-internal covers the subnet, add one internal-allow rule."
}

template_name() { echo "${MIG_NAME}-tpl-$(date +%Y%m%d-%H%M%S)"; }

create_template() {
  local tpl="$1"
  echo ">> Create instance template $tpl (image family $IMAGE_FAMILY, spot=$WORKER_SPOT)"
  # shellcheck disable=SC2046
  "${GC[@]}" compute instance-templates create "$tpl" \
    --machine-type="$MACHINE" \
    --image-family="$IMAGE_FAMILY" --image-project="$PROJECT" \
    --boot-disk-size="${WORKER_BOOT_DISK_SIZE:-256GB}" --boot-disk-type=pd-ssd \
    --create-disk="device-name=sandbox-xfs,size=${DATA_DISK},type=pd-ssd,auto-delete=yes" \
    --enable-nested-virtualization \
    --service-account="$SA_EMAIL" --scopes=storage-rw \
    $(spot_args) \
    --metadata="nomad-server-ip=${CONTROL_IP}" \
    --metadata-from-file=startup-script="$DIR/startup-worker.sh"
}

cmd_up() {
  local tpl; tpl="$(template_name)"
  create_template "$tpl"
  echo ">> Create MIG $MIG_NAME at size ${MIG_MIN:-1} (Nomad Autoscaler owns size hereafter)"
  "${GC[@]}" compute instance-groups managed create "$MIG_NAME" \
    --zone="$ZONE" --template="$tpl" --size="${MIG_MIN:-1}"
  echo ">> MIG up. The autoscaler resizes it from the sandbox:workers_desired signal."
}

cmd_roll() {
  local tpl; tpl="$(template_name)"
  create_template "$tpl"
  echo ">> Rolling-replace $MIG_NAME onto $tpl"
  "${GC[@]}" compute instance-groups managed set-instance-template "$MIG_NAME" --zone="$ZONE" --template="$tpl"
  "${GC[@]}" compute instance-groups managed rolling-action replace "$MIG_NAME" --zone="$ZONE" --max-unavailable=1
}

cmd_status() {
  "${GC[@]}" compute instance-groups managed describe "$MIG_NAME" --zone="$ZONE" \
    --format="value(name,targetSize)" 2>/dev/null || { echo "MIG $MIG_NAME not found"; return; }
  "${GC[@]}" compute instance-groups managed list-instances "$MIG_NAME" --zone="$ZONE" \
    --format="table(instance.basename(),instanceStatus,currentAction)"
}

cmd_down() {
  "${GC[@]}" compute instance-groups managed delete "$MIG_NAME" --zone="$ZONE" --quiet
  echo ">> Deleted MIG $MIG_NAME (instance templates kept; delete with gcloud if done)."
}

case "${1:-}" in
  init)   cmd_init ;;
  up)     cmd_up ;;
  roll)   cmd_roll ;;
  status) cmd_status ;;
  down)   cmd_down ;;
  *) echo "usage: $0 {init|up|roll|status|down}" >&2; exit 1 ;;
esac
