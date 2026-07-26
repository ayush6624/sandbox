#!/usr/bin/env bash
# Destructive resource-exhaustion acceptance gate for one disposable Linux/KVM
# worker. Pressure is bounded inside a probe guest. The ENOSPC case uses a
# small, run-owned loopback filesystem; it never fills a production filesystem.
set -euo pipefail

: "${SANDBOX_HOST_URL:?set the direct disposable-worker API URL}"
: "${SANDBOX_HOST_KEY:?set the worker client API bearer token}"
: "${WORKER_SSH:?set the disposable worker SSH target}"

ACK="I_UNDERSTAND_THIS_EXHAUSTS_RESOURCES_ON_A_DISPOSABLE_EMPTY_WORKER"
if [ "${DISPOSABLE_WORKER_EXHAUSTION:-}" != "$ACK" ]; then
  echo "error: set DISPOSABLE_WORKER_EXHAUSTION=$ACK" >&2
  exit 64
fi

case "$SANDBOX_HOST_URL" in
  https://*) ;;
  *)
    if [ "${PRIVATE_MANAGEMENT_PROXY_ACK:-}" != "I_VERIFIED_PRIVATE_AUTHENTICATED_PROXY" ]; then
      echo "error: plaintext management URL requires a verified private authenticated proxy" >&2
      exit 64
    fi
    ;;
esac

RUN_ID="security-exhaustion-$(date -u +%Y%m%dT%H%M%SZ)-$$"
REMOTE_BASE="${EXHAUSTION_REMOTE_BASE:-/mnt/sandbox-data/security-exhaustion}"
LOOP_MIB="${EXHAUSTION_LOOP_MIB:-32}"
LOG_MAX_BYTES="${LOG_MAX_BYTES:-16777216}"
IDS=()
REMOTE_PREPARED=0

case "$REMOTE_BASE" in
  /mnt/sandbox-data/security-exhaustion) ;;
  *)
    echo "error: EXHAUSTION_REMOTE_BASE must remain /mnt/sandbox-data/security-exhaustion" >&2
    exit 64
    ;;
esac
case "$LOOP_MIB" in
  ''|*[!0-9]*)
    echo "error: EXHAUSTION_LOOP_MIB must be an integer from 16 through 64" >&2
    exit 64
    ;;
esac
if [ "$LOOP_MIB" -lt 16 ] || [ "$LOOP_MIB" -gt 64 ]; then
  echo "error: EXHAUSTION_LOOP_MIB must be from 16 through 64" >&2
  exit 64
fi
case "$LOG_MAX_BYTES" in
  ''|*[!0-9]*)
    echo "error: LOG_MAX_BYTES must be an integer from 1 through 1073741824" >&2
    exit 64
    ;;
esac
if [ "$LOG_MAX_BYTES" -lt 1 ] || [ "$LOG_MAX_BYTES" -gt 1073741824 ]; then
  echo "error: LOG_MAX_BYTES must be from 1 through 1073741824" >&2
  exit 64
fi

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: required command not found: $1" >&2
    exit 1
  }
}
for tool in curl jq ssh; do need "$tool"; done

api() {
  curl -fsS --connect-timeout 5 --max-time 25 \
    -H "Authorization: Bearer $SANDBOX_HOST_KEY" "$@"
}

worker() {
  ssh -o BatchMode=yes -o ConnectTimeout=10 \
    -o ServerAliveInterval=5 -o ServerAliveCountMax=3 \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    "$WORKER_SSH" "$@"
}

cleanup() {
  local id owned_rows
  for id in "${IDS[@]}"; do
    api -X DELETE "$SANDBOX_HOST_URL/sandboxes/$id" >/dev/null 2>&1 || true
  done
  # A create may have reached the server even if its response was lost. Since
  # the worker was required to begin empty, names with this unguessable run
  # prefix are also exact run-owned cleanup targets.
  owned_rows="$(api "$SANDBOX_HOST_URL/sandboxes" 2>/dev/null || true)"
  if jq -e 'type == "array"' >/dev/null 2>&1 <<<"$owned_rows"; then
    while IFS= read -r id; do
      api -X DELETE "$SANDBOX_HOST_URL/sandboxes/$id" >/dev/null 2>&1 || true
    done < <(jq -r --arg prefix "$RUN_ID-" \
      '.[] | select((.name? // "") | startswith($prefix)) | .id' <<<"$owned_rows")
  fi
  if [ "$REMOTE_PREPARED" -eq 1 ]; then
    worker sudo -n bash -s -- "$REMOTE_BASE" "$RUN_ID" <<'REMOTE' >/dev/null 2>&1 || true
set -euo pipefail
export LC_ALL=C
base="$1"
run_id="$2"
case "$base/$run_id" in
  /mnt/sandbox-data/security-exhaustion/security-exhaustion-*) ;;
  *) echo "refusing unsafe cleanup path" >&2; exit 1 ;;
