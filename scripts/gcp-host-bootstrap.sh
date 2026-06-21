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

echo "==> [1/5] Install debootstrap (+ e2fsprogs)"
if ! command -v debootstrap &>/dev/null; then
  sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq debootstrap e2fsprogs
fi
echo "  debootstrap: $(command -v debootstrap)"

echo "==> [2/5] Firecracker binary"
sudo bash scripts/setup-firecracker.sh

echo "==> [3/5] Guest kernel"
sudo bash scripts/setup-kernel.sh

echo "==> [4/5] Build devbox rootfs (~5 min, resumable)"
sudo bash scripts/build-devbox-rootfs.sh

echo "==> [5/5] Bake sandboxd agent into the rootfs"
sudo ./sandbox install-agent --agent ./sandboxd

echo "==> host bootstrap complete"
