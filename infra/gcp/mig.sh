#!/usr/bin/env bash
# The autoscaled worker fleet: a Managed Instance Group of Firecracker hosts
# built from the baked $WORKER_IMAGE_FAMILY image. The Nomad Autoscaler (on the
# control VM) owns the MIG's size — do NOT attach a GCE autoscaler.
#
#   ./mig.sh init      # release bucket + grant the fleet SA read on it; firewall check
#   ./mig.sh up        # create instance template + MIG at MIG_MIN (+ standby pool)
#   ./mig.sh roll       # new template from the current image + rolling replace
#   ./mig.sh standby   # (re)apply the standby-pool policy to a live MIG
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
GOLDEN_FAMILY="${GOLDEN_DATA_IMAGE_FAMILY:-sandbox-golden-data}"
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

# Standby pool: pre-created VMs kept SUSPENDED and/or STOPPED next to the group.
# In scale-out-pool mode a resize (the Nomad Autoscaler's scale-up) resumes a
# suspended VM first, then starts a stopped VM, instead of paying the full
# create+boot path. The MIG replenishes both pools in the background. Suspended
# VMs preserve RAM/device state; stopped VMs preserve disks only. The initial
# delay lets startup-worker.sh + Nomad + sandbox serve finish initialization.
standby_args() {
  local stopped="${STANDBY_STOPPED_SIZE:-0}"
  local suspended="${STANDBY_SUSPENDED_SIZE:-0}"
  if [ "$stopped" -lt 0 ] || [ "$suspended" -lt 0 ]; then
    echo "error: standby sizes must be >= 0" >&2
    return 1
  fi
  if [ $((stopped + suspended)) -gt 0 ]; then
    echo "--stopped-size=$stopped --suspended-size=$suspended --standby-policy-mode=scale-out-pool --standby-policy-initial-delay=${STANDBY_INITIAL_DELAY:-180}"
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
  echo "   sandbox ports 5200-$((5200 + ${PORTS_PER_HOST:-$((4 * ${SLOTS_PER_HOST:-48}))} - 1)), nomad 4646-4648). Default VPC rules:"
  "${GC[@]}" compute firewall-rules list --filter="network=default AND direction=INGRESS" \
    --format="table(name,sourceRanges.list(),allowed[].map().firewall_rule().list())" 2>/dev/null || true
  echo "   If no default-allow-internal covers the subnet, add one internal-allow rule."
}

template_name() { echo "${MIG_NAME}-tpl-$(date +%Y%m%d-%H%M%S)"; }

# data_disk_arg builds the --create-disk spec for the per-instance XFS data disk.
# When a golden data-disk image exists (./bake-image.sh golden), the disk is
# created FROM it — pre-populated with the base rootfs + a pre-built golden
# snapshot, so serve adopts instead of copying+cold-building (startup-worker.sh
# then skips mkfs/copy and just xfs_growfs's to fill the disk). If no such image
# exists yet, fall back to a blank disk (today's behavior: startup-worker
# formats it and stages the rootfs, serve cold-builds the golden). Safe to roll
# the MIG before ever baking a golden.
data_disk_arg() {
  local base="device-name=sandbox-xfs,size=${DATA_DISK},type=pd-ssd,auto-delete=yes"
  if "${GC[@]}" compute images describe-from-family "$GOLDEN_FAMILY" >/dev/null 2>&1; then
    echo "${base},image-family=${GOLDEN_FAMILY},image-project=${PROJECT}"
  else
    echo "$base"
  fi
}

create_template() {
  local tpl="$1"
  local disk_spec; disk_spec="$(data_disk_arg)"
  echo ">> Create instance template $tpl (boot family $IMAGE_FAMILY, data disk: ${disk_spec#*type=pd-ssd,auto-delete=yes}, spot=$WORKER_SPOT)"
  case "$disk_spec" in
    *image-family=*) echo "   data disk seeded from golden image family $GOLDEN_FAMILY (serve adopts the golden)";;
    *)               echo "   data disk BLANK (no $GOLDEN_FAMILY image yet — worker stages rootfs + cold-builds golden)";;
  esac
  # shellcheck disable=SC2046
  "${GC[@]}" compute instance-templates create "$tpl" \
    --machine-type="$MACHINE" \
    --image-family="$IMAGE_FAMILY" --image-project="$PROJECT" \
    --boot-disk-size="${WORKER_BOOT_DISK_SIZE:-256GB}" --boot-disk-type=pd-ssd \
    --create-disk="$disk_spec" \
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
  # shellcheck disable=SC2046
  "${GC[@]}" compute instance-groups managed create "$MIG_NAME" \
    --zone="$ZONE" --template="$tpl" --size="${MIG_MIN:-1}" \
    $(standby_args)
  echo ">> MIG up. The autoscaler resizes it from the sandbox:workers_desired signal."
  if [ $((${STANDBY_STOPPED_SIZE:-0} + ${STANDBY_SUSPENDED_SIZE:-0})) -gt 0 ]; then
    echo ">> Standby pool: ${STANDBY_SUSPENDED_SIZE:-0} suspended + ${STANDBY_STOPPED_SIZE:-0} stopped VMs (scale-out-pool mode)."
  fi
}

cmd_standby() {
  echo ">> Apply standby policy to $MIG_NAME (suspended-size=${STANDBY_SUSPENDED_SIZE:-0}, stopped-size=${STANDBY_STOPPED_SIZE:-0})"
  if [ $((${STANDBY_STOPPED_SIZE:-0} + ${STANDBY_SUSPENDED_SIZE:-0})) -gt 0 ]; then
    # shellcheck disable=SC2046
    "${GC[@]}" compute instance-groups managed update "$MIG_NAME" --zone="$ZONE" $(standby_args)
  else
    "${GC[@]}" compute instance-groups managed update "$MIG_NAME" --zone="$ZONE" \
      --stopped-size=0 --suspended-size=0 --standby-policy-mode=manual
  fi
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
    --format="table(name,targetSize,targetSuspendedSize,targetStoppedSize,standbyPolicy.mode)" 2>/dev/null || { echo "MIG $MIG_NAME not found"; return; }
  "${GC[@]}" compute instance-groups managed list-instances "$MIG_NAME" --zone="$ZONE" \
    --format="table(instance.basename(),instanceStatus,currentAction)"
}

cmd_down() {
  "${GC[@]}" compute instance-groups managed delete "$MIG_NAME" --zone="$ZONE" --quiet
  echo ">> Deleted MIG $MIG_NAME (instance templates kept; delete with gcloud if done)."
}

case "${1:-}" in
  init)    cmd_init ;;
  up)      cmd_up ;;
  roll)    cmd_roll ;;
  standby) cmd_standby ;;
  status)  cmd_status ;;
  down)    cmd_down ;;
  *) echo "usage: $0 {init|up|roll|standby|status|down}" >&2; exit 1 ;;
esac
