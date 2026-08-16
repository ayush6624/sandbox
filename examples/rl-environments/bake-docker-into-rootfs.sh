#!/usr/bin/env bash
# Bake Docker (and optionally pre-pulled images) into the base rootfs, so a
# Harbor task can run its docker-compose project INSIDE a microVM.
#
# Why bake instead of apt-get at task start: installing Docker per task costs
# ~30-60 s of the task's own budget and needs egress; baked, a snapshot-restored
# guest resumes with dockerd already running and the daemon is ready in ~0 ms.
#
# Run on the sandbox host, as root:
#
#     sudo bash examples/rl-environments/bake-docker-into-rootfs.sh
#     sudo bash examples/rl-environments/bake-docker-into-rootfs.sh \
#         --pull ubuntu:24.04 --pull python:3.13-slim
#
# Then restart `serve`. This bumps the base rootfs mtime, which invalidates the
# golden snapshot by design (goldenUsable keys on base mtime+size), so the next
# server start cold-builds a fresh golden that includes Docker.
#
# On the GCP fleet the agent and base image are image-pinned, so this is a
# rebake, not a rollout:
#     infra/gcp/bake-image.sh bake && infra/gcp/bake-image.sh golden
#     # then roll the MIG
set -euo pipefail

ROOTFS="${ROOTFS:-/opt/fc/devbox-rootfs.ext4}"
MOUNT_POINT="$(mktemp -d /tmp/rootfs-docker.XXXXXX)"
PULL_IMAGES=()

while [ $# -gt 0 ]; do
  case "$1" in
    --pull) PULL_IMAGES+=("$2"); shift 2 ;;
    --rootfs) ROOTFS="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [ "$(id -u)" -ne 0 ]; then
  echo "must run as root (loop-mounts the rootfs)" >&2
  exit 1
fi
if [ ! -f "$ROOTFS" ]; then
  echo "no rootfs at $ROOTFS — run scripts/build-devbox-rootfs.sh first" >&2
  exit 1
fi
if pgrep -x firecracker >/dev/null 2>&1; then
  echo "firecracker processes are running; stop the server first (sandbox stop-server)" >&2
  exit 1
fi

cleanup() {
  # Unmount the bind mounts before the rootfs, or the rootfs stays busy.
  for target in dev/pts dev proc sys; do
    umount -l "${MOUNT_POINT}/${target}" 2>/dev/null || true
  done
  umount "$MOUNT_POINT" 2>/dev/null || true
  rmdir "$MOUNT_POINT" 2>/dev/null || true
}
trap cleanup EXIT

echo "==> mounting $ROOTFS at $MOUNT_POINT"
mount -o loop "$ROOTFS" "$MOUNT_POINT"

# A chroot needs these for apt's post-install scripts and for `docker pull`
# below (which starts a real daemon inside the chroot).
mount --bind /proc "${MOUNT_POINT}/proc"
mount --bind /sys "${MOUNT_POINT}/sys"
mount --bind /dev "${MOUNT_POINT}/dev"
mount --bind /dev/pts "${MOUNT_POINT}/dev/pts"
cp /etc/resolv.conf "${MOUNT_POINT}/etc/resolv.conf.bake"

echo "==> installing Docker into the image"
chroot "$MOUNT_POINT" /bin/bash -euo pipefail -c '
  # The guest materializes its real /etc/resolv.conf at boot; during the bake we
  # need working DNS in the chroot, so swap in the host copy and restore after.
  cp /etc/resolv.conf /etc/resolv.conf.guest 2>/dev/null || true
  cp /etc/resolv.conf.bake /etc/resolv.conf
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  # Ubuntu 24.04 archive packages: docker.io is the engine, docker-compose-v2
  # provides the `docker compose` subcommand Harbor drives. Using the archive
  # rather than download.docker.com keeps the bake independent of a third-party
  # apt repo.
  apt-get install -y docker.io docker-compose-v2 uidmap
  # The unprivileged sandbox user must be able to reach the daemon socket:
  # Harbor execs as that user, and sudo is not available non-interactively.
  usermod -aG docker sandbox
  systemctl enable docker.service containerd.service
  # Compose reads the daemon over the socket, so socket activation is fine, but
  # enabling the service means dockerd is up before the first exec lands.
  apt-get clean
  rm -rf /var/lib/apt/lists/*
'

if [ ${#PULL_IMAGES[@]} -gt 0 ]; then
  echo "==> pre-pulling ${#PULL_IMAGES[@]} image(s) into the baked layer store"
  # A task whose compose FROM is already in the guest's image store skips the
  # network entirely at task start — this is where most of a Terminal-Bench
  # task's setup time goes.
  chroot "$MOUNT_POINT" /bin/bash -euo pipefail -c "
    dockerd --iptables=false --bridge=none >/var/log/bake-dockerd.log 2>&1 &
    daemon_pid=\$!
    for _ in \$(seq 1 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
    docker info >/dev/null || { echo 'dockerd never came up in the chroot'; cat /var/log/bake-dockerd.log; exit 1; }
    for image in ${PULL_IMAGES[*]}; do
      echo \"  pulling \$image\"
      docker pull \"\$image\"
    done
    kill \$daemon_pid
    wait \$daemon_pid 2>/dev/null || true
  "
fi

echo "==> restoring guest resolv.conf"
chroot "$MOUNT_POINT" /bin/bash -c '
  if [ -f /etc/resolv.conf.guest ]; then mv /etc/resolv.conf.guest /etc/resolv.conf; fi
  rm -f /etc/resolv.conf.bake
'
# /etc/resolv.conf must stay a REAL FILE in the guest — never a symlink to
# /proc/net/pnp. c-ares (Node/undici, and therefore Claude Code) sizes the file
# before reading it, sees st_size=0 on procfs, and falls back to 127.0.0.1:53.
if [ -L "${MOUNT_POINT}/etc/resolv.conf" ]; then
  echo "resolv.conf became a symlink — refusing to ship this image" >&2
  exit 1
fi

sync
echo "==> done. Docker is baked into $ROOTFS"
echo "    Restart the server so it rebuilds the golden snapshot:"
echo "      sudo ./sandbox stop-server && sudo ./sandbox serve --config configs/devbox.json"
