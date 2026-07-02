#!/usr/bin/env bash
# Memory density: bring up N sandboxes two ways (fan-out from one snapshot vs N
# cold boots) and measure the host memory footprint of the firecracker processes.
#
# Metric: sum(RSS) counts shared pages once per process; sum(PSS) divides shared
# pages among sharers. Fan-out clones mmap the SAME snapshot memory file, so
# their pages are shared in the page cache (COW until written) — PSS << RSS·N is
# the density win. Also reports host MemAvailable delta as a cross-check.
set -uo pipefail
API=${API:?}; TOK=${TOK:?}; HOST=${HOST:?}; N=${N:-64}; OUT=${OUT:-/dev/stdout}
h(){ curl -s -H "Authorization: Bearer $TOK" "$@"; }
ssh_(){ local i; for i in 1 2 3 4 5; do
    out=$(ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 "ayush@$SSH_HOST" "$@" 2>/dev/null) && { echo "$out"; return 0; }
    sleep 3
  done; return 1; }

# host-side probe: "<count> <sumRSS_kb> <sumPSS_kb> <memAvailable_kb>"
probe(){
  ssh_ 'sudo sh -c '"'"'R=0;P=0;C=0; for d in /proc/[0-9]*; do [ "$(cat $d/comm 2>/dev/null)" = firecracker ] || continue; C=$((C+1)); r=$(awk "/^Rss:/{print \$2}" $d/smaps_rollup 2>/dev/null); p=$(awk "/^Pss:/{print \$2}" $d/smaps_rollup 2>/dev/null); R=$((R+${r:-0})); P=$((P+${p:-0})); done; MA=$(awk "/^MemAvailable:/{print \$2}" /proc/meminfo); echo "$C $R $P $MA"'"'"''
}

del_all(){ for id in "$@"; do h -X DELETE "$API/sandboxes/$id" >/dev/null; done; }

echo ">> memory density, N=$N on $HOST" >&2
MA0=$(ssh_ 'awk "/^MemAvailable:/{print \$2}" /proc/meminfo')

# --- Fan-out arm ---
SRC=$(h -X POST "$API/sandboxes" -H 'Content-Type: application/json' -d '{}'); SID=$(echo "$SRC"|jq -r .id)
SNAP=$(h -X POST "$API/sandboxes/$SID/snapshot"); SNID=$(echo "$SNAP"|jq -r .id)
h -X DELETE "$API/sandboxes/$SID" >/dev/null
FO=$(h -X POST "$API/snapshots/$SNID/fanout" -H 'Content-Type: application/json' -d "{\"count\":$N}")
FO_IDS=$(echo "$FO" | jq -r 'if type=="array" then .[].id else empty end')
FO_N=$(echo "$FO_IDS" | grep -c . || true)
sleep 2
read fc_c fc_rss fc_pss fc_ma <<< "$(probe)"
echo "  fanout:   procs=$fc_c rss=$((fc_rss/1024))MB pss=$((fc_pss/1024))MB" >&2
del_all $FO_IDS
h -X DELETE "$API/snapshots/$SNID" >/dev/null
sleep 2

# --- Cold-boot arm ---
CB_IDS=""
for i in $(seq 1 "$N"); do
  id=$(h -X POST "$API/sandboxes" -H 'Content-Type: application/json' -d '{}' | jq -r .id)
  [ "$id" != null ] && CB_IDS="$CB_IDS $id"
done
CB_N=$(echo $CB_IDS | wc -w | tr -d ' ')
sleep 2
read cb_c cb_rss cb_pss cb_ma <<< "$(probe)"
echo "  coldboot: procs=$cb_c rss=$((cb_rss/1024))MB pss=$((cb_pss/1024))MB" >&2
del_all $CB_IDS

jq -nc \
  --argjson n "$N" \
  --argjson fo_n "$FO_N" --argjson fo_rss "$fc_rss" --argjson fo_pss "$fc_pss" \
  --argjson cb_n "$CB_N" --argjson cb_rss "$cb_rss" --argjson cb_pss "$cb_pss" \
  '{requested:$n,
    fanout:{procs:$fo_n, rss_mb:($fo_rss/1024|floor), pss_mb:($fo_pss/1024|floor)},
    coldboot:{procs:$cb_n, rss_mb:($cb_rss/1024|floor), pss_mb:($cb_pss/1024|floor)}}' > "$OUT"
echo ">> wrote $OUT" >&2
