#!/usr/bin/env bash
# Build a custom rootfs with Node.js, Python, and common build tooling.
# A bare sandbox — no app server is started on boot; use exec/files (sandboxd)
# to run whatever you like. Must run on Linux as root (uses debootstrap + chroot).
# Supports resuming — re-run the script and it skips completed steps.
set -euo pipefail

ASSET_DIR="${ASSET_DIR:-/opt/fc}"
ROOTFS_PATH="${ASSET_DIR}/devbox-rootfs.ext4"
ROOTFS_SIZE="${ROOTFS_SIZE:-10G}"
BUILD_DIR="/opt/fc/rootfs-build"

if [ -f "$ROOTFS_PATH" ]; then
  echo "==> Rootfs already exists at ${ROOTFS_PATH}"
  echo "  Delete it first if you want to rebuild: sudo rm ${ROOTFS_PATH}"
  exit 0
fi

# --- Pre-check: debootstrap must be installed ---
if ! command -v debootstrap &>/dev/null; then
  echo "ERROR: debootstrap is not installed."
  echo "  Install it with: sudo apt-get update && sudo apt-get install -y debootstrap"
  exit 1
fi

echo "==> Building devbox rootfs (${ROOTFS_SIZE})"
echo "  Build dir: ${BUILD_DIR}"
echo "  Output: ${ROOTFS_PATH}"
sudo mkdir -p "$ASSET_DIR"
sudo mkdir -p "$BUILD_DIR"

# --- Step 1: Debootstrap base Ubuntu ---
if [ -f "$BUILD_DIR/.step1-done" ]; then
  echo "==> [1/6] Debootstrap already done, skipping"
else
  echo ""
  echo "==> [1/6] Debootstrap Ubuntu 24.04 (noble)..."
  sudo debootstrap \
    --components=main,universe \
    --include=systemd,systemd-sysv,curl,ca-certificates,iproute2,dbus \
    noble "$BUILD_DIR" http://archive.ubuntu.com/ubuntu
  sudo touch "$BUILD_DIR/.step1-done"
fi

# --- Step 2: Install Node.js ---
if [ -f "$BUILD_DIR/.step2-done" ]; then
  echo "==> [2/6] Node.js already installed, skipping"
else
  echo ""
  echo "==> [2/6] Installing Node.js 22 LTS..."
  sudo chroot "$BUILD_DIR" bash -c '
    curl -fsSL https://deb.nodesource.com/setup_22.x | bash -
    apt-get install -y nodejs
    node --version
    npm --version
  '
  sudo touch "$BUILD_DIR/.step2-done"
fi

# --- Step 3: Install pnpm + TypeScript ---
if [ -f "$BUILD_DIR/.step3-done" ]; then
  echo "==> [3/6] pnpm + TypeScript already installed, skipping"
else
  echo ""
  echo "==> [3/6] Installing pnpm and TypeScript..."
  sudo chroot "$BUILD_DIR" bash -c '
    npm install -g pnpm typescript
    pnpm --version
    tsc --version
  '
  sudo touch "$BUILD_DIR/.step3-done"
fi

# --- Step 4: Install Python + build tooling ---
if [ -f "$BUILD_DIR/.step4-done" ]; then
  echo "==> [4/6] Python + build tooling already installed, skipping"
else
  echo ""
  echo "==> [4/6] Installing Python 3, pip, venv, and build tooling..."

  # debootstrap only enables "main"; python3-pip/venv live in "universe".
  # Write a full sources.list (main + universe, with the updates/security
  # pockets) so the package set — and the resulting image — has them.
  sudo tee "$BUILD_DIR/etc/apt/sources.list" > /dev/null <<'APT'
deb http://archive.ubuntu.com/ubuntu noble main restricted universe multiverse
deb http://archive.ubuntu.com/ubuntu noble-updates main restricted universe multiverse
deb http://security.ubuntu.com/ubuntu noble-security main restricted universe multiverse
APT

  sudo chroot "$BUILD_DIR" bash -c '
    export DEBIAN_FRONTEND=noninteractive
    apt-get update
    apt-get install -y python3 python3-pip python3-venv build-essential git make
    python3 --version
    pip3 --version
    gcc --version | head -1
    git --version
  '
  # Create the working directory used by exec/files (HOME=/home/sandbox).
  sudo chroot "$BUILD_DIR" bash -c 'mkdir -p /home/sandbox'
  sudo touch "$BUILD_DIR/.step4-done"
fi

# --- Step 5: Configure system settings (no app server — bare sandbox) ---
if [ -f "$BUILD_DIR/.step5-done" ]; then
  echo "==> [5/6] System already configured, skipping"
else
  echo ""
  echo "==> [5/6] Configuring system settings..."

  # DNS — write a static resolv.conf (kernel ip= param doesn't always populate /etc/resolv.conf)
  sudo tee "$BUILD_DIR/etc/resolv.conf" > /dev/null <<'DNS'
nameserver 8.8.8.8
nameserver 8.8.4.4
DNS

  # Disable services that fight with kernel ip= boot param
  sudo chroot "$BUILD_DIR" bash -c '
    systemctl disable systemd-networkd.service 2>/dev/null || true
    systemctl disable systemd-resolved.service 2>/dev/null || true
    systemctl mask systemd-networkd.service 2>/dev/null || true
    systemctl mask systemd-resolved.service 2>/dev/null || true
  '

  # Set root password for serial console debugging
  echo "root:devbox" | sudo chroot "$BUILD_DIR" chpasswd

  # Enable serial console login
  sudo chroot "$BUILD_DIR" systemctl enable serial-getty@ttyS0.service 2>/dev/null || true

  sudo touch "$BUILD_DIR/.step5-done"
fi

# --- Step 6: Clean up and build ext4 image ---
echo ""
echo "==> [6/6] Building ext4 image (${ROOTFS_SIZE})..."
sudo chroot "$BUILD_DIR" bash -c '
  apt-get clean
  rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*
'

sudo truncate -s "$ROOTFS_SIZE" "$ROOTFS_PATH"
sudo mkfs.ext4 -d "$BUILD_DIR" -F "$ROOTFS_PATH"

# Clean up build dir after successful image creation
sudo rm -rf "$BUILD_DIR"

echo ""
echo "==> Devbox rootfs ready!"
ls -lh "$ROOTFS_PATH"
echo ""
echo "Don't forget to bake in the agent: sudo ./websandbox install-agent --agent ./sandboxd"
echo "Config should have:"
echo "  \"rootfs_path\": \"${ROOTFS_PATH}\""
