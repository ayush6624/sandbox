#!/usr/bin/env bash
# Build a custom rootfs with Node.js, Python, common build tooling, popular CLI
# utilities (htop, jq, ripgrep, gh, …), and AI coding agents (Claude Code, Codex).
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

# --- Step 3: Install pnpm, TypeScript + AI coding agents ---
if [ -f "$BUILD_DIR/.step3-done" ]; then
  echo "==> [3/6] pnpm + TypeScript + AI CLIs already installed, skipping"
else
  echo ""
  echo "==> [3/6] Installing pnpm, TypeScript, Claude Code, Codex..."
  # Claude Code (@anthropic-ai/claude-code) and OpenAI Codex (@openai/codex) are
  # global npm CLIs. Users supply their own API keys/logins at runtime — nothing
  # secret is baked in. Codex ships a per-platform native binary via npm optional
  # deps; the linux/amd64 build host resolves the matching linux-x64 package.
  sudo chroot "$BUILD_DIR" bash -c '
    npm install -g pnpm typescript @anthropic-ai/claude-code @openai/codex
    pnpm --version
    tsc --version
    claude --version || true
    codex --version || true
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
    apt-get install -y \
      python3 python3-pip python3-venv build-essential git make openssh-server \
      htop vim nano tmux jq ripgrep fd-find bat tree ncdu wget unzip zip less rsync man-db

    # GitHub CLI from the official apt repo (not in the Ubuntu archive). The
    # keyring is a plain .gpg referenced via signed-by, so no gpg import needed.
    mkdir -p -m 755 /etc/apt/keyrings
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /etc/apt/keyrings/githubcli-archive-keyring.gpg
    chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list
    apt-get update
    apt-get install -y gh

    # fd/bat ship as fdfind/batcat on Debian/Ubuntu to avoid name clashes; add
    # the conventional command names.
    ln -sf /usr/bin/fdfind /usr/local/bin/fd
    ln -sf /usr/bin/batcat /usr/local/bin/bat

    python3 --version
    pip3 --version
    gcc --version | head -1
    git --version
    gh --version | head -1
  '

  # --- SSH server ---
  # Key-only root login. The host installs each sandbox's authorized_keys at
  # create (POST /sandboxes ssh_pubkey → sandboxd /ssh-key → /root/.ssh), and
  # the userspace port proxy forwards a host port to guest :22 on demand
  # (wake-on-connect included). Config drop-in overrides the distro default.
  sudo tee "$BUILD_DIR/etc/ssh/sshd_config.d/sandbox.conf" > /dev/null <<'SSHD'
# Managed by build-devbox-rootfs.sh — sandbox SSH access.
PermitRootLogin prohibit-password
PubkeyAuthentication yes
PasswordAuthentication no
SSHD
  sudo chroot "$BUILD_DIR" bash -c '
    export DEBIAN_FRONTEND=noninteractive
    # Host keys are not generated in the chroot by the postinst; do it now so
    # sshd can start on first boot. NB these are baked into the golden image, so
    # every clone shares host keys — fine for ephemeral dev sandboxes (each is
    # reached on a distinct host:port, so known_hosts entries do not collide).
    ssh-keygen -A
    # Ubuntu 24.04 ships ssh via socket activation; use the always-on service so
    # the port is listening the instant the guest boots (the host may dial :22
    # immediately after the readiness gate).
    systemctl disable ssh.socket 2>/dev/null || true
    systemctl enable ssh.service 2>/dev/null || true
  '
  # Create the working directory used by exec/files (HOME=/home/sandbox,
  # default exec cwd /home/sandbox/app — sandboxd falls back to / without it).
  sudo chroot "$BUILD_DIR" bash -c 'mkdir -p /home/sandbox/app'

  # Shell rc files for /home/sandbox (the HOME of sandboxd's interactive
  # shells) so `bash -l` gets a color prompt + color aliases. install-agent
  # rewrites these on every run; keep the content in sync with installagent.go.
  sudo tee "$BUILD_DIR/home/sandbox/.profile" > /dev/null <<'PROFILE'
# ~/.profile: sourced by login shells (sandboxd's /shell runs bash -l).
if [ -n "$BASH" ] && [ -f "$HOME/.bashrc" ]; then
	. "$HOME/.bashrc"
fi
PROFILE
  sudo tee "$BUILD_DIR/home/sandbox/.bashrc" > /dev/null <<'BASHRC'
# ~/.bashrc for sandbox shells — enable colors (baked by install-agent).
case $- in *i*) ;; *) return ;; esac

eval "$(dircolors -b 2>/dev/null)"
alias ls='ls --color=auto'
alias ll='ls --color=auto -al'
alias grep='grep --color=auto'
alias fgrep='fgrep --color=auto'
alias egrep='egrep --color=auto'
alias diff='diff --color=auto'
alias ip='ip -color=auto'

PS1='\[\e[1;32m\]\u@\h\[\e[0m\]:\[\e[1;34m\]\w\[\e[0m\]\$ '
BASHRC

  sudo touch "$BUILD_DIR/.step4-done"
fi

# --- Step 5: Configure system settings (no app server — bare sandbox) ---
if [ -f "$BUILD_DIR/.step5-done" ]; then
  echo "==> [5/6] System already configured, skipping"
else
  echo ""
  echo "==> [5/6] Configuring system settings..."

  # DNS — the Firecracker SDK passes the config's nameservers
  # (IPConfiguration.Nameservers, from configs/*.json) to the guest *only* via
  # the kernel `ip=` boot param, which the guest kernel exposes at
  # /proc/net/pnp in resolv.conf format. glibc reads /etc/resolv.conf and never
  # /proc/net/pnp, so symlink the two. This honors whatever nameservers the
  # config sets instead of hardcoding them. See firecracker-go-sdk network.go
  # (IPConfiguration) and cni/vmconf (IPBootParam) for the contract.
  sudo chroot "$BUILD_DIR" bash -c 'rm -f /etc/resolv.conf && ln -s /proc/net/pnp /etc/resolv.conf'

  # systemd-resolved/networkd would clobber that symlink and fight the kernel
  # ip= config, so keep them masked.
  sudo chroot "$BUILD_DIR" bash -c '
    systemctl disable systemd-networkd.service 2>/dev/null || true
    systemctl disable systemd-resolved.service 2>/dev/null || true
    systemctl mask systemd-networkd.service 2>/dev/null || true
    systemctl mask systemd-resolved.service 2>/dev/null || true
  '

  # Stable hostname + matching /etc/hosts so `sudo` and other tools that look
  # up the local hostname don't warn "unable to resolve host".
  echo "sandbox" | sudo tee "$BUILD_DIR/etc/hostname" > /dev/null
  sudo tee "$BUILD_DIR/etc/hosts" > /dev/null <<'HOSTS'
127.0.0.1   localhost sandbox
::1         localhost ip6-localhost ip6-loopback
HOSTS

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
echo "Don't forget to bake in the agent: sudo ./sandbox install-agent --agent ./sandboxd"
echo "Config should have:"
echo "  \"rootfs_path\": \"${ROOTFS_PATH}\""