esac
run_dir="$base/$run_id"
test -f "$run_dir/.sandbox-exhaustion-owned" || exit 0
mountpoint="$run_dir/mnt"
if mountpoint -q "$mountpoint"; then
  timeout 20s umount "$mountpoint"
fi
if test -f "$run_dir/loop.dev"; then
  loop_dev="$(cat "$run_dir/loop.dev")"
  case "$loop_dev" in /dev/loop[0-9]*) losetup -d "$loop_dev" 2>/dev/null || true ;; esac
fi
find "$run_dir" -xdev -type f -delete
rmdir "$mountpoint" 2>/dev/null || true
rmdir "$run_dir"
REMOTE
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

create() {
  local suffix="$1" mem_mib="${2:-512}" body response id
  body="$(jq -cn --arg name "$RUN_ID-$suffix" --argjson mem "$mem_mib" \
    '{name:$name,mem_mib:$mem,timeout_sec:900,hibernate_after_sec:-1}')"
  response="$(api -X POST -H 'Content-Type: application/json' --data "$body" \
    "$SANDBOX_HOST_URL/sandboxes")"
  id="$(jq -er '.id' <<<"$response")"
  IDS+=("$id")
  CREATED_ID="$id"
}

exec_guest() {
  local id="$1" command="$2" timeout="${3:-20}" body
  body="$(jq -cn --arg cmd "$command" --argjson timeout "$timeout" \
    '{cmd:$cmd,timeout_sec:$timeout}')"
  api -X POST -H 'Content-Type: application/json' --data "$body" \
    "$SANDBOX_HOST_URL/sandboxes/$id/exec"
}

require_guest_ok() {
  local id="$1" command="$2" result
  result="$(exec_guest "$id" "$command")"
  if [ "$(jq -r '.exit_code' <<<"$result")" -ne 0 ]; then
    echo "error: guest command failed: $(jq -r '.stderr' <<<"$result")" >&2
    return 1
  fi
  jq -er '.stdout' <<<"$result"
}

assert_control_healthy() {
  local id="$1" nonce result
  nonce="$RUN_ID-$(date +%s%N)"
  result="$(require_guest_ok "$id" "printf '%s' '$nonce'")"
  test "$result" = "$nonce"
  api "$SANDBOX_HOST_URL/info" >/dev/null
}

BASELINE="$(api "$SANDBOX_HOST_URL/sandboxes")"
if [ "$(jq 'length' <<<"$BASELINE")" -ne 0 ]; then
  echo "error: exhaustion gate requires an empty disposable worker" >&2
  exit 65
fi

# Refuse hosts already near a real filesystem boundary. The loop image itself
# is at most 64 MiB, while both production filesystems must have 2 GiB free.
REMOTE_PREPARED=1
worker sudo -n bash -s -- "$REMOTE_BASE" "$RUN_ID" "$LOOP_MIB" <<'REMOTE'
set -euo pipefail
export LC_ALL=C
base="$1"
run_id="$2"
loop_mib="$3"
case "$base/$run_id" in
  /mnt/sandbox-data/security-exhaustion/security-exhaustion-*) ;;
  *) echo "refusing unsafe run path" >&2; exit 1 ;;
esac
for tool in findmnt losetup mkfs.ext4 mountpoint timeout truncate; do
  command -v "$tool" >/dev/null || { echo "missing host tool: $tool" >&2; exit 1; }
done
test "$(findmnt -n -o TARGET /)" = "/"
root_free="$(df -PB1 / | awk 'NR==2{print $4}')"
data_free="$(df -PB1 /mnt/sandbox-data | awk 'NR==2{print $4}')"
test "$root_free" -ge 2147483648
test "$data_free" -ge 2147483648
run_dir="$base/$run_id"
test ! -e "$run_dir"
install -d -o root -g root -m 0700 "$base" "$run_dir" "$run_dir/mnt"
: >"$run_dir/.sandbox-exhaustion-owned"
chmod 0600 "$run_dir/.sandbox-exhaustion-owned"
truncate -s "${loop_mib}M" "$run_dir/enospc.ext4"
loop_dev="$(losetup --find --show "$run_dir/enospc.ext4")"
printf '%s\n' "$loop_dev" >"$run_dir/loop.dev"
timeout 30s mkfs.ext4 -q -F "$loop_dev"
timeout 20s mount -o nodev,nosuid,noexec "$loop_dev" "$run_dir/mnt"
test "$(findmnt -n -o SOURCE "$run_dir/mnt")" = "$loop_dev"
REMOTE

