#!/usr/bin/env bash
# Build a custom rootfs with Node.js, pnpm, TypeScript, and a Vite React template.
# Must run on Linux as root (uses debootstrap + chroot).
# Supports resuming — re-run the script and it skips completed steps.
set -euo pipefail

ASSET_DIR="${ASSET_DIR:-/opt/fc}"
ROOTFS_PATH="${ASSET_DIR}/devbox-rootfs.ext4"
ROOTFS_SIZE="${ROOTFS_SIZE:-4G}"
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

# --- Step 4: Scaffold Vite React-TS template ---
if [ -f "$BUILD_DIR/.step4-done" ]; then
  echo "==> [4/6] Vite project already scaffolded, skipping"
else
  echo ""
  echo "==> [4/6] Scaffolding Vite React-TS project..."
  sudo chroot "$BUILD_DIR" bash -c '
    mkdir -p /home/sandbox
    cd /home/sandbox
    yes | pnpm create vite app --template react-ts
    cd app
    pnpm install --frozen-lockfile 2>/dev/null || pnpm install
    echo "Installed $(ls node_modules | wc -l) packages"
  '
  sudo touch "$BUILD_DIR/.step4-done"
fi

# --- Step 5: Configure systemd service + system settings ---
if [ -f "$BUILD_DIR/.step5-done" ]; then
  echo "==> [5/6] Systemd already configured, skipping"
else
  echo ""
  echo "==> [5/6] Configuring systemd services..."

  # Vite dev server service
  sudo tee "$BUILD_DIR/etc/systemd/system/vite-dev.service" > /dev/null <<'UNIT'
[Unit]
Description=Vite Dev Server
After=multi-user.target

[Service]
Type=simple
WorkingDirectory=/home/sandbox/app
ExecStart=/usr/bin/npx vite --host 0.0.0.0
Restart=on-failure
RestartSec=2
Environment=HOME=/home/sandbox
Environment=NODE_ENV=development

[Install]
WantedBy=multi-user.target
UNIT

  sudo chroot "$BUILD_DIR" systemctl enable vite-dev.service

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
echo "Config should have:"
echo "  \"rootfs_path\": \"${ROOTFS_PATH}\""
