#!/usr/bin/env bash
# Fail-closed production-host security gate. It creates four short-lived
# sandboxes on one Linux/KVM worker (two independent creates and two
# snapshot-source batch creates), verifies the live guest/host boundaries, and
# injects one unexpected VMM crash. It never restarts or reboots the worker.
#
# Required:
#   SANDBOX_HOST_URL  Direct worker API, e.g. https://10.160.0.119:8080
#   SANDBOX_HOST_KEY  Client API bearer token
#   WORKER_SSH        SSH target for the same worker, e.g. ayush@10.160.0.119
#   WORKER_API_HOST   Worker hostname/IP used to reach allocated port forwards
#
# Optional:
#   JAILER_BASE       Worker jailer base (default /mnt/sandbox-data/jailer)
#   LOG_MAX_BYTES     Expected per-VMM log limit (default 16777216)
#   POLL_TIMEOUT_SEC  Lifecycle/reconciliation timeout (default 120)
#   CURL_CA_BUNDLE    CA bundle used by curl for a private worker certificate
set -euo pipefail

: "${SANDBOX_HOST_URL:?set a direct worker API URL}"
: "${SANDBOX_HOST_KEY:?set the worker client API bearer token}"
: "${WORKER_SSH:?set the SSH target for that worker}"
: "${WORKER_API_HOST:?set the worker hostname or IP used for port forwards}"

JAILER_BASE="${JAILER_BASE:-/mnt/sandbox-data/jailer}"
LOG_MAX_BYTES="${LOG_MAX_BYTES:-16777216}"
POLL_TIMEOUT_SEC="${POLL_TIMEOUT_SEC:-120}"
RUN_ID="security-$(date -u +%Y%m%dT%H%M%SZ)-$$"
IDS=()
SNAPSHOT_ID=""
SSH_KEY_DIR=""

case "$SANDBOX_HOST_URL" in
  https://*) ;;
  *)
    if [ "${PRIVATE_MANAGEMENT_PROXY_ACK:-}" != "I_VERIFIED_PRIVATE_AUTHENTICATED_PROXY" ]; then
      echo "error: plaintext management URL requires a verified private authenticated proxy" >&2
      echo "set PRIVATE_MANAGEMENT_PROXY_ACK=I_VERIFIED_PRIVATE_AUTHENTICATED_PROXY only after verifying that boundary" >&2
      exit 64
    fi
    ;;
esac

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: required command not found: $1" >&2
    exit 1
  }
}
for tool in curl jq ssh ssh-keygen; do need "$tool"; done

api() {
  curl -fsS -H "Authorization: Bearer $SANDBOX_HOST_KEY" "$@"
}

