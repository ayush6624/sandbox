#!/usr/bin/env bash
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STARTUP_WORKER_LIB_ONLY=1
# shellcheck source=startup-worker.sh
source "$DIR/startup-worker.sh"
unset STARTUP_WORKER_LIB_ONLY

TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_eq() {
  local want="$1"
  local got="$2"
  local message="$3"
  [ "$got" = "$want" ] || fail "$message: got '$got', want '$want'"
}

test_replace_fstab_mount() {
  local fstab="$TEST_DIR/fstab"
  printf '%s\n' \
    '# system filesystems' \
    'UUID=boot / ext4 defaults 0 1' \
    'UUID=stale-one /mnt/sandbox-data xfs defaults,nofail 0 2' \
    'UUID=other /srv/other xfs defaults 0 2' \
    'UUID=stale-two   /mnt/sandbox-data   xfs   defaults,nofail   0 2' \
    > "$fstab"

  replace_fstab_mount "$fstab" /mnt/sandbox-data current-uuid
  replace_fstab_mount "$fstab" /mnt/sandbox-data current-uuid

  assert_eq 1 "$(awk '$2 == "/mnt/sandbox-data" { count++ } END { print count + 0 }' "$fstab")" \
    "fstab must contain exactly one data-disk mount entry"
  grep -qx 'UUID=current-uuid /mnt/sandbox-data xfs defaults,nofail 0 2' "$fstab" ||
    fail "fstab must contain the current data-disk UUID"
  grep -qx 'UUID=other /srv/other xfs defaults 0 2' "$fstab" ||
    fail "fstab rewrite must preserve unrelated mounts"
  if grep -q 'stale-one\|stale-two' "$fstab"; then
    fail "fstab rewrite must remove every stale data-disk UUID"
  fi
}

TEST_MOUNTED=1
TEST_EXPECTED_DEVICE=8:16
TEST_ACTUAL_DEVICE=8:16
TEST_ACTUAL_SOURCE=/dev/sdb
TEST_ACTUAL_TYPE=xfs
TEST_ACTUAL_OPTIONS=rw,relatime

mountpoint() {
  [ "$TEST_MOUNTED" = 1 ]
}

readlink() {
  printf '%s\n' /dev/sdb
}

lsblk() {
  printf '%s\n' "$TEST_EXPECTED_DEVICE"
}

findmnt() {
  case "$*" in
    *MAJ:MIN*) printf '%s\n' "$TEST_ACTUAL_DEVICE" ;;
    *SOURCE*) printf '%s\n' "$TEST_ACTUAL_SOURCE" ;;
    *FSTYPE*) printf '%s\n' "$TEST_ACTUAL_TYPE" ;;
    *OPTIONS*) printf '%s\n' "$TEST_ACTUAL_OPTIONS" ;;
    *) return 2 ;;
  esac
}

expect_mount_validation_failure() {
  local message="$1"
  if validate_data_disk_mount /dev/disk/by-id/google-sandbox-xfs /mnt/sandbox-data \
    > /dev/null 2>&1; then
    fail "$message"
  fi
}

test_validate_data_disk_mount() {
  validate_data_disk_mount /dev/disk/by-id/google-sandbox-xfs /mnt/sandbox-data

  TEST_ACTUAL_DEVICE=8:1
  expect_mount_validation_failure "boot-disk-backed mount must fail admission"
  TEST_ACTUAL_DEVICE=8:16

  TEST_ACTUAL_TYPE=ext4
  expect_mount_validation_failure "non-XFS mount must fail admission"
  TEST_ACTUAL_TYPE=xfs

  TEST_ACTUAL_OPTIONS=ro,relatime
  expect_mount_validation_failure "read-only mount must fail admission"
  TEST_ACTUAL_OPTIONS=rw,relatime

  TEST_MOUNTED=0
  expect_mount_validation_failure "plain boot-disk directory must fail admission"
  TEST_MOUNTED=1
}

SYSTEMCTL_LOG="$TEST_DIR/systemctl.log"
TEST_DISABLE_STATUS=0
TEST_START_STATUS=0
TEST_MASK_STATUS=0
TEST_ACTIVE_STATUS=1

systemctl() {
  printf '%s\n' "$*" >> "$SYSTEMCTL_LOG"
  case "$1" in
    disable) return "$TEST_DISABLE_STATUS" ;;
    start) return "$TEST_START_STATUS" ;;
    mask) return "$TEST_MASK_STATUS" ;;
    is-active) return "$TEST_ACTIVE_STATUS" ;;
    *) return 2 ;;
  esac
}

test_package_update_admission_gate() {
  : > "$SYSTEMCTL_LOG"
  TEST_ACTIVE_STATUS=1
  disable_automatic_package_updates
  grep -q '^mask --now apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service unattended-upgrades.service$' "$SYSTEMCTL_LOG" ||
    fail "startup must stop and mask every automatic package-update unit"

  TEST_MASK_STATUS=1
  if disable_automatic_package_updates > /dev/null 2>&1; then
    fail "startup must fail when package-update units cannot be masked"
  fi
  TEST_MASK_STATUS=0

  TEST_ACTIVE_STATUS=0
  if disable_automatic_package_updates > /dev/null 2>&1; then
    fail "startup must fail while any package-update unit remains active"
  fi
  TEST_ACTIVE_STATUS=1
}

test_nomad_admission_gate() {
  hold_nomad_for_admission
  grep -qx 'disable --now nomad' "$SYSTEMCTL_LOG" ||
    fail "startup must disable and stop Nomad before admission"

  TEST_ACTIVE_STATUS=0
  if hold_nomad_for_admission > /dev/null 2>&1; then
    fail "startup must fail if Nomad remains active before admission"
  fi

  TEST_ACTIVE_STATUS=0
  start_nomad_after_admission
  grep -qx 'start nomad' "$SYSTEMCTL_LOG" ||
    fail "startup must start Nomad only after admission"

  TEST_ACTIVE_STATUS=1
  if start_nomad_after_admission > /dev/null 2>&1; then
    fail "startup must fail unless Nomad becomes active"
  fi
}

test_admission_order() {
  local hold_line
  local updates_line
  local disk_line
  local start_line

  updates_line="$(awk '$0 == "disable_automatic_package_updates" { print NR }' "$DIR/startup-worker.sh")"
  hold_line="$(awk '$0 == "hold_nomad_for_admission" { print NR }' "$DIR/startup-worker.sh")"
  disk_line="$(awk '$0 == "prepare_data_disk \"$XFS_DEV\" \"$XFS_MNT\" /etc/fstab" { print NR }' "$DIR/startup-worker.sh")"
  start_line="$(awk '$0 == "start_nomad_after_admission" { print NR }' "$DIR/startup-worker.sh")"

  [ -n "$updates_line" ] && [ -n "$hold_line" ] && [ -n "$disk_line" ] && [ -n "$start_line" ] ||
    fail "startup flow must contain package-update quarantine, Nomad hold, disk admission, and Nomad start"
  [ "$updates_line" -lt "$hold_line" ] && [ "$hold_line" -lt "$disk_line" ] && [ "$disk_line" -lt "$start_line" ] ||
    fail "Nomad must remain stopped until after data-disk admission"
  if grep -q 'systemctl enable.*nomad' "$DIR/startup-worker.sh"; then
    fail "Nomad must remain disabled across reboots"
  fi
}

test_replace_fstab_mount
test_validate_data_disk_mount
test_package_update_admission_gate
test_nomad_admission_gate
test_admission_order

echo "startup-worker tests passed"
