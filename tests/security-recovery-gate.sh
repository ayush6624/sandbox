#!/usr/bin/env bash
# Destructive P0 recovery gate for a disposable Linux/KVM worker. Unlike
# security-gate.sh, this intentionally restarts sandbox-serve or reboots its
# host. It refuses to run unless the worker begins empty and the explicit
# acknowledgement is present.
set -euo pipefail

: "${SANDBOX_HOST_URL:?set the direct disposable-worker API URL}"
: "${SANDBOX_HOST_KEY:?set the worker client API bearer token}"
: "${WORKER_SSH:?set the disposable worker SSH target}"
: "${RECOVERY_MODE:?set RECOVERY_MODE=server-crash or host-reboot}"

ACK="I_UNDERSTAND_THIS_RESTARTS_OR_REBOOTS_A_DISPOSABLE_WORKER"
if [ "${DISPOSABLE_WORKER_RECOVERY:-}" != "$ACK" ]; then
  echo "error: set DISPOSABLE_WORKER_RECOVERY=$ACK" >&2
  exit 64
fi
case "$RECOVERY_MODE" in server-crash|host-reboot) ;; *)
  echo "error: RECOVERY_MODE must be server-crash or host-reboot" >&2
  exit 64
esac

JAILER_BASE="${JAILER_BASE:-/mnt/sandbox-data/jailer}"
RECOVERY_TIMEOUT_SEC="${RECOVERY_TIMEOUT_SEC:-300}"
RUN_ID="security-recovery-$(date -u +%Y%m%dT%H%M%SZ)-$$"
PROBE_ID=""

case "$SANDBOX_HOST_URL" in
  https://*) ;;
  *)
    if [ "${PRIVATE_MANAGEMENT_PROXY_ACK:-}" != "I_VERIFIED_PRIVATE_AUTHENTICATED_PROXY" ]; then
      echo "error: plaintext management URL requires a verified private authenticated proxy" >&2
      exit 64
    fi
    ;;
esac

api() {
  curl -fsS --connect-timeout 5 --max-time 10 \
    -H "Authorization: Bearer $SANDBOX_HOST_KEY" "$@"
}
worker() {
  ssh -o BatchMode=yes -o ConnectTimeout=10 \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    "$WORKER_SSH" "$@"
}
wait_host_cleanup() {
  local deadline
  deadline=$(( $(date +%s) + RECOVERY_TIMEOUT_SEC ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if worker "sudo -n test ! -e '$JAILER_BASE/firecracker/$VMID' && sudo -n test ! -e '/sys/fs/cgroup$CGROUP'" \
      >/dev/null 2>&1; then
      return
    fi
    sleep 2
  done
  echo "error: stale jail or cgroup remained after $RECOVERY_MODE" >&2
  return 1
}
cleanup() {
  if [ -n "$PROBE_ID" ]; then
    api -X DELETE "$SANDBOX_HOST_URL/sandboxes/$PROBE_ID" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

for tool in curl jq ssh; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "error: required command not found: $tool" >&2
    exit 1
  }
done

BASELINE="$(api "$SANDBOX_HOST_URL/sandboxes")"
if [ "$(jq 'length' <<<"$BASELINE")" -ne 0 ]; then
  echo "error: destructive recovery gate requires an empty disposable worker" >&2
  exit 65
fi

BODY="$(jq -cn --arg name "$RUN_ID-probe" \
  '{name:$name,timeout_sec:900,hibernate_after_sec:-1}')"
PROBE="$(api -X POST -H 'Content-Type: application/json' --data "$BODY" \
  "$SANDBOX_HOST_URL/sandboxes")"
PROBE_ID="$(jq -er '.id' <<<"$PROBE")"
LIST="$(api "$SANDBOX_HOST_URL/sandboxes")"
ROW="$(jq -cer --arg id "$PROBE_ID" '.[] | select(.id == $id)' <<<"$LIST")"
PID="$(jq -er '.pid | select(. > 0)' <<<"$ROW")"
VMID="$(jq -er '.vm_id | select(length > 0)' <<<"$ROW")"
CGROUP="$(worker "sudo -n awk -F: '\$1==\"0\"{print \$3}' /proc/$PID/cgroup")"
EXEC_BODY="$(jq -cn '{cmd:"ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub -E sha256 | awk '\\''{print $2}'\\''",timeout_sec:20}')"
IDENTITY_BEFORE="$(api -X POST -H 'Content-Type: application/json' --data "$EXEC_BODY" \
  "$SANDBOX_HOST_URL/sandboxes/$PROBE_ID/exec" | jq -er '.stdout')"

echo "security recovery gate: inject $RECOVERY_MODE on disposable worker"
case "$RECOVERY_MODE" in
  server-crash)
    worker sudo -n bash -s <<'REMOTE'
set -euo pipefail
matches=()
for proc in /proc/[0-9]*; do
  pid="${proc##*/}"
  test -r "$proc/cmdline" || continue
  mapfile -d '' -t argv <"$proc/cmdline" || true
  test "${#argv[@]}" -ge 2 || continue
  test "${argv[1]}" = "serve" || continue
  test "$(basename "$(readlink "$proc/exe")")" = "sandbox" || continue
  matches+=("$pid")
done
if [ "${#matches[@]}" -ne 1 ]; then
  echo "error: expected exactly one validated 'sandbox serve' PID, found ${#matches[@]}" >&2
  exit 1
fi
kill -KILL "${matches[0]}"
REMOTE
    ;;
  host-reboot)
    worker "sudo -n systemctl reboot" >/dev/null 2>&1 || true
    ;;