cleanup() {
  local id
  for id in "${IDS[@]}"; do
    api -X DELETE "$SANDBOX_HOST_URL/sandboxes/$id" >/dev/null 2>&1 || true
  done
  if [ -n "$SNAPSHOT_ID" ]; then
    api -X DELETE "$SANDBOX_HOST_URL/v1/snapshots/$SNAPSHOT_ID" \
      -H "Idempotency-Key: $RUN_ID-cleanup-snapshot" >/dev/null 2>&1 || true
  fi
  if [ -n "$SSH_KEY_DIR" ]; then
    rm -f "$SSH_KEY_DIR/id_ed25519" "$SSH_KEY_DIR/id_ed25519.pub"
    rmdir "$SSH_KEY_DIR" 2>/dev/null || true
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

worker() {
  ssh -o BatchMode=yes -o ConnectTimeout=10 \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    "$WORKER_SSH" "$@"
}

create() {
  local suffix="$1" ssh_pubkey="${2:-}" body id
  body="$(jq -cn --arg name "$RUN_ID-$suffix" --arg key "$ssh_pubkey" \
    '{name:$name,timeout_sec:600,hibernate_after_sec:-1} +
     (if $key == "" then {} else {ssh_pubkey:$key} end)')"
  CREATED_RESPONSE="$(api -X POST -H 'Content-Type: application/json' \
    --data "$body" "$SANDBOX_HOST_URL/sandboxes")"
  id="$(jq -er '.id' <<<"$CREATED_RESPONSE")"
  IDS+=("$id")
}

exec_guest() {
  local id="$1" cmd="$2" body
  body="$(jq -cn --arg cmd "$cmd" '{cmd:$cmd,timeout_sec:20}')"
  api -X POST -H 'Content-Type: application/json' --data "$body" \
    "$SANDBOX_HOST_URL/sandboxes/$id/exec"
}

guest_stdout() {
  local id="$1" cmd="$2" result
  result="$(exec_guest "$id" "$cmd")"
  if [ "$(jq -r '.exit_code' <<<"$result")" -ne 0 ]; then
    echo "error: guest command failed in $id: $(jq -r '.stderr' <<<"$result")" >&2
    return 1
  fi
  jq -r '.stdout' <<<"$result"
}

wait_operation() {
  local operation="$1" deadline now
  deadline=$(( $(date +%s) + POLL_TIMEOUT_SEC ))
  while :; do
    operation="$(api "$SANDBOX_HOST_URL/v1/operations/$(jq -er '.id' <<<"$operation")")"
    if [ "$(jq -r '.completed_at // empty' <<<"$operation")" != "" ]; then
      jq -e '.failed == 0 and .succeeded == .requested' >/dev/null <<<"$operation" || {
        echo "error: snapshot batch create failed: $operation" >&2
        return 1
      }
      printf '%s\n' "$operation"
      return
    fi
    now="$(date +%s)"
    if [ "$now" -ge "$deadline" ]; then
      echo "error: snapshot batch create did not complete within ${POLL_TIMEOUT_SEC}s" >&2
      return 1
    fi
    sleep 1
  done
}

wait_absent() {
  local id="$1" deadline now
  deadline=$(( $(date +%s) + POLL_TIMEOUT_SEC ))
  while :; do
    if ! api "$SANDBOX_HOST_URL/sandboxes" |
      jq -e --arg id "$id" '.[] | select(.id == $id)' >/dev/null; then
      return
    fi
    now="$(date +%s)"
    if [ "$now" -ge "$deadline" ]; then
      echo "error: crashed sandbox $id still exists after ${POLL_TIMEOUT_SEC}s" >&2
      return 1
    fi
    sleep 1
  done
}

refresh_rows() {
  local list id
  list="$(api "$SANDBOX_HOST_URL/sandboxes")"
  VM_ARGS=()
  for id in "${IDS[@]}"; do
    local row pid vmid
    row="$(jq -cer --arg id "$id" '.[] | select(.id == $id)' <<<"$list")"
    pid="$(jq -er '.pid | select(. > 0)' <<<"$row")"
    vmid="$(jq -er '.vm_id | select(length > 0)' <<<"$row")"
    VM_ARGS+=("$pid" "$vmid")
  done
}

inspect_host_isolation() {
  worker sudo -n bash -s -- "$LOG_MAX_BYTES" "$JAILER_BASE" "${VM_ARGS[@]}" <<'REMOTE'
set -euo pipefail
max_bytes="$1"
jailer_base="$2"
shift 2
test "$#" -ge 4
test $(( $# % 2 )) -eq 0
command -v setpriv >/dev/null
test "$(stat -c '%u' "$jailer_base")" = "0"
case "$(stat -c '%a' "$jailer_base")" in
  ??[2367]) echo "error: jailer base is writable by untrusted users" >&2; exit 1 ;;
esac

host_pid_ns="$(readlink /proc/1/ns/pid)"
host_mnt_ns="$(readlink /proc/1/ns/mnt)"
uids=""
gids=""
pids=""
pid_namespaces=""
mount_namespaces=""
jails=""
while [ "$#" -gt 0 ]; do
  pid="$1"
  vmid="$2"
  shift 2
  test -r "/proc/$pid/status"

  uid="$(awk '/^Uid:/{print $2}' "/proc/$pid/status")"
  gid="$(awk '/^Gid:/{print $2}' "/proc/$pid/status")"
  test "$uid" -ge 200000
  test "$gid" -ge 200000
  case " $uids " in *" $uid "*) echo "error: VMM UID $uid was reused" >&2; exit 1 ;; esac
  case " $gids " in *" $gid "*) echo "error: VMM GID $gid was reused" >&2; exit 1 ;; esac
  uids="$uids $uid"
  gids="$gids $gid"
  pids="$pids $pid"

  pid_ns="$(readlink "/proc/$pid/ns/pid")"
  mnt_ns="$(readlink "/proc/$pid/ns/mnt")"
  test "$pid_ns" != "$host_pid_ns"
  test "$mnt_ns" != "$host_mnt_ns"
  case " $pid_namespaces " in *" $pid_ns "*) echo "error: VMM PID namespace was reused" >&2; exit 1 ;; esac
  case " $mount_namespaces " in *" $mnt_ns "*) echo "error: VMM mount namespace was reused" >&2; exit 1 ;; esac
  pid_namespaces="$pid_namespaces $pid_ns"
  mount_namespaces="$mount_namespaces $mnt_ns"

  args="$(tr '\0' ' ' <"/proc/$pid/cmdline")"
  case "$args" in
    *--no-seccomp*) echo "error: PID $pid disables seccomp" >&2; exit 1 ;;
  esac
  grep -q '^NoNewPrivs:[[:space:]]*1$' "/proc/$pid/status"
  grep -q '^Seccomp:[[:space:]]*2$' "/proc/$pid/status"

  cg_rel="$(awk -F: '$1=="0"{print $3}' "/proc/$pid/cgroup")"
  test -n "$cg_rel"
  cgroup="/sys/fs/cgroup$cg_rel"
  test "$(basename "$cgroup")" = "$vmid"
  test -f "$cgroup/cgroup.controllers"
  memory_max="$(cat "$cgroup/memory.max")"
  cpu_quota="$(awk '{print $1}' "$cgroup/cpu.max")"
  pids_max="$(cat "$cgroup/pids.max")"
  test "$memory_max" != "max"
  test "$memory_max" -gt 0
  test "$(cat "$cgroup/memory.swap.max")" = "0"
  test "$cpu_quota" != "max"
  test "$cpu_quota" -gt 0
  test "$(cat "$cgroup/cpu.weight")" -ge 1
  test "$pids_max" != "max"
  test "$pids_max" -gt 0
  grep -Eq '^[0-9]+:[0-9]+ .*rbps=[0-9]+' "$cgroup/io.max"
  grep -Eq '^[0-9]+:[0-9]+ .*wbps=[0-9]+' "$cgroup/io.max"
  nofile="$(awk '/Max open files/{print $4}' "/proc/$pid/limits")"
  file_size="$(awk '/Max file size/{print $4}' "/proc/$pid/limits")"
  test "$nofile" != "unlimited"
  test "$nofile" -le 256
  test "$file_size" != "unlimited"
  test "$file_size" -le 68719476736

  jail="$jailer_base/firecracker/$vmid"
  jail_root="$jail/root"
  test -d "$jail_root"
  test "$(stat -c '%u' "$jail")" = "0"
  test "$(stat -c '%u' "$jail_root")" = "0"
  test -S "$jail_root/run/firecracker.socket"
  test -s "$jail_root/firecracker.pid"
  test "$(cat "$jail_root/firecracker.pid")" = "$pid"
  test "$(stat -c '%u:%a' "$jail_root/kernel/vmlinux")" = "0:444"
  test "$(stat -c '%u:%g:%a' "$jail_root/disks/rootfs.ext4")" = "$uid:$gid:600"
  allocation="$jailer_base/.allocations/$uid"
  test -f "$allocation"
  test "$(cat "$allocation")" = "$vmid"
  jails="$jails $jail"

  log="/tmp/firecracker-$vmid.log"
  test -f "$log"
  test "$(stat -c '%a' "$log")" = "600"
  test "$(stat -c '%s' "$log")" -le "$max_bytes"
