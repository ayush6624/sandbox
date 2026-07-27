#!/usr/bin/env bash
# Trace a held autoscaling burst from the control VM on one wall clock.
#
# Required environment:
#   PROJECT, ZONE, MIG_NAME, GATEWAY_TOKEN, WORKER_SSH_USER,
#   EXPECTED_WORKER_RELEASE,
#   LIVE_AUTOSCALE_BENCHMARK=I_UNDERSTAND_THIS_CREATES_REAL_VMS
#
# Optional:
#   GATEWAY_URL       gateway URL reachable from this VM (default localhost:9090)
#   NOMAD_ADDR        Nomad HTTP address (the nomad CLI default is used if unset)
#   BURST_COUNT       create count and concurrency (default 160)
#   POLL_MS           observer target interval (default 250)
#   EXPECTED_RUNNING  required initial RUNNING instance count (default 2)
#   EXPECTED_SUSPENDED_MIN required initial SUSPENDED count (default 2)
#   EXPECTED_FREE_PER_HOST required free slots on each ready host (default 48)
#   TRACE_DIR         output directory (default timestamped under tests/results)
#   MAX_BURST_COUNT   hard cap for BURST_COUNT (default 512)
#   BENCHMARK_TIMEOUT_SEC wall-clock driver budget (default 10800)
#   CLEANUP_TIMEOUT_SEC bounded run-owned cleanup budget (default 120)
#   TRAFFIC_SCENARIOS space-separated autoscale-traffic.ts scenarios. When set,
#                     run that correctness suite instead of the legacy one-shot
#                     burst.
#
# The trace contains no bearer tokens or SSH material. The harness refuses to
# start if the gateway already owns sandboxes, and its EXIT trap deletes only
# run-owned sandboxes left by an interrupted/failed run. It intentionally does
# not resize or deploy the fleet during setup: establish the desired two-running
# plus suspended-standby state before invoking it.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SDK_DIR="$REPO/sdk/typescript"

: "${PROJECT:?set PROJECT}"
: "${ZONE:?set ZONE}"
: "${MIG_NAME:?set MIG_NAME}"
: "${GATEWAY_TOKEN:?set GATEWAY_TOKEN}"
: "${WORKER_SSH_USER:?set WORKER_SSH_USER for the worker SSH readiness probe}"
: "${EXPECTED_WORKER_RELEASE:?set EXPECTED_WORKER_RELEASE to the deployed release}"

LIVE_ACK="I_UNDERSTAND_THIS_CREATES_REAL_VMS"
if [ "${LIVE_AUTOSCALE_BENCHMARK:-}" != "$LIVE_ACK" ]; then
  echo "error: set LIVE_AUTOSCALE_BENCHMARK=$LIVE_ACK" >&2
  exit 1
fi

