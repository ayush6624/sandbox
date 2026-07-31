#!/usr/bin/env bash
# GCE startup-script for autoscaled Firecracker worker instances (MIG). Runs as
# root on every boot; idempotent. The image (bake-image.sh) already has
# firecracker, the guest kernel, the base rootfs at /opt/fc, and a Nomad client
# (disabled, with a client.hcl template). This script:
#   1. formats + mounts the blank per-instance XFS data disk
#   2. stages the base rootfs onto it (where configs/devbox-gcp.json expects it)
#   3. renders the Nomad client config with the control VM's IP and starts Nomad
# Nomad then places the `sandbox-serve` system job, which pulls binaries from
# GCS, bakes sandboxd into the rootfs, and runs `sandbox serve`.
# Output: /var/log/startup-script.log

meta() {
  curl -fsS -H "Metadata-Flavor: Google" \
    "http://metadata.google.internal/computeMetadata/v1/instance/attributes/$1" 2>/dev/null || true
}

fatal() {
  echo "FATAL: $*" >&2
  return 1
}

# Boot-phase instrumentation: append "<phase>\t<epoch_ms>" so `sandbox serve`
# can export the worker's readiness timeline on /metrics (see
# internal/server/bootphase.go). This is what turns the autoscale profile's
# opaque ~26s "worker becomes usable" block into per-stage numbers. tmpfs, so a
# freshly booted instance always starts with an empty timeline. Never fatal —
# diagnostics must not be able to fail a boot.
PHASE_FILE="${PHASE_FILE:-/run/sandbox/boot-phases}"
phase() {
  mkdir -p "$(dirname "$PHASE_FILE")" 2>/dev/null || return 0
  printf '%s\t%s\n' "$1" "$(date +%s%3N)" >> "$PHASE_FILE" 2>/dev/null || true
}

replace_fstab_mount() {
  local fstab_path="$1"
  local mount_path="$2"
  local fs_uuid="$3"
  local tmp

  tmp="$(mktemp "${fstab_path}.sandbox.XXXXXX")"
  if ! awk -v mount_path="$mount_path" \
    'NF < 2 || $2 != mount_path { print }' \
    "$fstab_path" > "$tmp"; then
    rm -f "$tmp"
    fatal "could not remove stale $mount_path entries from $fstab_path"
    return 1
  fi
  if ! printf 'UUID=%s %s xfs defaults,nofail 0 2\n' \
    "$fs_uuid" "$mount_path" >> "$tmp"; then
    rm -f "$tmp"
    fatal "could not write the current $mount_path entry to $fstab_path"
    return 1
  fi
  chmod 0644 "$tmp"
  mv -f "$tmp" "$fstab_path"
}

validate_data_disk_mount() {
  local dev="$1"
  local mount_path="$2"
  local expected_real
  local expected_device
  local actual_device
  local actual_source
  local actual_type
  local actual_options

  if ! mountpoint -q "$mount_path"; then
    fatal "$mount_path is not a mountpoint"
    return 1
  fi

  expected_real="$(readlink -f "$dev")"
  expected_device="$(lsblk -dnro MAJ:MIN "$expected_real" | tr -d '[:space:]')"
  actual_device="$(findmnt -nro MAJ:MIN --target "$mount_path" | tr -d '[:space:]')"
  actual_source="$(findmnt -nro SOURCE --target "$mount_path")"
  actual_type="$(findmnt -nro FSTYPE --target "$mount_path")"
  actual_options="$(findmnt -nro OPTIONS --target "$mount_path")"

  if [ -z "$expected_device" ] || [ "$actual_device" != "$expected_device" ]; then
    fatal "$mount_path is backed by ${actual_source:-unknown} (${actual_device:-unknown}), expected $expected_real (${expected_device:-unknown})"
    return 1
  fi
  if [ "$actual_type" != xfs ]; then
    fatal "$mount_path has filesystem ${actual_type:-unknown}, expected xfs"
    return 1
  fi
  case ",$actual_options," in
    *,rw,*) ;;
    *)
      fatal "$mount_path is not mounted read-write (options: ${actual_options:-unknown})"
      return 1
      ;;
  esac
}