done

# Prove both the chroot boundary and host-side DAC isolation. Entering the
# chroot to run `test` is not viable because a Firecracker jail intentionally
# contains no shell or coreutils.
set -- $uids
first_uid="$1"
set -- $gids
first_gid="$1"
set -- $pids
first_pid="$1"
set -- $jails
first_jail="$1"
second_jail="$2"
test -n "$second_jail"
test "$second_jail" != "$first_jail"
test "$(readlink -f "/proc/$first_pid/root")" = "$first_jail/root"
if setpriv --reuid "$first_uid" --regid "$first_gid" --clear-groups \
  test -r "$second_jail/root/disks/rootfs.ext4"; then
    echo "error: VMM identity $first_uid:$first_gid can read another jail's rootfs" >&2
    exit 1
fi

iptables -C FORWARD -i br-fc -o br-fc -j DROP
test "$(sysctl -n net.bridge.bridge-nf-call-iptables)" = "1"
if find /tmp -maxdepth 1 -type p -name 'sandbox-log-*.fifo' -mmin +5 -print -quit | grep -q .; then
  echo "error: stale Firecracker log FIFO found" >&2
  exit 1
fi
REMOTE
}

echo "security gate: create two independent same-host sandboxes"
SSH_KEY_DIR="$(mktemp -d "${TMPDIR:-/tmp}/sandbox-security-key.XXXXXX")"
ssh-keygen -q -t ed25519 -N '' -C "$RUN_ID" -f "$SSH_KEY_DIR/id_ed25519"
SSH_PUBLIC_KEY="$(cat "$SSH_KEY_DIR/id_ed25519.pub")"
CREATED_RESPONSE=""
create independent-a "$SSH_PUBLIC_KEY"
A="$CREATED_RESPONSE"
create independent-b
B="$CREATED_RESPONSE"
A_ID="$(jq -er '.id' <<<"$A")"
B_ID="$(jq -er '.id' <<<"$B")"
B_IP="$(jq -er '.guest_ip' <<<"$B")"

