#!/usr/bin/env bash
# Fetch a prebuilt rootfs tarball from a URL and turn it into a ready-to-serve
# base image: download -> verify -> sparse-extract into /opt/fc -> bake the
# sandboxd agent in. No local rebuild. Run on the target host (Linux) as root,
# from the repo root (so ./sandbox and ./sandboxd resolve).
#
#   sudo bash scripts/fetch-rootfs.sh                   # pulls the published image
#   sudo bash scripts/fetch-rootfs.sh https://<bucket>/devbox-rootfs.tar.zst
#   FORCE=1 sudo bash scripts/fetch-rootfs.sh           # overwrite an existing image
#
# Env overrides:
#   AGENT=./sandboxd              sandboxd binary to bake in
#   SANDBOX=./sandbox       sandbox binary used for install-agent
#   CONFIG=configs/devbox.json    config (resolves rootfs_base for install-agent)
#   ASSET_DIR=/opt/fc             where the image is extracted
set -euo pipefail

ROOTFS_URL="${ROOTFS_URL:-${1:-https://sandbox.ayushgoyal.dev/images/devbox-rootfs.tar.zst}}"
ASSET_DIR="${ASSET_DIR:-/opt/fc}"
ROOTFS_PATH="${ROOTFS_PATH:-${ASSET_DIR}/devbox-rootfs.ext4}"
AGENT="${AGENT:-./sandboxd}"
SANDBOX="${SANDBOX:-./sandbox}"
CONFIG="${CONFIG:-configs/devbox.json}"

if [ -z "$ROOTFS_URL" ]; then
  echo "Usage: sudo bash scripts/fetch-rootfs.sh <url-to-devbox-rootfs.tar.zst>" >&2
  exit 1
fi

for tool in curl tar zstd sha256sum; do
  if ! command -v "$tool" &>/dev/null; then
    echo "ERROR: '$tool' is not installed (try: sudo apt-get install -y $tool)" >&2
    exit 1
  fi
done

if [ -f "$ROOTFS_PATH" ] && [ "${FORCE:-0}" != "1" ]; then
  echo "ERROR: ${ROOTFS_PATH} already exists. Re-run with FORCE=1 to overwrite." >&2
  exit 1
fi

if [ ! -f "$AGENT" ]; then
  echo "ERROR: agent binary not found at ${AGENT} (set AGENT=/path/to/sandboxd, or run 'make sync' first)" >&2
  exit 1
fi
if [ ! -f "$SANDBOX" ]; then
  echo "ERROR: sandbox binary not found at ${SANDBOX} (set SANDBOX=/path, or run 'make sync' first)" >&2
  exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
ARCHIVE="${TMP}/rootfs.tar.zst"

echo "==> Downloading ${ROOTFS_URL}"
curl -fL --retry 3 -o "$ARCHIVE" "$ROOTFS_URL"

echo "==> Verifying checksum"
if curl -fsL -o "${ARCHIVE}.sha256" "${ROOTFS_URL}.sha256" 2>/dev/null; then
  expected="$(awk '{print $1}' "${ARCHIVE}.sha256")"
  actual="$(sha256sum "$ARCHIVE" | awk '{print $1}')"
  if [ "$expected" != "$actual" ]; then
    echo "ERROR: checksum mismatch — refusing to use this download" >&2
    echo "  expected: $expected" >&2
    echo "  actual:   $actual" >&2
    exit 1
  fi
  echo "  ok ($actual)"
else
  echo "  WARNING: no ${ROOTFS_URL}.sha256 found — skipping verification" >&2
fi

echo "==> Extracting into ${ASSET_DIR}"
mkdir -p "$ASSET_DIR"
rm -f "$ROOTFS_PATH"
tar --sparse -I 'zstd -T0' -xf "$ARCHIVE" -C "$ASSET_DIR"

if [ ! -f "$ROOTFS_PATH" ]; then
  echo "ERROR: archive did not contain $(basename "$ROOTFS_PATH") under ${ASSET_DIR}" >&2
  echo "  archive contents:" >&2
  tar --sparse -I 'zstd -T0' -tf "$ARCHIVE" >&2
  exit 1
fi

echo "==> Baking the sandboxd agent in"
"$SANDBOX" install-agent --config "$CONFIG" --agent "$AGENT"

echo ""
echo "==> Ready: ${ROOTFS_PATH}"
ls -lh "$ROOTFS_PATH"
echo ""
echo "Start the server: sudo ${SANDBOX} serve --config ${CONFIG}"
