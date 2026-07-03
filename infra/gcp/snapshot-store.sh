#!/usr/bin/env bash
# GCS snapshot durability: bucket + service account for the fleet.
#
#   ./snapshot-store.sh init      # create bucket + SA + IAM binding (idempotent)
#   ./snapshot-store.sh attach    # attach the SA to every VM in NAMES (stop→set→start, serial)
#   ./snapshot-store.sh status    # show bucket + per-VM service account
#
# The bucket name must match "snapshot_bucket" in configs/devbox-gcp.json —
# that's what `sandbox serve` reads. Hosts authenticate via the attached
# service account (GCE metadata server); no key files anywhere.
#
# attach requires a stop/start per VM (GCP can't change the SA of a running
# instance). VMs are done one at a time so the rest of the fleet keeps
# serving; data disks persist, so no re-bootstrap — but any sandboxes running
# on the VM die with the stop (they're disposable).
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=config.env
source "$DIR/config.env"

SNAPSHOT_BUCKET="${SNAPSHOT_BUCKET:-${PROJECT}-sandbox-snapshots}"
SA_NAME="sandbox-fleet-sa"
SA_EMAIL="${SA_NAME}@${PROJECT}.iam.gserviceaccount.com"
REGION="${ZONE%-*}"   # asia-south1-a → asia-south1

GC=(gcloud --project="$PROJECT")

cmd_init() {
  echo ">> Bucket gs://${SNAPSHOT_BUCKET} (${REGION})"
  if ! "${GC[@]}" storage buckets describe "gs://${SNAPSHOT_BUCKET}" >/dev/null 2>&1; then
    "${GC[@]}" storage buckets create "gs://${SNAPSHOT_BUCKET}" \
      --location="$REGION" --uniform-bucket-level-access
  else
    echo "   already exists"
  fi

  echo ">> Service account ${SA_EMAIL}"
  if ! "${GC[@]}" iam service-accounts describe "$SA_EMAIL" >/dev/null 2>&1; then
    "${GC[@]}" iam service-accounts create "$SA_NAME" \
      --display-name="sandbox fleet (GCS snapshot store)"
  else
    echo "   already exists"
  fi

  echo ">> Granting roles/storage.objectAdmin on the bucket (bucket-scoped only)"
  "${GC[@]}" storage buckets add-iam-policy-binding "gs://${SNAPSHOT_BUCKET}" \
    --member="serviceAccount:${SA_EMAIL}" --role="roles/storage.objectAdmin" >/dev/null
  echo ">> Done. Ensure configs/devbox-gcp.json has \"snapshot_bucket\": \"${SNAPSHOT_BUCKET}\""
}

cmd_attach() {
  for h in "${NAMES[@]}"; do
    local_sa="$("${GC[@]}" compute instances describe "$h" --zone="$ZONE" \
      --format='value(serviceAccounts[0].email)' 2>/dev/null || true)"
    if [ "$local_sa" = "$SA_EMAIL" ]; then
      echo ">> [$h] already has ${SA_NAME}; skipping"
      continue
    fi
    echo ">> [$h] stop → set-service-account → start"
    "${GC[@]}" compute instances stop "$h" --zone="$ZONE"
    "${GC[@]}" compute instances set-service-account "$h" --zone="$ZONE" \
      --service-account="$SA_EMAIL" --scopes=storage-rw
    "${GC[@]}" compute instances start "$h" --zone="$ZONE"
    echo ">> [$h] done (Tailscale + serve come back via startup script/systemd)"
  done
  echo ">> All hosts attached. Restart sandbox-serve units after 'make sync' so the new config lands."
}

cmd_status() {
  echo ">> Bucket: gs://${SNAPSHOT_BUCKET}"
  "${GC[@]}" storage buckets describe "gs://${SNAPSHOT_BUCKET}" \
    --format='value(location,storage_class)' 2>/dev/null || echo "   (missing — run init)"
  echo ">> Per-VM service accounts:"
  for h in "${NAMES[@]}"; do
    sa="$("${GC[@]}" compute instances describe "$h" --zone="$ZONE" \
      --format='value(serviceAccounts[0].email)' 2>/dev/null || echo '?')"
    echo "   [$h] ${sa:-none}"
  done
}

case "${1:-}" in
  init)   cmd_init ;;
  attach) cmd_attach ;;
  status) cmd_status ;;
  *) echo "usage: $0 {init|attach|status}" >&2; exit 1 ;;
esac
