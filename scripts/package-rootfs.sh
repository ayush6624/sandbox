#!/usr/bin/env bash
# Package the built rootfs into a compressed, sparse-aware tarball + checksum,
# ready to upload to object storage (e.g. Cloudflare R2). Run on the build host
# (Linux) — reading the base image requires root.
#
#   sudo bash scripts/package-rootfs.sh            # -> ./dist/devbox-rootfs.tar.zst (+ .sha256)
#   OUT_DIR=/tmp/out sudo bash scripts/package-rootfs.sh
#
# Restore on a fresh host (recreates the sparse holes), then bake the agent:
#   tar --sparse -I 'zstd -T0' -xf devbox-rootfs.tar.zst -C /opt/fc
#   sudo ./websandbox install-agent --agent ./sandboxd
set -euo pipefail

ASSET_DIR="${ASSET_DIR:-/opt/fc}"
ROOTFS_PATH="${ROOTFS_PATH:-${ASSET_DIR}/devbox-rootfs.ext4}"
OUT_DIR="${OUT_DIR:-./dist}"
ARTIFACT="${ARTIFACT:-devbox-rootfs.tar.zst}"

if [ ! -f "$ROOTFS_PATH" ]; then
  echo "ERROR: rootfs not found at ${ROOTFS_PATH}" >&2
  echo "  Build it first: sudo bash scripts/build-devbox-rootfs.sh" >&2
  exit 1
fi

for tool in tar zstd sha256sum; do
  if ! command -v "$tool" &>/dev/null; then
    echo "ERROR: '$tool' is not installed (try: sudo apt-get install -y $tool)" >&2
    exit 1
  fi
done

mkdir -p "$OUT_DIR"
OUT_PATH="${OUT_DIR%/}/${ARTIFACT}"

echo "==> Packaging ${ROOTFS_PATH}"
echo "  Output: ${OUT_PATH}"
echo "  (sparse-aware tar + zstd -T0 — reads the full image, ~a minute)"
# -S/--sparse preserves the holes in the 10G sparse ext4 so the archive only
# holds real content. Archive from the asset dir so it stores just the filename.
tar --sparse -I 'zstd -T0' \
  -cf "$OUT_PATH" \
  -C "$(dirname "$ROOTFS_PATH")" "$(basename "$ROOTFS_PATH")"

echo "==> Writing checksum"
( cd "$OUT_DIR" && sha256sum "$ARTIFACT" > "${ARTIFACT}.sha256" )

# Hand the artifacts back to the invoking user so they're easy to upload.
if [ -n "${SUDO_USER:-}" ]; then
  chown "$SUDO_USER":"$(id -gn "$SUDO_USER" 2>/dev/null || echo "$SUDO_USER")" \
    "$OUT_PATH" "${OUT_PATH}.sha256" 2>/dev/null || true
fi

echo ""
echo "==> Done. Upload both files to R2:"
ls -lh "$OUT_PATH" "${OUT_PATH}.sha256"
