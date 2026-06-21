#!/usr/bin/env bash
# Download a Firecracker-compatible guest kernel to /opt/fc/vmlinux.
set -euo pipefail

ASSET_DIR="${ASSET_DIR:-/opt/fc}"
ARCH="$(uname -m)"
S3="https://s3.amazonaws.com/spec.ccfc.min"

sudo mkdir -p "$ASSET_DIR"
if [ -f "$ASSET_DIR/vmlinux" ]; then
  echo "==> Kernel already exists at $ASSET_DIR/vmlinux, skipping"
  exit 0
fi

# Firecracker's CI publishes guest kernels under date-stamped prefixes:
#   firecracker-ci/YYYYMMDD-<sha>-N/<arch>/vmlinux-X.Y.Z
# (per the official getting-started guide). We pick the newest such prefix.
# NOTE: the old version-series layout (firecracker-ci/v1.15/...) still exists but
# stops updating, and the *latest* release's version prefix is often empty — so
# keying off the GitHub release version is unreliable. Discover the date prefix
# instead. `|| true` keeps an empty grep (pipefail) from tripping set -e.
echo "==> Finding latest CI kernel (arch: ${ARCH})"
PREFIX=$(curl -fsSL --max-time 30 "$S3?list-type=2&prefix=firecracker-ci/&delimiter=/" \
  | grep -oP "(?<=<Prefix>)firecracker-ci/[0-9]{8}-[^/]+/(?=</Prefix>)" \
  | sort | tail -1 || true)

KERNEL_KEY=""
if [ -n "$PREFIX" ]; then
  echo "  CI artifact set: ${PREFIX}"
  KERNEL_KEY=$(curl -fsSL --max-time 30 "$S3?list-type=2&prefix=${PREFIX}${ARCH}/vmlinux-" \
    | grep -oP "(?<=<Key>)(${PREFIX}${ARCH}/vmlinux-[0-9]+\.[0-9]+\.[0-9]{1,3})(?=</Key>)" \
    | sort -V | tail -1 || true)
fi

if [ -z "$KERNEL_KEY" ]; then
  echo "ERROR: could not find a CI kernel under firecracker-ci/<date>/${ARCH}/"
  exit 1
fi

echo "  Downloading kernel: ${KERNEL_KEY}"
sudo curl -fSL "$S3/${KERNEL_KEY}" -o "$ASSET_DIR/vmlinux"

echo "  Kernel installed:"
ls -lh "$ASSET_DIR/vmlinux"