echo "security gate: verify key-only SSH as sandbox and reject root login"
SSH_PORT_ROW="$(api -X POST -H 'Content-Type: application/json' \
  --data '{"guest_port":22}' "$SANDBOX_HOST_URL/sandboxes/$A_ID/ports")"
SSH_PORT="$(jq -er '.host_port | select(. > 0)' <<<"$SSH_PORT_ROW")"
SSH_OPTIONS=(
  -i "$SSH_KEY_DIR/id_ed25519"
  -p "$SSH_PORT"
  -o BatchMode=yes
  -o ConnectTimeout=10
  -o IdentitiesOnly=yes
  -o StrictHostKeyChecking=no
  -o UserKnownHostsFile=/dev/null
)
SSH_UID="$(ssh "${SSH_OPTIONS[@]}" "sandbox@$WORKER_API_HOST" id -u)"
test "$SSH_UID" = "1000"
if ssh "${SSH_OPTIONS[@]}" "root@$WORKER_API_HOST" id -u >/dev/null 2>&1; then
  echo "error: remote root SSH login unexpectedly succeeded" >&2
  exit 1
fi

echo "security gate: verify default guest identity and privileged boundary"
for id in "$A_ID" "$B_ID"; do
  identity="$(guest_stdout "$id" \
    'set -eu; test "$(id -u)" = 1000; test "$(id -g)" = 1000; test "$USER" = sandbox; test "$HOME" = /home/sandbox; test ! -r /var/lib/sandboxd/identity; test ! -w /var/lib/sandboxd; test ! -r /etc/ssh/ssh_host_ed25519_key; code=$(curl -sS -o /dev/null -w "%{http_code}" -X POST -H "Content-Type: application/json" --data "{\"sandbox_id\":\"forged\"}" http://127.0.0.1:8090/identity); test "$code" = 403; printf secure')"
  test "$identity" = "secure"
done

echo "security gate: verify independent guest SSH identities and allowed egress"
A_SSH="$(guest_stdout "$A_ID" "ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub -E sha256 | awk '{print \$2}'")"
B_SSH="$(guest_stdout "$B_ID" "ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub -E sha256 | awk '{print \$2}'")"
test -n "$A_SSH"
test -n "$B_SSH"
test "$A_SSH" != "$B_SSH"
guest_stdout "$A_ID" 'curl -fsS --connect-timeout 5 --max-time 15 https://example.com/ >/dev/null; printf allowed' |
  grep -qx allowed

echo "security gate: verify guest-to-guest denial"
PEER_RESULT="$(exec_guest "$A_ID" \
  "curl -sS --connect-timeout 2 --max-time 3 http://$B_IP:8090/health")"
if [ "$(jq -r '.exit_code' <<<"$PEER_RESULT")" -eq 0 ]; then
  echo "error: direct guest-to-guest connection unexpectedly succeeded" >&2
  exit 1
fi

echo "security gate: verify SSH identity survives pause/resume"
api -X POST -H "Idempotency-Key: $RUN_ID-pause" \
  "$SANDBOX_HOST_URL/v1/sandboxes/$A_ID:pause" >/dev/null
api -X POST -H "Idempotency-Key: $RUN_ID-resume" \
  "$SANDBOX_HOST_URL/v1/sandboxes/$A_ID:resume" >/dev/null
A_SSH_RESUMED="$(guest_stdout "$A_ID" "ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub -E sha256 | awk '{print \$2}'")"
test "$A_SSH_RESUMED" = "$A_SSH"

echo "security gate: create snapshot and two sandboxes with the v1 batch create API"
SNAPSHOT="$(api -X POST -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $RUN_ID-snapshot" \
  --data "$(jq -cn --arg name "$RUN_ID-snapshot" '{name:$name,retention_seconds:900}')" \
  "$SANDBOX_HOST_URL/v1/sandboxes/$A_ID/snapshots")"