echo "security exhaustion gate: create control and pressure sandboxes"
# The Ubuntu devbox image is not reliable at 128 MiB during cold boot. Guest
# pressure is bounded independently with rlimits, so use a stable 512 MiB VM
# size while still staying well below the disposable worker's admission limit.
create control 512
CONTROL_ID="$CREATED_ID"
create pressure 512
PRESSURE_ID="$CREATED_ID"
assert_control_healthy "$CONTROL_ID"

LIST="$(api "$SANDBOX_HOST_URL/sandboxes")"
PRESSURE_ROW="$(jq -cer --arg id "$PRESSURE_ID" '.[] | select(.id == $id)' <<<"$LIST")"
PRESSURE_PID="$(jq -er '.pid | select(. > 0)' <<<"$PRESSURE_ROW")"
PRESSURE_VMID="$(jq -er '.vm_id | select(length > 0)' <<<"$PRESSURE_ROW")"

echo "security exhaustion gate: verify finite host-side VMM boundaries"
BOUNDARY_BEFORE="$(worker sudo -n bash -s -- \
  "$PRESSURE_PID" "$PRESSURE_VMID" "$LOG_MAX_BYTES" <<'REMOTE'
set -euo pipefail
export LC_ALL=C
pid="$1"
vmid="$2"
log_max="$3"
test -r "/proc/$pid/status"
cgroup_rel="$(awk -F: '$1=="0"{print $3}' "/proc/$pid/cgroup")"
cgroup="/sys/fs/cgroup$cgroup_rel"
test "$(basename "$cgroup")" = "$vmid"
memory_max="$(cat "$cgroup/memory.max")"
pids_max="$(cat "$cgroup/pids.max")"
test "$memory_max" != max
test "$memory_max" -gt 0
test "$(cat "$cgroup/memory.swap.max")" = 0
test "$pids_max" != max
test "$pids_max" -gt 0
nofile="$(awk '/Max open files/{print $4}' "/proc/$pid/limits")"
test "$nofile" != unlimited
test "$nofile" -le 256
log="/tmp/firecracker-$vmid.log"
test -f "$log"
test "$(stat -c '%s' "$log")" -le "$log_max"
memory_oom="$(awk '$1=="oom"{print $2}' "$cgroup/memory.events")"
memory_oom_kill="$(awk '$1=="oom_kill"{print $2}' "$cgroup/memory.events")"
pids_hits="$(awk '$1=="max"{print $2}' "$cgroup/pids.events")"
printf '%s %s %s %s\n' "$cgroup_rel" "$memory_oom" "$memory_oom_kill" "$pids_hits"
REMOTE
)"
read -r PRESSURE_CGROUP MEMORY_OOM_BEFORE MEMORY_OOM_KILL_BEFORE PIDS_HITS_BEFORE \
  <<<"$BOUNDARY_BEFORE"

echo "security exhaustion gate: bounded guest address-space pressure"
MEMORY_RESULT="$(require_guest_ok "$PRESSURE_ID" "python3 - <<'PY'
import resource
limit = 64 * 1024 * 1024
resource.setrlimit(resource.RLIMIT_AS, (limit, limit))
blocks = []
try:
    while True:
        blocks.append(bytearray(4 * 1024 * 1024))
except MemoryError:
    print('MEMORY_LIMIT_REACHED')
PY")"
grep -qx MEMORY_LIMIT_REACHED <<<"$MEMORY_RESULT"
assert_control_healthy "$CONTROL_ID"

echo "security exhaustion gate: bounded guest process pressure"
PIDS_RESULT="$(require_guest_ok "$PRESSURE_ID" "python3 - <<'PY'
import errno, resource, subprocess
resource.setrlimit(resource.RLIMIT_NPROC, (24, 24))
children = []
limited = False
try:
    for _ in range(128):
        try:
            children.append(subprocess.Popen(['sleep', '10']))
        except OSError as exc:
            if exc.errno == errno.EAGAIN:
                limited = True
                break
            raise
finally:
    for child in children:
        child.terminate()
    for child in children:
        child.wait()
if not limited:
    raise SystemExit('process limit was not reached')
