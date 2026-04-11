#!/usr/bin/env bash
# Install Firecracker binary on the host.
set -euo pipefail

FC_VERSION="${FC_VERSION:-v1.15.0}"
ARCH="$(uname -m)"

echo "==> Installing Firecracker ${FC_VERSION} (${ARCH})"

if command -v firecracker &>/dev/null; then
  echo "  Firecracker already installed: $(firecracker --version 2>&1 | head -1)"
  exit 0
fi

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

RELEASE_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${ARCH}.tgz"
echo "  Downloading ${RELEASE_URL}"
curl -fSL "$RELEASE_URL" -o "$TMPDIR/firecracker.tgz"

tar xzf "$TMPDIR/firecracker.tgz" -C "$TMPDIR"
sudo install -o root -g root -m 0755 "$TMPDIR/release-${FC_VERSION}-${ARCH}/firecracker-${FC_VERSION}-${ARCH}" /usr/local/bin/firecracker
sudo install -o root -g root -m 0755 "$TMPDIR/release-${FC_VERSION}-${ARCH}/jailer-${FC_VERSION}-${ARCH}" /usr/local/bin/jailer

echo "  Installed: $(firecracker --version 2>&1 | head -1)"
