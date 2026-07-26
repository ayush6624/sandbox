#!/usr/bin/env bash
# Destructive management-credential rotation gate for a live service. The
# credential file is atomically replaced without restarting the service, then
# restored byte-for-byte on every exit path.
#
# Required:
#   MANAGEMENT_URL            Management API base URL
#   MANAGEMENT_CURRENT_TOKEN  A credential present in the original token file
#   MANAGEMENT_SSH            SSH target for the management service host
#
# Optional:
#   MANAGEMENT_TOKEN_FILE       Remote credential file
#                               (default /etc/sandbox-gateway/client.tokens)
#   MANAGEMENT_TOKEN_FILE_ALLOWLIST
#                               Colon-separated exact paths this gate may replace
#                               (default is only MANAGEMENT_TOKEN_FILE's default)
#   MANAGEMENT_PROBE_PATH       Authenticated GET endpoint (default /info)
#   MANAGEMENT_ACCESS_LOG_FILE  Absolute remote log file to scan for leaked tokens
#   LOG_SCAN_MAX_BYTES          Maximum trailing log bytes scanned (default 1048576)
#   CURL_CONNECT_TIMEOUT_SEC    curl connect timeout (default 5)
#   CURL_MAX_TIME_SEC           curl total timeout (default 10)
set -euo pipefail

: "${MANAGEMENT_URL:?set the management API base URL}"
: "${MANAGEMENT_CURRENT_TOKEN:?set the current management client credential}"
: "${MANAGEMENT_SSH:?set the management service SSH target}"

ROTATION_ACK="I_UNDERSTAND_THIS_TEMPORARILY_ROTATES_LIVE_MANAGEMENT_CREDENTIALS"
if [ "${MANAGEMENT_TOKEN_ROTATION:-}" != "$ROTATION_ACK" ]; then
  echo "error: set MANAGEMENT_TOKEN_ROTATION=$ROTATION_ACK" >&2
  exit 64
fi

MANAGEMENT_TOKEN_FILE="${MANAGEMENT_TOKEN_FILE:-/etc/sandbox-gateway/client.tokens}"
MANAGEMENT_TOKEN_FILE_ALLOWLIST="${MANAGEMENT_TOKEN_FILE_ALLOWLIST:-/etc/sandbox-gateway/client.tokens}"
MANAGEMENT_PROBE_PATH="${MANAGEMENT_PROBE_PATH:-/info}"
MANAGEMENT_ACCESS_LOG_FILE="${MANAGEMENT_ACCESS_LOG_FILE:-}"
LOG_SCAN_MAX_BYTES="${LOG_SCAN_MAX_BYTES:-1048576}"
CURL_CONNECT_TIMEOUT_SEC="${CURL_CONNECT_TIMEOUT_SEC:-5}"
CURL_MAX_TIME_SEC="${CURL_MAX_TIME_SEC:-10}"
PRIVATE_PROXY_ACK="I_VERIFIED_PRIVATE_AUTHENTICATED_PROXY"

case "$MANAGEMENT_URL" in
  https://*) ;;
  http://*)
    if [ "${PRIVATE_MANAGEMENT_PROXY_ACK:-}" != "$PRIVATE_PROXY_ACK" ]; then
      echo "error: plaintext management URL requires a verified private authenticated proxy" >&2
      echo "set PRIVATE_MANAGEMENT_PROXY_ACK=$PRIVATE_PROXY_ACK only after verifying that boundary" >&2
      exit 64
    fi
    ;;
  *)
    echo "error: MANAGEMENT_URL must use https:// or http://" >&2
    exit 64
    ;;
esac
MANAGEMENT_URL="${MANAGEMENT_URL%/}"

