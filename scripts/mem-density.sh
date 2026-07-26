#!/usr/bin/env bash
# Memory density: bring up N sandboxes two ways (snapshot-source batch vs N
# default-source creates) and measure the host memory footprint of the
# Firecracker processes.
#
# Metric: sum(RSS) counts shared pages once per process; sum(PSS) divides shared
# pages among sharers. Snapshot-source sandboxes mmap the same snapshot memory
# file, so their pages are shared in the page cache (COW until written) —
# PSS << RSS·N is the density win.
set -euo pipefail
API=${API:?}; TOK=${TOK:?}; HOST=${HOST:?}; N=${N:-64}; OUT=${OUT:-/dev/stdout}
SSH_USER=${SSH_USER:-ayush}
BENCH_RUN_ID=${BENCH_RUN_ID:-memory-density-$(date -u +%Y%m%dT%H%M%SZ)}
BENCH_RUN_ID="${BENCH_RUN_ID//[^a-zA-Z0-9-]/-}"
BENCH_RUN_ID="${BENCH_RUN_ID:0:48}"
SANDBOX_RELEASE=${SANDBOX_RELEASE:-unknown}
IDS=()
SNAPSHOT_ID=""
case "$API" in
  https://*) ;;
  http://127.0.0.1:*|http://localhost:*|http://\[::1\]:*) ;;
  http://*)
    [ "${PRIVATE_MANAGEMENT_PROXY_ACK:-}" = "I_VERIFIED_PRIVATE_AUTHENTICATED_PROXY" ] || {
      echo "error: refusing non-loopback plaintext management URL $API" >&2
      exit 1
    }
    ;;
  *)
    echo "error: API must use http:// or https://" >&2
    exit 1
    ;;
esac
h(){ curl -fsS -H "Authorization: Bearer $TOK" "$@"; }
ssh_(){ local i out; for i in 1 2 3 4 5; do
    out=$(ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 \
      "$SSH_USER@$SSH_HOST" "$@" 2>/dev/null) && { echo "$out"; return 0; }
    sleep 3
  done; return 1; }

# host-side probe: "<count> <sumRSS_kb> <sumPSS_kb> <memAvailable_kb>"
probe(){
  ssh_ 'sudo sh -c '"'"'R=0;P=0;C=0; for d in /proc/[0-9]*; do [ "$(cat $d/comm 2>/dev/null)" = firecracker ] || continue; C=$((C+1)); r=$(awk "/^Rss:/{print \$2}" $d/smaps_rollup 2>/dev/null); p=$(awk "/^Pss:/{print \$2}" $d/smaps_rollup 2>/dev/null); R=$((R+${r:-0})); P=$((P+${p:-0})); done; MA=$(awk "/^MemAvailable:/{print \$2}" /proc/meminfo); echo "$C $R $P $MA"'"'"''
}
wait_for_zero_vmm(){
  local attempt count
  for attempt in $(seq 1 120); do
    read count _ <<<"$(probe)"
    [ "$count" -eq 0 ] && return 0
    sleep 0.25
  done
  echo "error: Firecracker processes did not return to zero after cleanup" >&2
  return 1
}

del_all(){
  local id
  for id in "$@"; do
    h -X DELETE "$API/v1/sandboxes/$id" >/dev/null 2>&1 || true
  done
}
cleanup(){
  local rc=$?
  trap - EXIT INT TERM
  del_all "${IDS[@]}"
  if [ -n "$SNAPSHOT_ID" ]; then
    h -X DELETE "$API/v1/snapshots/$SNAPSHOT_ID" >/dev/null 2>&1 || true
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for tool in curl jq ssh; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "error: required command not found: $tool" >&2
    exit 1
  }
done
[[ "$N" =~ ^[1-9][0-9]*$ ]] || {
  echo "error: N must be a positive integer" >&2
  exit 1
}

baseline="$(h "$API/v1/sandboxes?page_size=1")"
if [ "$(jq -r '.sandboxes | length' <<<"$baseline")" -ne 0 ]; then
  echo "error: direct worker must have zero sandboxes before memory-density measurement" >&2
  exit 1
fi
read baseline_vmm _ <<<"$(probe)"
if [ "$baseline_vmm" -ne 0 ]; then
  echo "error: direct worker has $baseline_vmm untracked Firecracker process(es)" >&2
  exit 1
fi

echo ">> memory density, N=$N on $HOST" >&2

# --- Snapshot-source arm ---
CREATE_BODY="$(jq -nc --arg run "$BENCH_RUN_ID" --arg release "$SANDBOX_RELEASE" \
  '{source:{type:"template",id:"default"},metadata:{benchmark:"memory-density",
    benchmark_run_id:$run,benchmark_release:$release}}')"