SNAPSHOT_ID="$(jq -er '.id' <<<"$SNAPSHOT")"
BATCH="$(api -X POST -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $RUN_ID-create-many" \
  --data "$(jq -cn --arg id "$SNAPSHOT_ID" --arg run "$RUN_ID" \
    '{count:2,sandbox:{source:{type:"snapshot",id:$id},lifecycle:{ttl_seconds:600},metadata:{probe:"security-gate",run_id:$run,role:"batch"}},max_parallelism:2}')" \
  "$SANDBOX_HOST_URL/v1/sandbox-batches")"
BATCH_DONE="$(wait_operation "$BATCH")"
while IFS= read -r id; do IDS+=("$id"); done < <(jq -er '.results[].sandbox.id' <<<"$BATCH_DONE")
test "${#IDS[@]}" -eq 4

CLONE_A_SSH="$(guest_stdout "${IDS[2]}" "ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub -E sha256 | awk '{print \$2}'")"
CLONE_B_SSH="$(guest_stdout "${IDS[3]}" "ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub -E sha256 | awk '{print \$2}'")"
test -n "$CLONE_A_SSH"
test -n "$CLONE_B_SSH"
test "$CLONE_A_SSH" != "$CLONE_B_SSH"
test "$CLONE_A_SSH" != "$A_SSH"
test "$CLONE_B_SSH" != "$A_SSH"

echo "security gate: inspect VMM users, namespaces, cgroups, jails, logs, and firewall"
refresh_rows
inspect_host_isolation

echo "security gate: inject an unexpected VMM crash and verify reconciliation"
CRASH_ID="${IDS[3]}"
CRASH_PID="${VM_ARGS[6]}"
CRASH_VMID="${VM_ARGS[7]}"
CRASH_CGROUP="$(worker "sudo -n awk -F: '\$1==\"0\"{print \$3}' /proc/$CRASH_PID/cgroup")"
worker "sudo -n kill -KILL $CRASH_PID"
wait_absent "$CRASH_ID"
IDS=("${IDS[@]:0:3}")
worker "sudo -n test ! -e '$JAILER_BASE/firecracker/$CRASH_VMID' && sudo -n test ! -e '/sys/fs/cgroup$CRASH_CGROUP'"

echo "security gate: verify expected-exit cleanup"
EXPECTED_VMIDS=()
refresh_rows
for ((i=1; i<${#VM_ARGS[@]}; i+=2)); do EXPECTED_VMIDS+=("${VM_ARGS[$i]}"); done
for id in "${IDS[@]}"; do
  api -X DELETE "$SANDBOX_HOST_URL/sandboxes/$id" >/dev/null
done
IDS=()
for vmid in "${EXPECTED_VMIDS[@]}"; do
  worker "sudo -n test ! -e '$JAILER_BASE/firecracker/$vmid' && sudo -n test ! -e '/tmp/firecracker-$vmid.log'"
done

api -X DELETE "$SANDBOX_HOST_URL/v1/snapshots/$SNAPSHOT_ID" \
  -H "Idempotency-Key: $RUN_ID-delete-snapshot" >/dev/null
SNAPSHOT_ID=""

echo "PASS: jailed VMM isolation, finite resource controls, guest identity, snapshot batch identity, network policy, crash reconciliation, and cleanup"