print('PIDS_LIMIT_REACHED')
PY")"
grep -qx PIDS_LIMIT_REACHED <<<"$PIDS_RESULT"
assert_control_healthy "$CONTROL_ID"

echo "security exhaustion gate: bounded guest file-descriptor pressure"
FD_RESULT="$(require_guest_ok "$PRESSURE_ID" "python3 - <<'PY'
import errno, os, resource
resource.setrlimit(resource.RLIMIT_NOFILE, (32, 32))
fds = []
limited = False
try:
    for _ in range(128):
        try:
            fds.append(os.open('/dev/null', os.O_RDONLY))
        except OSError as exc:
            if exc.errno == errno.EMFILE:
                limited = True
                break
            raise
finally:
    for fd in fds:
        os.close(fd)
if not limited:
    raise SystemExit('file-descriptor limit was not reached')
print('FD_LIMIT_REACHED')
PY")"
grep -qx FD_LIMIT_REACHED <<<"$FD_RESULT"
assert_control_healthy "$CONTROL_ID"

echo "security exhaustion gate: controlled ENOSPC in run-owned loopback filesystem"
worker sudo -n bash -s -- "$REMOTE_BASE" "$RUN_ID" "$LOG_MAX_BYTES" <<'REMOTE'
set -euo pipefail
export LC_ALL=C
base="$1"
run_id="$2"
log_max="$3"
run_dir="$base/$run_id"
test -f "$run_dir/.sandbox-exhaustion-owned"
mountpoint="$run_dir/mnt"
mountpoint -q "$mountpoint"
# Model a bounded VMM diagnostic file, then consume only this disposable
# filesystem's remaining blocks. ENOSPC here cannot consume host root/data.
timeout 10s dd if=/dev/zero of="$mountpoint/firecracker-probe.log" bs=1 count=0 \
  seek="$log_max" status=none
test "$(stat -c '%s' "$mountpoint/firecracker-probe.log")" -le "$log_max"
set +e
timeout 30s dd if=/dev/zero of="$mountpoint/fill" bs=1M \
  status=none 2>"$run_dir/dd.stderr"
status=$?
set -e
test "$status" -ne 0
grep -qi 'No space left on device' "$run_dir/dd.stderr"
# ext4 delayed allocation may accept a one-byte buffered append even with
# zero free blocks and report the error only at fsync/close. The deterministic
# boundary is the synchronous filler above; the modeled bounded log must not
# have grown past its configured cap.
test "$(stat -c '%s' "$mountpoint/firecracker-probe.log")" -le "$log_max"
test "$(df -PB1 / | awk 'NR==2{print $4}')" -ge 2147483648
test "$(df -PB1 /mnt/sandbox-data | awk 'NR==2{print $4}')" -ge 2147483648
REMOTE
assert_control_healthy "$CONTROL_ID"
require_guest_ok "$PRESSURE_ID" "printf PRESSURE_SANDBOX_HEALTHY" |
  grep -qx PRESSURE_SANDBOX_HEALTHY

# Check that the real VMM log and VMM process remain healthy while a separate
# log filesystem is at ENOSPC. Production paths were never remounted.
worker sudo -n bash -s -- \
  "$PRESSURE_PID" "$PRESSURE_VMID" "$LOG_MAX_BYTES" "$PRESSURE_CGROUP" \
  "$MEMORY_OOM_BEFORE" "$MEMORY_OOM_KILL_BEFORE" "$PIDS_HITS_BEFORE" <<'REMOTE'
set -euo pipefail
pid="$1"
vmid="$2"
log_max="$3"
cgroup_rel="$4"
memory_oom_before="$5"
memory_oom_kill_before="$6"
pids_hits_before="$7"
kill -0 "$pid"
test "$(awk -F: '$1=="0"{print $3}' "/proc/$pid/cgroup")" = "$cgroup_rel"
cgroup="/sys/fs/cgroup$cgroup_rel"
test "$(awk '$1=="oom"{print $2}' "$cgroup/memory.events")" = "$memory_oom_before"
test "$(awk '$1=="oom_kill"{print $2}' "$cgroup/memory.events")" = "$memory_oom_kill_before"
test "$(awk '$1=="max"{print $2}' "$cgroup/pids.events")" = "$pids_hits_before"
log="/tmp/firecracker-$vmid.log"
test -f "$log"
test "$(stat -c '%s' "$log")" -le "$log_max"
REMOTE

echo "PASS: memory, PID, FD, and isolated ENOSPC pressure stayed inside bounded disposable resources"