prepare_data_disk() {
  local dev="$1"
  local mount_path="$2"
  local fstab_path="$3"
  local fs_type
  local fs_uuid
  local blkid_status

  if [ ! -b "$dev" ]; then
    fatal "$dev is not an attached block device — instance template must add the data disk"
    return 1
  fi

  blkid_status=0
  fs_type="$(blkid -s TYPE -o value "$dev" 2>/dev/null)" || blkid_status=$?
  if [ "$blkid_status" -ne 0 ] && [ "$blkid_status" -ne 2 ]; then
    fatal "could not inspect the filesystem on $dev (blkid exit $blkid_status)"
    return 1
  fi
  case "$fs_type" in
    xfs) ;;
    "")
      mkfs.xfs -f "$dev"
      ;;
    *)
      fatal "$dev contains unexpected filesystem $fs_type; refusing to format it"
      return 1
      ;;
  esac

  if ! fs_uuid="$(blkid -s UUID -o value "$dev" 2>/dev/null)" ||
    [ -z "$fs_uuid" ]; then
    fatal "$dev has no filesystem UUID after XFS preparation"
    return 1
  fi

  mkdir -p "$mount_path"
  replace_fstab_mount "$fstab_path" "$mount_path" "$fs_uuid"

  if mountpoint -q "$mount_path"; then
    validate_data_disk_mount "$dev" "$mount_path"
  else
    if ! mount -t xfs -o rw "$dev" "$mount_path"; then
      fatal "could not mount $dev explicitly at $mount_path"
      return 1
    fi
    validate_data_disk_mount "$dev" "$mount_path"
  fi

  # A golden-seeded data disk can be smaller than the instance disk. Growing is
  # safe when it is already full, but failure is an admission failure: a worker
  # must never advertise capacity with an unverified storage path.
  if ! xfs_growfs "$mount_path"; then
    fatal "xfs_growfs failed for $mount_path"
    return 1
  fi
  validate_data_disk_mount "$dev" "$mount_path"

  mkdir -p "$mount_path"/{base,rootfs,snapshots,jailer}
  chown root:root "$mount_path/jailer"
  chmod 0755 "$mount_path/jailer"
}

hold_nomad_for_admission() {
  if ! systemctl disable --now nomad; then
    fatal "could not disable and stop Nomad before worker admission"
    return 1
  fi
  if systemctl is-active --quiet nomad; then
    fatal "Nomad is still active before worker storage admission"
    return 1
  fi
}

start_nomad_after_admission() {
  if ! systemctl start nomad; then
    fatal "could not start Nomad after worker admission"
    return 1
  fi
  if ! systemctl is-active --quiet nomad; then
    fatal "Nomad did not become active after worker admission"
    return 1
  fi
}

# Unit tests source the functions without executing the GCE startup flow.
if [ "${STARTUP_WORKER_LIB_ONLY:-0}" = 1 ]; then
  return 0 2>/dev/null || exit 0
fi

set -euxo pipefail
exec > >(tee -a /var/log/startup-script.log) 2>&1

phase startup_script_entered

# Nomad is intentionally left disabled across reboots. The metadata startup
# script is the admission controller: it starts Nomad only after validating the
# exact data-disk device, filesystem, and mount. This also stops an instance
# that previously completed startup from briefly re-advertising capacity on a
# later boot before its disk has been checked.
hold_nomad_for_admission

NOMAD_SERVER_IP="$(meta nomad-server-ip)"
[ -n "$NOMAD_SERVER_IP" ] || { echo "FATAL: no nomad-server-ip metadata"; exit 1; }

#############################################
# 1. XFS data disk (reflink CoW for rootfs + snapshots)
#############################################
XFS_DEV=/dev/disk/by-id/google-sandbox-xfs
XFS_MNT=/mnt/sandbox-data
prepare_data_disk "$XFS_DEV" "$XFS_MNT" /etc/fstab
phase data_disk_ready

#############################################
# 2. Stage the base rootfs onto the data disk
#############################################
# The image baked the rootfs to the boot disk (/opt/fc); configs/devbox-gcp.json
# reads it from /mnt/sandbox-data/base. Copy once (fresh data disk each boot on
# a spot-recreated instance, so this runs on first boot of each instance).
if [ ! -f "$XFS_MNT/base/devbox-rootfs.ext4" ] && [ -f /opt/fc/devbox-rootfs.ext4 ]; then
  # --preserve=timestamps keeps the rootfs mtime stable across the copy, and the
  # .agent-stamp sidecar rides along, so the Nomad job's install-agent finds a
  # matching stamp and short-circuits BEFORE mounting — no mtime bump, which is
  # what lets a baked golden snapshot (keyed on base rootfs mtime+size) stay
  # adoptable on a fresh host. Without this the copy reset mtime + dropped the
  # stamp, forcing a full re-bake (and golden rebuild) on every boot.
  cp --sparse=always --preserve=mode,timestamps /opt/fc/devbox-rootfs.ext4 "$XFS_MNT/base/devbox-rootfs.ext4"
  if [ -f /opt/fc/devbox-rootfs.ext4.agent-stamp ]; then
    cp --preserve=mode,timestamps /opt/fc/devbox-rootfs.ext4.agent-stamp "$XFS_MNT/base/devbox-rootfs.ext4.agent-stamp"
  fi
fi
# Stamped unconditionally: on a data disk seeded from the golden image the copy
# is skipped entirely, and knowing THAT is the point (it's the ~2 GB the baked
# golden image saves).
phase rootfs_staged

#############################################
# 3. Render Nomad client config + start Nomad
#############################################
sed -i "s|__NOMAD_SERVER_IP__|${NOMAD_SERVER_IP}|g" /etc/nomad.d/client.hcl
start_nomad_after_admission
phase nomad_started

phase startup_script_done
echo "startup-worker finished OK (nomad server ${NOMAD_SERVER_IP})"
