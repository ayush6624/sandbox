#!/usr/bin/env bash
# Focused production-host security gate. Creates two short-lived sandboxes on
# one worker, verifies the live VMM and bridge controls, then deletes only the
# sandboxes created by this run.
#
# Required:
#   SANDBOX_HOST_URL  Direct worker API, e.g. http://10.160.0.119:8080
#   SANDBOX_HOST_KEY  Worker API bearer token
#   WORKER_SSH        SSH target for the same worker, e.g. ayush@10.160.0.119
#
# Optional:
#   LOG_MAX_BYTES     Expected per-VMM limit (default 16777216)
set -euo pipefail

: "${SANDBOX_HOST_URL:?set a direct worker API URL}"
: "${SANDBOX_HOST_KEY:?set the worker API bearer token}"
: "${WORKER_SSH:?set the SSH target for that worker}"

LOG_MAX_BYTES="${LOG_MAX_BYTES:-16777216}"
RUN_ID="security-$(date -u +%Y%m%dT%H%M%SZ)-$$"
IDS=()

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: required command not found: $1" >&2
    exit 1
  }
}
for tool in curl jq ssh; do need "$tool"; done

api() {
  curl -fsS -H "Authorization: Bearer $SANDBOX_HOST_KEY" "$@"
}

cleanup() {
  local id
  for id in "${IDS[@]}"; do
    api -X DELETE "$SANDBOX_HOST_URL/sandboxes/$id" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

create() {
  local suffix="$1" body id
  body="$(jq -cn --arg name "$RUN_ID-$suffix" \
    '{name:$name,timeout_sec:600,hibernate_after_sec:-1}')"
  CREATED_RESPONSE="$(api -X POST -H 'Content-Type: application/json' \
    --data "$body" "$SANDBOX_HOST_URL/sandboxes")"
  id="$(jq -er '.id' <<<"$CREATED_RESPONSE")"
  IDS+=("$id")
}

echo "security gate: create two same-host sandboxes"
CREATED_RESPONSE=""
create a
A="$CREATED_RESPONSE"
create b
B="$CREATED_RESPONSE"
A_ID="$(jq -r '.id' <<<"$A")"
B_IP="$(jq -r '.guest_ip' <<<"$B")"

# Refresh from the registry-backed list because create responses intentionally
# hide or may not yet contain some host-runtime fields.
LIST="$(api "$SANDBOX_HOST_URL/sandboxes")"
A_ROW="$(jq -cer --arg id "$A_ID" '.[] | select(.id == $id)' <<<"$LIST")"
B_ID="$(jq -r '.id' <<<"$B")"
B_ROW="$(jq -cer --arg id "$B_ID" '.[] | select(.id == $id)' <<<"$LIST")"
A_PID="$(jq -er '.pid | select(. > 0)' <<<"$A_ROW")"
B_PID="$(jq -er '.pid | select(. > 0)' <<<"$B_ROW")"
A_VMID="$(jq -er '.vm_id | select(length > 0)' <<<"$A_ROW")"
B_VMID="$(jq -er '.vm_id | select(length > 0)' <<<"$B_ROW")"

echo "security gate: verify guest-to-guest denial"
EXEC_BODY="$(jq -cn --arg cmd \
  "curl -sS --connect-timeout 2 --max-time 3 http://$B_IP:8090/health" \
  '{cmd:$cmd,timeout_sec:5}')"
EXEC_RESULT="$(api -X POST -H 'Content-Type: application/json' --data "$EXEC_BODY" \
  "$SANDBOX_HOST_URL/sandboxes/$A_ID/exec")"
if [ "$(jq -r '.exit_code' <<<"$EXEC_RESULT")" -eq 0 ]; then
  echo "error: direct guest-to-guest connection unexpectedly succeeded" >&2
  exit 1
fi

echo "security gate: inspect live VMMs, logs, firewall, and stale FIFOs"
ssh -o BatchMode=yes -o ConnectTimeout=10 \
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  "$WORKER_SSH" sudo -n bash -s -- \
  "$A_PID" "$B_PID" "$A_VMID" "$B_VMID" "$LOG_MAX_BYTES" <<'REMOTE'
set -euo pipefail
max_bytes="$5"
for pid in "$1" "$2"; do
  args="$(tr '\0' ' ' <"/proc/$pid/cmdline")"
  case "$args" in
    *--no-seccomp*) echo "error: PID $pid disables seccomp" >&2; exit 1 ;;
  esac
  grep -q '^NoNewPrivs:[[:space:]]*1$' "/proc/$pid/status"
  grep -q '^Seccomp:[[:space:]]*2$' "/proc/$pid/status"
done
for vmid in "$3" "$4"; do
  path="/tmp/firecracker-$vmid.log"
  test -f "$path"
  test "$(stat -c '%a' "$path")" = "600"
  test "$(stat -c '%s' "$path")" -le "$max_bytes"
done
iptables -C FORWARD -i br-fc -o br-fc -j DROP
test "$(sysctl -n net.bridge.bridge-nf-call-iptables)" = "1"
if find /tmp -maxdepth 1 -type p -name 'sandbox-log-*.fifo' -mmin +5 -print -quit | grep -q .; then
  echo "error: stale Firecracker log FIFO found" >&2
  exit 1
fi
REMOTE

echo "security gate: verify expected-exit log cleanup"
api -X DELETE "$SANDBOX_HOST_URL/sandboxes/$A_ID" >/dev/null
api -X DELETE "$SANDBOX_HOST_URL/sandboxes/$B_ID" >/dev/null
IDS=()
ssh -o BatchMode=yes -o ConnectTimeout=10 \
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  "$WORKER_SSH" "sudo -n test ! -e /tmp/firecracker-$A_VMID.log && sudo -n test ! -e /tmp/firecracker-$B_VMID.log"

echo "PASS: seccomp, bounded logs, FIFO hygiene, lifecycle cleanup, and guest isolation"