case "$MANAGEMENT_PROBE_PATH" in
  /*) ;;
  *)
    echo "error: MANAGEMENT_PROBE_PATH must begin with /" >&2
    exit 64
    ;;
esac

# Restrict remote paths to a deliberately small, shell-safe alphabet before
# they are ever passed through SSH's remote command parser.
validate_remote_path() {
  local path="$1" label="$2"
  case "$path" in
    /*) ;;
    *)
      echo "error: $label must be an absolute path" >&2
      exit 64
      ;;
  esac
  case "$path" in
    *..*|*[!A-Za-z0-9_./-]*)
      echo "error: $label contains a forbidden path component or character" >&2
      exit 64
      ;;
  esac
}
validate_remote_path "$MANAGEMENT_TOKEN_FILE" MANAGEMENT_TOKEN_FILE
if [ -n "$MANAGEMENT_ACCESS_LOG_FILE" ]; then
  validate_remote_path "$MANAGEMENT_ACCESS_LOG_FILE" MANAGEMENT_ACCESS_LOG_FILE
fi

allowed=0
IFS=: read -r -a token_file_allowlist <<<"$MANAGEMENT_TOKEN_FILE_ALLOWLIST"
for candidate in "${token_file_allowlist[@]}"; do
  validate_remote_path "$candidate" MANAGEMENT_TOKEN_FILE_ALLOWLIST
  if [ "$candidate" = "$MANAGEMENT_TOKEN_FILE" ]; then
    allowed=1
  fi
done
if [ "$allowed" -ne 1 ]; then
  echo "error: MANAGEMENT_TOKEN_FILE is not in the exact-path allowlist" >&2
  exit 64
fi

case "$LOG_SCAN_MAX_BYTES:$CURL_CONNECT_TIMEOUT_SEC:$CURL_MAX_TIME_SEC" in
  *[!0-9:]*|0:*|*:0:*|*:0)
    echo "error: timeout and log byte limits must be positive integers" >&2
    exit 64
    ;;
esac

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: required command not found: $1" >&2
    exit 1
  }
}
for tool in base64 cmp curl grep mktemp openssl ssh; do need "$tool"; done

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/sandbox-token-rotation.XXXXXX")"
chmod 0700 "$work_dir"
original_file="$work_dir/original"
new_file="$work_dir/new"
overlap_file="$work_dir/overlap"
new_only_file="$work_dir/new-only"
scan_patterns="$work_dir/scan-patterns"
request_header="$work_dir/request-header"
restored_file="$work_dir/restored"
restore_armed=0
main_complete=0

management_ssh() {
  ssh -o BatchMode=yes -o ConnectTimeout=10 \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    "$MANAGEMENT_SSH" "$@"
}

remote_atomic_replace() {
  local source_file="$1"
  # The credential bytes travel only over stdin. They are base64 framed so
  # arbitrary original bytes can be restored exactly, including a missing
  # trailing newline.
  {
    printf '%s\n' \
      'set -euo pipefail' \
      'path="$1"' \
      'dir="${path%/*}"' \
      'tmp="$(mktemp "$dir/.sandbox-token-rotation.XXXXXX")"' \
      'cleanup() { rm -f -- "$tmp"; }' \
      'trap cleanup EXIT' \
      'base64 -d >"$tmp" <<'\''__SANDBOX_TOKEN_PAYLOAD__'\'''
    base64 <"$source_file"
    printf '%s\n' \
      '__SANDBOX_TOKEN_PAYLOAD__' \
      'chown root:root "$tmp"' \
      'chmod 0600 "$tmp"' \
      'mv -fT -- "$tmp" "$path"' \
      'trap - EXIT'
  } | management_ssh sudo -n /bin/bash -s -- "$MANAGEMENT_TOKEN_FILE"
}

request_status() {
  local token_file="$1" status
  printf 'Authorization: Bearer %s\n' "$(<"$token_file")" >"$request_header"
  chmod 0600 "$request_header"
  if ! status="$(curl -sS -o /dev/null -w '%{http_code}' \
    --connect-timeout "$CURL_CONNECT_TIMEOUT_SEC" \
    --max-time "$CURL_MAX_TIME_SEC" \
    -H "@$request_header" \
    "$MANAGEMENT_URL$MANAGEMENT_PROBE_PATH")"; then
    : >"$request_header"
    echo "error: management probe transport failed" >&2
    return 1
  fi
  : >"$request_header"
  printf '%s\n' "$status"
}

expect_accepted() {
  local token_file="$1" label="$2" status
  status="$(request_status "$token_file")"
  case "$status" in
    2??) ;;
    *)
      echo "error: $label credential was not accepted (HTTP $status)" >&2
      return 1
      ;;
  esac
}

expect_rejected() {
  local token_file="$1" label="$2" status
  status="$(request_status "$token_file")"
  case "$status" in
    401|403) ;;
    *)
      echo "error: $label credential was not rejected (HTTP $status)" >&2
      return 1
      ;;
  esac
}

scan_access_log() {
  [ -n "$MANAGEMENT_ACCESS_LOG_FILE" ] || return 0
  if management_ssh sudo -n tail -c "$LOG_SCAN_MAX_BYTES" -- \
    "$MANAGEMENT_ACCESS_LOG_FILE" |
    grep -F -f "$scan_patterns" >/dev/null; then
    echo "error: a management credential appeared in the configured access log" >&2
    return 1
  else
    local pipeline_status=("${PIPESTATUS[@]}")
    if [ "${pipeline_status[0]}" -ne 0 ]; then
      echo "error: could not read the configured access log" >&2
      return 1
    fi
    if [ "${pipeline_status[1]}" -ne 1 ]; then
      echo "error: access-log credential scan failed" >&2
      return 1
    fi
  fi
}

finish() {
  local original_status=$? cleanup_status=0
  trap - EXIT INT TERM
  if [ "$restore_armed" -eq 1 ]; then
    if ! remote_atomic_replace "$original_file"; then
      echo "error: FAILED TO RESTORE the original management credential file" >&2
      cleanup_status=1
    else
      if ! management_ssh sudo -n base64 -w0 -- "$MANAGEMENT_TOKEN_FILE" |
        base64 -d >"$restored_file" ||
        ! cmp -s "$original_file" "$restored_file"; then
        echo "error: restored credential file bytes differ from the original" >&2
        cleanup_status=1
      fi
      if ! expect_accepted "$work_dir/current" original; then
        echo "error: restored original credential did not become active" >&2
        cleanup_status=1
      fi
      if ! expect_rejected "$new_file" temporary; then
        echo "error: temporary credential remained active after restoration" >&2
        cleanup_status=1
      fi
      if ! scan_access_log; then
        cleanup_status=1
      fi
    fi
  fi
  rm -f -- "$original_file" "$new_file" "$overlap_file" \
    "$new_only_file" "$scan_patterns" "$request_header" "$restored_file" \
    "$work_dir/current"
  rmdir "$work_dir" 2>/dev/null || cleanup_status=1
  if [ "$original_status" -ne 0 ]; then
    exit "$original_status"
  fi
  if [ "$cleanup_status" -ne 0 ]; then
    exit 1
  fi
  if [ "$main_complete" -eq 1 ]; then
    echo "PASS: live management credentials rotated through overlap and new-only, then restored exactly"
  fi
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Refuse symlinks, loose permissions, and non-root ownership. The parent
# directory must also be root-owned and not writable by group or world.
management_ssh sudo -n /bin/bash -s -- "$MANAGEMENT_TOKEN_FILE" <<'REMOTE'
set -euo pipefail
path="$1"
dir="${path%/*}"
test -f "$path"
test ! -L "$path"
test "$(stat -c '%U:%G' "$path")" = "root:root"
test "$(stat -c '%a' "$path")" = "600"
test "$(stat -c '%U' "$dir")" = "root"
mode="$(stat -c '%a' "$dir")"
test $(( 8#$mode & 8#022 )) -eq 0
REMOTE

# Capture the exact original bytes before arming cleanup. A failed capture
# cannot lead to replacing a credential file with incomplete data.
management_ssh sudo -n base64 -w0 -- "$MANAGEMENT_TOKEN_FILE" |
  base64 -d >"$original_file"
chmod 0600 "$original_file"
if [ ! -s "$original_file" ]; then
  echo "error: remote management credential file is empty" >&2
  exit 1
fi

printf '%s' "$MANAGEMENT_CURRENT_TOKEN" >"$work_dir/current"
chmod 0600 "$work_dir/current"
case "$MANAGEMENT_CURRENT_TOKEN" in
  *$'\n'*|*$'\r'*)
    echo "error: MANAGEMENT_CURRENT_TOKEN must be a single header-safe line" >&2
    exit 64
    ;;
esac
if ! grep -Fxf "$work_dir/current" "$original_file" >/dev/null; then
  echo "error: MANAGEMENT_CURRENT_TOKEN is not an exact active line in the remote file" >&2
  exit 1
fi
expect_accepted "$work_dir/current" current

openssl rand -hex 32 >"$new_file"
chmod 0600 "$new_file"
if grep -Fxq -f "$new_file" "$original_file"; then
  echo "error: generated temporary credential unexpectedly duplicates an active credential" >&2
  exit 1
fi
{
  printf '%s' "$MANAGEMENT_CURRENT_TOKEN"
  printf '\n'
  printf '%s' "$(<"$new_file")"
  printf '\n'
} >"$overlap_file"
cp "$new_file" "$new_only_file"
chmod 0600 "$overlap_file" "$new_only_file"
{
  printf '%s\n' "$MANAGEMENT_CURRENT_TOKEN"
  printf '%s\n' "$(<"$new_file")"
} >"$scan_patterns"
chmod 0600 "$scan_patterns"

restore_armed=1
remote_atomic_replace "$overlap_file"
expect_accepted "$work_dir/current" current-during-overlap
expect_accepted "$new_file" temporary-during-overlap

remote_atomic_replace "$new_only_file"
expect_accepted "$new_file" temporary-new-only
expect_rejected "$work_dir/current" retired-current

main_complete=1
