#!/usr/bin/env bash
# One-time per-host setup for a GCP Firecracker sandbox host. Idempotent and
# resumable — safe to re-run. Run from ~/sandbox on the host (passwordless sudo).
#
#   bash scripts/gcp-host-bootstrap.sh
#
# Does: install debootstrap -> Firecracker binary -> guest kernel -> build the
# devbox rootfs (~5 min) -> bake the sandboxd agent into it. After this, the
# host just needs `sandbox serve` (we run it via systemd).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

echo "==> [0/5] Preflight"
[ -e /dev/kvm ] || { echo "FATAL: /dev/kvm missing — nested virtualization is not enabled on this VM"; exit 1; }
echo "  /dev/kvm present, $(nproc) cores"

echo "==> [1/6] Install debootstrap (+ e2fsprogs, xfsprogs)"
if ! command -v debootstrap &>/dev/null || ! command -v mkfs.xfs &>/dev/null; then
  sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq debootstrap e2fsprogs xfsprogs
fi
echo "  debootstrap: $(command -v debootstrap)"

echo "==> [2/6] Prepare XFS data disk (reflink CoW for rootfs + snapshots)"
# The data disk (config.env DATA_DISK_SIZE, attached as device-name=sandbox-xfs)
# is formatted XFS for reflink=1 — instant copy-on-write rootfs clones. The base
# rootfs, per-sandbox copies, and snapshots all live here so cp --reflink can
# share extents (reflink needs src+dst on one filesystem). Idempotent.
XFS_DEV=/dev/disk/by-id/google-sandbox-xfs
XFS_MNT=/mnt/sandbox-data
if [ -e "$XFS_DEV" ]; then
  if ! sudo blkid "$XFS_DEV" | grep -q 'TYPE="xfs"'; then
    echo "  formatting $XFS_DEV as XFS (reflink=1 default)"
    sudo mkfs.xfs -f "$XFS_DEV"
  fi
  sudo mkdir -p "$XFS_MNT"
  XFS_UUID=$(sudo blkid -s UUID -o value "$XFS_DEV")
  grep -q "$XFS_UUID" /etc/fstab || \
    echo "UUID=$XFS_UUID $XFS_MNT xfs defaults,nofail 0 2" | sudo tee -a /etc/fstab >/dev/null
  mountpoint -q "$XFS_MNT" || sudo mount "$XFS_MNT"
  sudo mkdir -p "$XFS_MNT"/{base,rootfs,snapshots}
  BASE_ASSET_DIR="$XFS_MNT/base"
  echo "  XFS mounted at $XFS_MNT ($(sudo xfs_info "$XFS_MNT" | grep -o 'reflink=[01]'))"
else
  BASE_ASSET_DIR=/opt/fc
  echo "  WARNING: $XFS_DEV not found — no data disk attached. Falling back to"
  echo "  the boot disk (ext4, NO reflink CoW); snapshot fan-out will be slow."
  echo "  NOTE: configs/devbox-gcp.json points rootfs_base at $XFS_MNT/base —"
  echo "  attach the data disk and re-run, or adjust the config to match."
fi

echo "==> [3/6] Firecracker binary"
sudo bash scripts/setup-firecracker.sh

echo "==> [4/6] Guest kernel"
sudo bash scripts/setup-kernel.sh

echo "==> [5/6] Build devbox rootfs (~5 min, resumable) into $BASE_ASSET_DIR"
sudo ASSET_DIR="$BASE_ASSET_DIR" bash scripts/build-devbox-rootfs.sh

echo "==> [6/6] Bake sandboxd agent into the rootfs"
# Image-bake (bake-image.sh) sets SKIP_INSTALL_AGENT=1: there's no data disk at
# bake time so the config's rootfs_base path doesn't exist, and workers bake a
# fresh sandboxd from GCS at Nomad job start anyway.
if [ "${SKIP_INSTALL_AGENT:-0}" = "1" ]; then
  echo "  SKIP_INSTALL_AGENT=1 — leaving agent install to job start"
else
  sudo ./sandbox install-agent --config configs/devbox-gcp.json --agent ./sandboxd
fi

echo "==> host bootstrap complete"
