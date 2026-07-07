#!/usr/bin/env bash
# GCE startup-script for autoscaled Firecracker worker instances (MIG). Runs as
# root on every boot; idempotent. The image (bake-image.sh) already has
# firecracker, the guest kernel, the base rootfs at /opt/fc, and a Nomad client
# (disabled, with a client.hcl template). This script:
#   1. formats + mounts the blank per-instance XFS data disk
#   2. stages the base rootfs onto it (where configs/devbox-gcp.json expects it)
#   3. renders the Nomad client config with the control VM's IP and starts Nomad
# Nomad then places the `sandbox-serve` system job, which pulls binaries from
# GCS, bakes sandboxd into the rootfs, and runs `sandbox serve`.
# Output: /var/log/startup-script.log
set -euxo pipefail
exec > >(tee -a /var/log/startup-script.log) 2>&1

meta() {
  curl -fsS -H "Metadata-Flavor: Google" \
    "http://metadata.google.internal/computeMetadata/v1/instance/attributes/$1" 2>/dev/null || true
}

NOMAD_SERVER_IP="$(meta nomad-server-ip)"
[ -n "$NOMAD_SERVER_IP" ] || { echo "FATAL: no nomad-server-ip metadata"; exit 1; }

#############################################
# 1. XFS data disk (reflink CoW for rootfs + snapshots)
#############################################
XFS_DEV=/dev/disk/by-id/google-sandbox-xfs
XFS_MNT=/mnt/sandbox-data
if [ ! -e "$XFS_DEV" ]; then
  echo "FATAL: $XFS_DEV not attached — instance template must add the data disk"
  exit 1
fi
if ! blkid "$XFS_DEV" | grep -q 'TYPE="xfs"'; then
  mkfs.xfs -f "$XFS_DEV"
fi
mkdir -p "$XFS_MNT"
XFS_UUID="$(blkid -s UUID -o value "$XFS_DEV")"
grep -q "$XFS_UUID" /etc/fstab || \
  echo "UUID=$XFS_UUID $XFS_MNT xfs defaults,nofail 0 2" >> /etc/fstab
mountpoint -q "$XFS_MNT" || mount "$XFS_MNT"
mkdir -p "$XFS_MNT"/{base,rootfs,snapshots}

#############################################
# 2. Stage the base rootfs onto the data disk
#############################################
# The image baked the rootfs to the boot disk (/opt/fc); configs/devbox-gcp.json
# reads it from /mnt/sandbox-data/base. Copy once (fresh data disk each boot on
# a spot-recreated instance, so this runs on first boot of each instance).
if [ ! -f "$XFS_MNT/base/devbox-rootfs.ext4" ] && [ -f /opt/fc/devbox-rootfs.ext4 ]; then
  cp --sparse=always /opt/fc/devbox-rootfs.ext4 "$XFS_MNT/base/devbox-rootfs.ext4"
fi

#############################################
# 3. Render Nomad client config + start Nomad
#############################################
sed -i "s|__NOMAD_SERVER_IP__|${NOMAD_SERVER_IP}|g" /etc/nomad.d/client.hcl
systemctl enable --now nomad

echo "startup-worker finished OK (nomad server ${NOMAD_SERVER_IP})"