GATEWAY_URL="${GATEWAY_URL:-http://127.0.0.1:9090}"
BURST_COUNT="${BURST_COUNT:-160}"
MAX_BURST_COUNT="${MAX_BURST_COUNT:-512}"
BENCHMARK_TIMEOUT_SEC="${BENCHMARK_TIMEOUT_SEC:-10800}"
CLEANUP_TIMEOUT_SEC="${CLEANUP_TIMEOUT_SEC:-120}"
POLL_MS="${POLL_MS:-250}"
EXPECTED_RUNNING="${EXPECTED_RUNNING:-2}"
EXPECTED_SUSPENDED_MIN="${EXPECTED_SUSPENDED_MIN:-2}"
EXPECTED_FREE_PER_HOST="${EXPECTED_FREE_PER_HOST:-48}"
TRACE_DIR="${TRACE_DIR:-$REPO/tests/results/autoscale-$(date -u +%Y%m%dT%H%M%SZ)}"
case "$TRACE_DIR" in
  /*) ;;
  *) TRACE_DIR="$REPO/$TRACE_DIR" ;;
esac
TRACE="$TRACE_DIR/timeline.jsonl"
TRAFFIC_SCENARIOS="${TRAFFIC_SCENARIOS:-}"
RESULT="$TRACE_DIR/$([ -n "$TRAFFIC_SCENARIOS" ] && printf traffic.json || printf burst.json)"
BENCH_LOG="$TRACE_DIR/benchmark.log"
MIG_SNAPSHOT="$TRACE_DIR/mig-latest.json"
RUN_MANIFEST="$TRACE_DIR/run.json"
CHECKSUMS="$TRACE_DIR/SHA256SUMS"
BENCH_RUN_ID="${BENCH_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
BENCH_RUN_ID="${BENCH_RUN_ID//[^a-zA-Z0-9-]/-}"
BENCH_RUN_ID="${BENCH_RUN_ID:0:24}"
READY_PREFIX="$TRACE_DIR/observer.ready.$BENCH_RUN_ID"
POLL_PIDS=()
CLEANUP_ARMED=0

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: required command not found: $1" >&2
    exit 1
  }
}
for tool in curl gcloud jq nomad ssh flock sha256sum timeout; do need "$tool"; done
if test -x "$SDK_DIR/node_modules/.bin/tsx"; then
  TSX="$SDK_DIR/node_modules/.bin/tsx"
elif command -v tsx >/dev/null 2>&1; then
  TSX="$(command -v tsx)"
else
  echo "error: install tsx globally or run npm install in $SDK_DIR before this benchmark" >&2
  exit 1
fi
for value in "$BURST_COUNT" "$MAX_BURST_COUNT" "$BENCHMARK_TIMEOUT_SEC" \
  "$CLEANUP_TIMEOUT_SEC" "$POLL_MS" "$EXPECTED_RUNNING" "$EXPECTED_FREE_PER_HOST"; do
  [[ "$value" =~ ^[1-9][0-9]*$ ]] || {
    echo "error: count, polling, running-host, and free-slot values must be positive integers" >&2
    exit 1
  }
done
if [ "$BURST_COUNT" -gt "$MAX_BURST_COUNT" ]; then
  echo "error: BURST_COUNT=$BURST_COUNT exceeds MAX_BURST_COUNT=$MAX_BURST_COUNT" >&2
  exit 1
fi
[[ "$EXPECTED_SUSPENDED_MIN" =~ ^[0-9]+$ ]] || {
  echo "error: EXPECTED_SUSPENDED_MIN must be a non-negative integer" >&2
  exit 1
}

mkdir -p "$TRACE_DIR"
: >"$TRACE"
RUN_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
GIT_COMMIT="${BENCH_GIT_COMMIT:-$(git -C "$REPO" rev-parse HEAD 2>/dev/null || printf unknown)}"

now_ms() { date +%s%3N; }

# All observer processes serialize complete JSON lines through a short-lived
# lock file. Building the line before taking the lock avoids retaining a flock
# when jq rejects malformed input.
emit() {
  local event="$1" data="${2-}" line
  [ -n "$data" ] || data='{}'
  line="$(jq -cn --argjson ts "$(now_ms)" --arg event "$event" --argjson data "$data" \
    '{ts_ms:$ts,event:$event,data:$data}')"
  (
    flock 9
    printf '%s\n' "$line" >>"$TRACE"
  ) 9>"$TRACE.lock"
}

gateway_get() {
  curl -fsS -H "Authorization: Bearer $GATEWAY_TOKEN" "$GATEWAY_URL$1"
}

owned_sandbox_ids() {
  jq -r --arg run "$BENCH_RUN_ID" '
    .[] | select(
      ((.name // "") | startswith("autoscale-" + $run + "-")) or
      ((.name // "") | startswith("burst-" + $run + "-"))
    ) | .id // .sandbox_id // empty
  '
}

cleanup_sandboxes() {
  local current id quiet=0 deadline=$((SECONDS + CLEANUP_TIMEOUT_SEC))
  local -a owned_ids=()
  while [ "$SECONDS" -lt "$deadline" ] && [ "$quiet" -lt 3 ]; do
    if ! current="$(gateway_get /sandboxes 2>/dev/null)"; then
      quiet=0
      sleep 1
      continue
    fi
    mapfile -t owned_ids < <(owned_sandbox_ids <<<"$current")
    if [ "${#owned_ids[@]}" -eq 0 ]; then
      quiet=$((quiet + 1))
    else
      quiet=0
      for id in "${owned_ids[@]}"; do
        curl -fsS -X DELETE -H "Authorization: Bearer $GATEWAY_TOKEN" \
          "$GATEWAY_URL/sandboxes/$id" >/dev/null 2>&1 || true
      done
    fi
    sleep 1
  done
  current="$(gateway_get /sandboxes)" || return 1
  mapfile -t owned_ids < <(owned_sandbox_ids <<<"$current")
  [ "${#owned_ids[@]}" -eq 0 ]
}

on_exit() {
  local rc=$? cleanup_rc=0 final_sandboxes='null' final_hosts='null' final_mig='null'
  trap - EXIT INT TERM
  set +e
  if [ "${#POLL_PIDS[@]}" -gt 0 ]; then
    kill "${POLL_PIDS[@]}" >/dev/null 2>&1 || true
    wait "${POLL_PIDS[@]}" >/dev/null 2>&1 || true
  fi
  if [ "$CLEANUP_ARMED" -ne 0 ]; then
    cleanup_sandboxes || cleanup_rc=$?
  fi
  final_sandboxes="$(gateway_get /sandboxes 2>/dev/null)" || final_sandboxes='null'
  final_hosts="$(gateway_get /hosts 2>/dev/null)" || final_hosts='null'
  final_mig="$(gcloud compute instance-groups managed list-instances "$MIG_NAME" \
    --project="$PROJECT" --zone="$ZONE" --format=json 2>/dev/null)" || final_mig='null'
  if [ "$cleanup_rc" -ne 0 ] && [ "$rc" -eq 0 ]; then rc=70; fi
  emit "cleanup_complete" "$(jq -cn --argjson exit_code "$rc" \
    --argjson cleanup_ok "$([ "$cleanup_rc" -eq 0 ] && printf true || printf false)" \
    '{exit_code:$exit_code,cleanup_ok:$cleanup_ok}')"
  jq -n \
    --arg run_id "$BENCH_RUN_ID" --arg project "$PROJECT" --arg zone "$ZONE" \
    --arg mig "$MIG_NAME" --arg gateway "$GATEWAY_URL" \
    --arg expected_release "$EXPECTED_WORKER_RELEASE" \
    --arg scenarios "$TRAFFIC_SCENARIOS" --arg result "$RESULT" \
    --arg started_at "$RUN_STARTED_AT" --arg finished_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg git_commit "$GIT_COMMIT" --argjson burst_count "$BURST_COUNT" \
    --argjson timeout_sec "$BENCHMARK_TIMEOUT_SEC" --argjson exit_code "$rc" \
    --argjson cleanup_ok "$([ "$cleanup_rc" -eq 0 ] && printf true || printf false)" \
    --argjson final_sandboxes "$final_sandboxes" --argjson final_hosts "$final_hosts" \
    --argjson final_mig "$final_mig" \
    '{
      run_id:$run_id,project:$project,zone:$zone,mig:$mig,gateway:$gateway,
      expected_release:$expected_release,
      traffic_scenarios:($scenarios | split(" ") | map(select(length > 0))),
      burst_count:$burst_count,benchmark_timeout_sec:$timeout_sec,
      started_at:$started_at,finished_at:$finished_at,git_commit:$git_commit,
      result:$result,exit_code:$exit_code,cleanup_ok:$cleanup_ok,
      final:{sandboxes:$final_sandboxes,hosts:$final_hosts,mig_instances:$final_mig}
    }' >"$RUN_MANIFEST"
  (
    cd "$TRACE_DIR" || exit
    find . -maxdepth 1 -type f ! -name SHA256SUMS -print0 |
      sort -z | xargs -0 -r sha256sum
  ) >"$CHECKSUMS"
  echo "trace: $TRACE"
  [ -f "$RESULT" ] && echo "result: $RESULT"
  echo "run manifest: $RUN_MANIFEST"
  echo "checksums: $CHECKSUMS"
  echo "benchmark log: $BENCH_LOG"
  exit "$rc"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

initial_sandboxes="$(gateway_get /sandboxes)"
initial_count="$(jq 'length' <<<"$initial_sandboxes")"
if [ "$initial_count" -ne 0 ]; then
  echo "error: gateway already has $initial_count sandboxes; refusing a destructive cleanup" >&2
  exit 1
fi
CLEANUP_ARMED=1

# Establish the exact benchmark shape before starting the observer. Suspended
# workers cannot reveal their in-memory Nomad release until resumed; the trace's
# allocation and release-compatible host transitions verify that part.
initial_mig="$(gcloud compute instance-groups managed list-instances "$MIG_NAME" \
  --project="$PROJECT" --zone="$ZONE" --format=json)"
initial_running="$(jq '[.[] | select(.instanceStatus == "RUNNING")] | length' <<<"$initial_mig")"
initial_suspended="$(jq '[.[] | select(.instanceStatus == "SUSPENDED")] | length' <<<"$initial_mig")"
if [ "$initial_running" -ne "$EXPECTED_RUNNING" ] || [ "$initial_suspended" -lt "$EXPECTED_SUSPENDED_MIN" ]; then
  echo "error: need exactly $EXPECTED_RUNNING RUNNING and at least $EXPECTED_SUSPENDED_MIN SUSPENDED workers; got $initial_running and $initial_suspended" >&2
  exit 1
fi

initial_hosts="$(gateway_get /hosts)"
bad_hosts="$(jq --argjson free "$EXPECTED_FREE_PER_HOST" \
  --arg release "$EXPECTED_WORKER_RELEASE" \
  '[.[] | select(.alive and (
    (.release_compatible | not) or (.release // "") != $release or
    .slots_used != 0 or .free != $free
  ))] | length' \
  <<<"$initial_hosts")"
ready_hosts="$(jq '[.[] | select(.alive)] | length' <<<"$initial_hosts")"
if [ "$ready_hosts" -ne "$EXPECTED_RUNNING" ] || [ "$bad_hosts" -ne 0 ]; then
  echo "error: gateway hosts are not a clean compatible ${EXPECTED_RUNNING}x${EXPECTED_FREE_PER_HOST}-slot floor" >&2
  exit 1
fi

initial_queue="$(gateway_get /metrics | awk '$1 == "sandbox_create_queue_depth" {print $2; exit}')"
if [ "${initial_queue:-unknown}" != "0" ]; then
  echo "error: gateway create queue is not empty (depth=${initial_queue:-unknown})" >&2
  exit 1
fi

declare -A MIG_STATE=()
declare -A MIG_IP=()
declare -A SSH_READY=()
declare -A NOMAD_NODE_STATE=()
declare -A NOMAD_ALLOC_STATE=()
declare -A GATEWAY_HOST_STATE=()
declare -A GATEWAY_ELIGIBLE=()
LAST_TARGET=""
LAST_QUEUE=""

observe_mig() {
  local group target rows instance name state ip snapshot_tmp
  group="$(gcloud compute instance-groups managed describe "$MIG_NAME" \
    --project="$PROJECT" --zone="$ZONE" --format=json 2>/dev/null)" || return
  target="$(jq -c '{target_size:.targetSize,target_suspended_size:(.targetSuspendedSize // 0),target_stopped_size:(.targetStoppedSize // 0)}' <<<"$group")"
  if [ "$target" != "$LAST_TARGET" ]; then
    LAST_TARGET="$target"
    emit "mig_target" "$target"
  fi

  rows="$(gcloud compute instance-groups managed list-instances "$MIG_NAME" \
    --project="$PROJECT" --zone="$ZONE" --format=json 2>/dev/null)" || return
  snapshot_tmp="$MIG_SNAPSHOT.tmp.$$"
  if ! jq -cn --argjson ts "$(now_ms)" --argjson group "$group" --argjson rows "$rows" '{
      ts_ms:$ts,
      target_size:($group.targetSize // 0),
      running:([$rows[] | select(.instanceStatus == "RUNNING")] | length),
      transitioning:([$rows[] | select(
        (.instanceStatus != "RUNNING" and .instanceStatus != "SUSPENDED") or
        ((.currentAction // "NONE") != "NONE")
      )] | length)
    }' >"$snapshot_tmp" ||
    ! mv "$snapshot_tmp" "$MIG_SNAPSHOT"; then
    return 1
  fi
  while IFS= read -r instance; do
    name="$(jq -r '.instance | split("/") | last' <<<"$instance")"
    state="$(jq -c '{instance:$name,status:(.instanceStatus // "UNKNOWN"),current_action:(.currentAction // "UNKNOWN")}' \
      --arg name "$name" <<<"$instance")"
    if [ "${MIG_STATE[$name]-}" != "$state" ]; then
      MIG_STATE[$name]="$state"
      emit "mig_instance_state" "$state"
    fi

    case "$(jq -r '.instanceStatus // ""' <<<"$instance")" in
      RUNNING|STAGING)
        if [ -z "${MIG_IP[$name]-}" ]; then
          ip="$(gcloud compute instances describe "$name" --project="$PROJECT" --zone="$ZONE" \
            --format='value(networkInterfaces[0].networkIP)' 2>/dev/null || true)"
          [ -n "$ip" ] && MIG_IP[$name]="$ip"
        fi
        ip="${MIG_IP[$name]-}"
        if [ -n "$ip" ] && [ -z "${SSH_READY[$name]-}" ]; then
          if ssh -o BatchMode=yes -o ConnectTimeout=1 \
            -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
            "$WORKER_SSH_USER@$ip" true </dev/null >/dev/null 2>&1; then
            SSH_READY[$name]=1
            emit "ssh_ready" "$(jq -cn --arg instance "$name" --arg ip "$ip" \
              '{instance:$instance,internal_ip:$ip}')"
          fi
        fi
        ;;
      *)
        unset 'SSH_READY[$name]'
        ;;
    esac
  done < <(jq -c '.[]' <<<"$rows")
  return 0
}

observe_nomad() {
  local rows row id state detail tasks
  rows="$(nomad node status -json 2>/dev/null)" || return
  jq -e 'type == "array"' <<<"$rows" >/dev/null || return
  while IFS= read -r row; do
    id="$(jq -r '.ID' <<<"$row")"
    state="$(jq -c '{node_id:.ID,name:.Name,status:.Status,scheduling_eligibility:.SchedulingEligibility}' <<<"$row")"
    if [ "${NOMAD_NODE_STATE[$id]-}" != "$state" ]; then
      NOMAD_NODE_STATE[$id]="$state"
      emit "nomad_node_state" "$state"
    fi
  done < <(jq -c '.[]' <<<"$rows")

  rows="$(nomad job allocs -json sandbox-serve 2>/dev/null)" || return
  jq -e 'type == "array"' <<<"$rows" >/dev/null || return
  while IFS= read -r row; do
    id="$(jq -r '.ID' <<<"$row")"
    detail="$(nomad alloc status -json "$id" 2>/dev/null)" || detail='{}'
    tasks="$(jq -c '
      [(.TaskStates // {}) | to_entries[] |
        {task:.key,state:.value.State,failed:.value.Failed,
         started_at:(.value.StartedAt // null),finished_at:(.value.FinishedAt // null)}]
    ' <<<"$detail")"
    state="$(jq -c --argjson tasks "$tasks" \
      '{alloc_id:.ID,node_id:.NodeID,node_name:.NodeName,client_status:.ClientStatus,
        desired_status:.DesiredStatus,task_group:.TaskGroup,tasks:$tasks}' <<<"$row")"
    if [ "${NOMAD_ALLOC_STATE[$id]-}" != "$state" ]; then
      NOMAD_ALLOC_STATE[$id]="$state"
      emit "nomad_allocation_state" "$state"
    fi
  done < <(jq -c '.[]' <<<"$rows")
  return 0
}

observe_gateway() {
  local hosts host id state metrics queue
  local -A seen=()
  hosts="$(gateway_get /hosts 2>/dev/null)" || return
  jq -e 'type == "array"' <<<"$hosts" >/dev/null || return
  while IFS= read -r host; do
    id="$(jq -r '.id' <<<"$host")"
    seen["$id"]=1
    state="$(jq -c '{host_id:.id,release:(.release // ""),release_compatible:.release_compatible,slots_total:.slots_total,slots_used:.slots_used,free:.free,alive:.alive}' <<<"$host")"
    if [ "${GATEWAY_HOST_STATE[$id]-}" != "$state" ]; then
      GATEWAY_HOST_STATE[$id]="$state"
      emit "gateway_host_state" "$state"
    fi
    if [ -z "${GATEWAY_ELIGIBLE[$id]-}" ] &&
      jq -e '.alive and .release_compatible and ((.free > 0) or (.slots_used > 0))' <<<"$host" >/dev/null; then
      GATEWAY_ELIGIBLE[$id]=1
      emit "gateway_host_eligible_observed" "$state"
    elif ! jq -e '.alive and .release_compatible and ((.free > 0) or (.slots_used > 0))' \
      <<<"$host" >/dev/null; then
      unset 'GATEWAY_ELIGIBLE[$id]'
    fi
  done < <(jq -c '.[]' <<<"$hosts")
  for id in "${!GATEWAY_ELIGIBLE[@]}"; do
    [ -n "${seen[$id]-}" ] || unset 'GATEWAY_ELIGIBLE[$id]'
  done

  metrics="$(gateway_get /metrics 2>/dev/null)" || return
  queue="$(awk '$1 == "sandbox_create_queue_depth" {print $2; exit}' <<<"$metrics")"
  if [ -n "$queue" ] && [ "$queue" != "$LAST_QUEUE" ]; then
    LAST_QUEUE="$queue"
    emit "gateway_queue_depth" "$(jq -cn --argjson depth "$queue" '{depth:$depth}')"
  fi
  [ -n "$queue" ]
}

observe_loop() {
  local source="$1" ready="$READY_PREFIX.$1" sleep_s
  sleep_s="$(awk -v ms="$POLL_MS" 'BEGIN { printf "%.3f", ms / 1000 }')"
  while true; do
    if "observe_$source" && [ ! -e "$ready" ]; then
      : >"$ready"
      emit "observer_ready" "$(jq -cn --arg source "$source" --argjson poll_ms "$POLL_MS" \
        '{source:$source,target_poll_ms:$poll_ms}')"
    fi
    sleep "$sleep_s"
  done
}

emit "preflight" "$(jq -cn \
  --arg project "$PROJECT" --arg zone "$ZONE" --arg mig "$MIG_NAME" \
  --arg run_id "$BENCH_RUN_ID" \
  --argjson burst_count "$BURST_COUNT" --argjson running "$initial_running" \
  --argjson suspended "$initial_suspended" --argjson free_per_host "$EXPECTED_FREE_PER_HOST" \
  '{project:$project,zone:$zone,mig:$mig,run_id:$run_id,initial_sandboxes:0,burst_count:$burst_count,
    initial_running:$running,initial_suspended:$suspended,free_per_host:$free_per_host}')"
for source in mig nomad gateway; do
  observe_loop "$source" &
  POLL_PIDS+=("$!")
done
# The first MIG pass performs instance-IP lookups and SSH probes. On the small
# control VM, gcloud startup alone can exceed the old 12-second gate.
for _ in $(seq 1 600); do
  [ -e "$READY_PREFIX.mig" ] && [ -e "$READY_PREFIX.nomad" ] && [ -e "$READY_PREFIX.gateway" ] && break
  sleep 0.1
done
if [ ! -e "$READY_PREFIX.mig" ] || [ ! -e "$READY_PREFIX.nomad" ] || [ ! -e "$READY_PREFIX.gateway" ]; then
  echo "error: observer failed to produce its initial snapshot" >&2
  exit 1
fi

emit "demand_marker" "$(jq -cn --argjson count "$BURST_COUNT" --arg scenarios "$TRAFFIC_SCENARIOS" \
  '{count:$count,concurrency:$count,traffic_scenarios:($scenarios | select(length > 0))}')"
set +e
if [ -n "$TRAFFIC_SCENARIOS" ]; then
  # Word splitting here is intentional: scenario names contain no whitespace.
  # shellcheck disable=SC2086
  (
    cd "$REPO/tests"
    SANDBOX_API_URL="$GATEWAY_URL" SANDBOX_API_KEY="$GATEWAY_TOKEN" \
      BENCH_RUN_ID="$BENCH_RUN_ID" AUTOSCALE_MIG_SNAPSHOT="$MIG_SNAPSHOT" \
      AUTOSCALE_TIMELINE="$TRACE" \
      AUTOSCALE_OUTPUT="$RESULT" timeout --signal=TERM --kill-after=30s \
        "${BENCHMARK_TIMEOUT_SEC}s" "$TSX" autoscale-traffic.ts $TRAFFIC_SCENARIOS
  ) >"$BENCH_LOG" 2>&1
else
  (
    cd "$SDK_DIR"
    SANDBOX_API_URL="$GATEWAY_URL" SANDBOX_API_KEY="$GATEWAY_TOKEN" \
      BENCH_RUN_ID="$BENCH_RUN_ID" timeout --signal=TERM --kill-after=30s \
        "${BENCHMARK_TIMEOUT_SEC}s" "$TSX" benchmarks/burst-bench.ts \
        --count "$BURST_COUNT" --concurrency "$BURST_COUNT" --hold --no-hibernate \
        --output "$RESULT"
  ) >"$BENCH_LOG" 2>&1
fi
benchmark_rc=$?
set -e

if [ ! -f "$RESULT" ]; then
  emit "benchmark_failed" "$(jq -cn --argjson exit_code "$benchmark_rc" '{exit_code:$exit_code}')"
  exit "$benchmark_rc"
fi

emit "benchmark_result" "$(jq -c . "$RESULT")"
emit "postflight_gateway_hosts" "$(gateway_get /hosts | jq -c \
  '[.[] | {host_id:.id,release:(.release // ""),release_compatible:.release_compatible,
           slots_total:.slots_total,slots_used:.slots_used,free:.free,alive:.alive}]')"
if [ "$benchmark_rc" -ne 0 ]; then
  exit "$benchmark_rc"
fi