esac

deadline=$(( $(date +%s) + RECOVERY_TIMEOUT_SEC ))
seen_unavailable=0
while [ "$(date +%s)" -lt "$deadline" ]; do
  if ! api "$SANDBOX_HOST_URL/info" >/dev/null 2>&1; then
    seen_unavailable=1
    sleep 2
    continue
  fi
  if [ "$seen_unavailable" -eq 1 ] || [ "$RECOVERY_MODE" = "server-crash" ]; then
    break
  fi
  sleep 2
done
api "$SANDBOX_HOST_URL/info" >/dev/null
if [ "$RECOVERY_MODE" = "host-reboot" ] && [ "$seen_unavailable" -ne 1 ]; then
  echo "error: host-reboot probe never observed the worker go unavailable" >&2
  exit 1
fi

if [ "$RECOVERY_MODE" = "server-crash" ]; then
  deadline=$(( $(date +%s) + RECOVERY_TIMEOUT_SEC ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if ! api "$SANDBOX_HOST_URL/sandboxes" |
      jq -e --arg id "$PROBE_ID" '.[] | select(.id == $id)' >/dev/null; then
      break
    fi
    sleep 2
  done
  if api "$SANDBOX_HOST_URL/sandboxes" |
    jq -e --arg id "$PROBE_ID" '.[] | select(.id == $id)' >/dev/null; then
    echo "error: stale running sandbox row survived the server crash" >&2
    exit 1
  fi
  wait_host_cleanup
  worker "sudo -n iptables -C FORWARD -i br-fc -o br-fc -j DROP"
  PROBE_ID=""
  echo "PASS: server crash reconciled the probe without stale jail, cgroup, or sandbox state"
  exit 0
fi

# A normal host reboot may gracefully pause a sandbox before shutdown. Both
# valid outcomes are accepted: the probe is deleted, or its identity is
# preserved in a paused sandbox that remains wakeable.
wait_host_cleanup
worker "sudo -n iptables -C FORWARD -i br-fc -o br-fc -j DROP"
AFTER="$(api "$SANDBOX_HOST_URL/sandboxes")"
if ! ROW_AFTER="$(jq -cer --arg id "$PROBE_ID" '.[] | select(.id == $id)' <<<"$AFTER")"; then
  PROBE_ID=""
  echo "PASS: host reboot cleanly deleted the probe and left no jail/cgroup state"
  exit 0
fi
STATUS_AFTER="$(jq -r '.status' <<<"$ROW_AFTER")"
case "$STATUS_AFTER" in hibernated|paused) ;; *)
  echo "error: host reboot left probe in unexpected state $STATUS_AFTER" >&2
  exit 1
esac
IDENTITY_AFTER="$(api -X POST -H 'Content-Type: application/json' --data "$EXEC_BODY" \
  "$SANDBOX_HOST_URL/sandboxes/$PROBE_ID/exec" | jq -er '.stdout')"
test "$IDENTITY_AFTER" = "$IDENTITY_BEFORE"
api -X DELETE "$SANDBOX_HOST_URL/sandboxes/$PROBE_ID" >/dev/null
PROBE_ID=""
echo "PASS: host reboot preserved and woke the paused probe identity without stale jail/cgroup state"