SRC="$(h -X POST "$API/v1/sandboxes" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $BENCH_RUN_ID-source" \
  -d "$CREATE_BODY")"
SID="$(jq -er '.id' <<<"$SRC")"
IDS+=("$SID")
SNAP="$(h -X POST "$API/v1/sandboxes/$SID/snapshots" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $BENCH_RUN_ID-snapshot" \
  -d '{"retention_seconds":1800}')"
SNAPSHOT_ID="$(jq -er '.id' <<<"$SNAP")"
del_all "$SID"
IDS=()
BATCH_BODY="$(jq -nc --argjson count "$N" --arg snapshot "$SNAPSHOT_ID" \
  --arg run "$BENCH_RUN_ID" --arg release "$SANDBOX_RELEASE" \
  '{count:$count,max_parallelism:32,sandbox:{
    source:{type:"snapshot",id:$snapshot},lifecycle:{ttl_seconds:1800},
    metadata:{benchmark:"memory-density",benchmark_run_id:$run,benchmark_release:$release}}}')"
BATCH="$(h -X POST "$API/v1/sandbox-batches" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $BENCH_RUN_ID-batch" \
  -d "$BATCH_BODY")"
OPERATION_ID="$(jq -er '.id' <<<"$BATCH")"
for _ in $(seq 1 1200); do
  OPERATION="$(h "$API/v1/operations/$OPERATION_ID")"
  [ "$(jq -r '.completed_at // empty' <<<"$OPERATION")" != "" ] && break
  sleep 0.25
done
[ "$(jq -r '.completed_at // empty' <<<"${OPERATION:-{}}")" != "" ] || {
  echo "error: snapshot-source batch did not complete" >&2
  exit 1
}
IDS=()
while IFS= read -r id; do
  [ -n "$id" ] && IDS+=("$id")
done < <(jq -r '.results[]?.sandbox?.id // empty' <<<"$OPERATION")
FO_N="${#IDS[@]}"
[ "$FO_N" -eq "$N" ] && [ "$(jq -r '.failed' <<<"$OPERATION")" -eq 0 ] || {
  echo "error: snapshot-source batch created $FO_N/$N sandboxes" >&2
  exit 1
}
sleep 2
read fc_c fc_rss fc_pss fc_ma <<< "$(probe)"
echo "  snapshot-source: procs=$fc_c rss=$((fc_rss/1024))MB pss=$((fc_pss/1024))MB" >&2
[ "$fc_c" -eq "$N" ] || {
  echo "error: expected $N Firecracker processes, observed $fc_c" >&2
  exit 1
}
del_all "${IDS[@]}"
IDS=()
wait_for_zero_vmm
h -X DELETE "$API/v1/snapshots/$SNAPSHOT_ID" >/dev/null
SNAPSHOT_ID=""
sleep 2

# --- Default-source arm ---
for i in $(seq 1 "$N"); do
  id="$(h -X POST "$API/v1/sandboxes" -H 'Content-Type: application/json' \
    -H "Idempotency-Key: $BENCH_RUN_ID-default-$i" \
    -d "$CREATE_BODY" |
    jq -er '.id')"
  IDS+=("$id")
done
CB_N="${#IDS[@]}"
sleep 2
read cb_c cb_rss cb_pss cb_ma <<< "$(probe)"
echo "  default-source:  procs=$cb_c rss=$((cb_rss/1024))MB pss=$((cb_pss/1024))MB" >&2
[ "$cb_c" -eq "$N" ] || {
  echo "error: expected $N Firecracker processes, observed $cb_c" >&2
  exit 1
}
del_all "${IDS[@]}"
IDS=()
wait_for_zero_vmm

jq -nc \
  --arg run_id "$BENCH_RUN_ID" --arg release "$SANDBOX_RELEASE" \
  --argjson n "$N" \
  --argjson fo_n "$FO_N" --argjson fo_rss "$fc_rss" --argjson fo_pss "$fc_pss" \
  --argjson cb_n "$CB_N" --argjson cb_rss "$cb_rss" --argjson cb_pss "$cb_pss" \
  '{metadata:{schema_version:2,benchmark:"memory-density",run_id:$run_id,
              release:$release,api_version:"v1"},
    requested:$n,
    snapshot_source:{procs:$fo_n, rss_mb:($fo_rss/1024|floor), pss_mb:($fo_pss/1024|floor)},
    default_source:{procs:$cb_n, rss_mb:($cb_rss/1024|floor), pss_mb:($cb_pss/1024|floor)},
    fanout:{procs:$fo_n, rss_mb:($fo_rss/1024|floor), pss_mb:($fo_pss/1024|floor)},
    coldboot:{procs:$cb_n, rss_mb:($cb_rss/1024|floor), pss_mb:($cb_pss/1024|floor)}}' > "$OUT"
echo ">> wrote $OUT" >&2
