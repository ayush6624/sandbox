#!/usr/bin/env bash
# Download a Firecracker-compatible kernel to /opt/fc/vmlinux.
set -euo pipefail

ASSET_DIR="${ASSET_DIR:-/opt/fc}"
ARCH="$(uname -m)"

# Resolve the latest Firecracker release to determine CI asset prefix.
RELEASE_URL="https://github.com/firecracker-microvm/firecracker/releases"
LATEST_VERSION=$(basename "$(curl -fsSLI -o /dev/null -w %{url_effective} "${RELEASE_URL}/latest")")
CI_VERSION="${LATEST_VERSION%.*}"

echo "==> Firecracker ${LATEST_VERSION} (CI prefix: ${CI_VERSION}, arch: ${ARCH})"
echo "==> Installing kernel to ${ASSET_DIR}"
sudo mkdir -p "$ASSET_DIR"

if [ -f "$ASSET_DIR/vmlinux" ]; then
  echo "  Kernel already exists, skipping"
  exit 0
fi

echo "  Finding latest kernel..."
KERNEL_KEY=$(curl -s "http://spec.ccfc.min.s3.amazonaws.com/?prefix=firecracker-ci/${CI_VERSION}/${ARCH}/vmlinux-&list-type=2" \
  | grep -oP "(?<=<Key>)(firecracker-ci/${CI_VERSION}/${ARCH}/vmlinux-[0-9]+\.[0-9]+\.[0-9]{1,3})(?=</Key>)" \
  | sort -V | tail -1)

if [ -z "$KERNEL_KEY" ]; then
  echo "ERROR: could not find kernel in S3 bucket"
  exit 1
fi

echo "  Downloading kernel: ${KERNEL_KEY}"
sudo curl -fSL "https://s3.amazonaws.com/spec.ccfc.min/${KERNEL_KEY}" -o "$ASSET_DIR/vmlinux"

echo "  Kernel installed:"
ls -lh "$ASSET_DIR/vmlinux"
