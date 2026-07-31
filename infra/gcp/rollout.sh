#!/usr/bin/env bash
# One command to put a commit on the production fleet and prove it landed.
#
#   ./rollout.sh                 # roll HEAD: build, upload, deploy what's stale, wait, verify
#   ./rollout.sh <sha>           # roll a previously published release (rollback)
#   ./rollout.sh --status        # where is the fleet right now? (no changes)
#   ./rollout.sh --dry-run       # print the plan and exit
#
# Flags: --skip-smoke, --smoke=full|http|none, --timeout=<sec> (default 600),
#        --force-build (re-upload even if the release exists), --yes.
#
# Why this exists: the documented path is `make gcs-release && ./deploy-job.sh`,
# which rolls the WORKERS ONLY. `sandbox` is a single binary containing both
# `serve` and `gateway`, so any Go change also needs `./control.sh deploy` —
# easy to forget, and the failure mode is a silently half-deployed fleet (an old
# gateway drops fields a new worker expects). This script derives what is stale
# by comparing each component's RUNNING release against the target, so it is
# idempotent: re-running when everything matches deploys nothing and just
# re-verifies.
#
# Convergence is judged on the gateway's own host inventory — release,
# release_compatible, alive, and free/warm capacity — because that, not
# `nomad alloc status`, is what decides whether a create can actually be placed
# (an unwarmed host deliberately advertises slots_free=0).
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$DIR/../.." && pwd)"
# shellcheck source=config.env
source "$DIR/config.env"
[ -f "$DIR/fleet-secrets.env" ] && source "$DIR/fleet-secrets.env"

TARGET=""
DRY_RUN=0
STATUS_ONLY=0
FORCE_BUILD=0
ASSUME_YES=0
SMOKE="full"
TIMEOUT=600

for arg in "$@"; do
  case "$arg" in
    --dry-run)      DRY_RUN=1 ;;
    --status)       STATUS_ONLY=1 ;;
    --force-build)  FORCE_BUILD=1 ;;
    --yes|-y)       ASSUME_YES=1 ;;
    --skip-smoke)   SMOKE="none" ;;
    --smoke=*)      SMOKE="${arg#*=}" ;;
    --timeout=*)    TIMEOUT="${arg#*=}" ;;
    -h|--help)      sed -n '2,20p' "$0"; exit 0 ;;
    -*)             echo "unknown flag: $arg" >&2; exit 2 ;;
    *)              TARGET="$arg" ;;
  esac
done
case "$SMOKE" in full|http|none) ;; *) echo "--smoke must be full|http|none" >&2; exit 2 ;; esac

CONTROL_NAME="${CONTROL_NAME:-sandbox-control}"
CONTROL_SSH_HOST="${CONTROL_SSH_HOST:-$CONTROL_NAME}"
CONTROL_IP="${CONTROL_INTERNAL_IP:?set CONTROL_INTERNAL_IP in config.env}"
GW_PORT="${GW_PORT:-9090}"
GW_URL="http://${CONTROL_IP}:${GW_PORT}"
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=15 -o StrictHostKeyChecking=accept-new)
sshc() { ssh "${SSH_OPTS[@]}" "${SSH_USER}@${CONTROL_SSH_HOST}" "$@"; }

step()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
info()  { printf '    %s\n' "$*"; }
warn()  { printf '    \033[33m! %s\033[0m\n' "$*"; }
fail()  { printf '\n\033[31mFAILED: %s\033[0m\n' "$*" >&2; exit 1; }

# --- gateway host inventory -> "<release> <compatible> <alive> <free> <warm>" per host ---
hosts_raw() {
  sshc "curl -s -m 15 -H 'Authorization: Bearer ${GATEWAY_CONTROL_TOKEN}' ${GW_URL}/internal/v1/hosts" 2>/dev/null || true
}
hosts_table() {
  hosts_raw | python3 -c '
import json,sys
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
for h in d:
    print(h.get("addr","?"), h.get("release","?"), h.get("release_compatible"),
          h.get("alive"), h.get("free",0), h.get("warm_ready",0))
'
}
gateway_release() {
  sshc 'systemctl show sandbox-gateway -p Environment --value 2>/dev/null' 2>/dev/null \
    | tr ' ' '\n' | sed -n 's/^SANDBOX_RELEASE=//p' | head -1
}

# A gateway URL reachable from wherever this script is running. The gateway
# binds the VPC address; a laptop usually has to come in over the tailnet
# instead, since the 10.160.0.0/20 subnet route needs Tailscale admin approval
# and has been unapproved before.
client_url() {
  local probe=(curl -s -o /dev/null -m 6 -H "Authorization: Bearer ${GATEWAY_TOKEN}")
  if "${probe[@]}" "${GW_URL}/sandboxes" 2>/dev/null; then echo "$GW_URL"; return 0; fi
  local ts
  ts="$(sshc 'tailscale ip -4 2>/dev/null | head -1' 2>/dev/null | tr -d '[:space:]')"
  if [ -n "$ts" ] && "${probe[@]}" "http://${ts}:${GW_PORT}/sandboxes" 2>/dev/null; then
    echo "http://${ts}:${GW_PORT}"; return 0
  fi
  return 1
}

print_state() {
  local gw; gw="$(gateway_release)"
  info "gateway  ${gw:-unreachable}"
  local any=0
  while read -r addr rel compat alive free warm; do
    any=1
    info "worker   ${rel}  compatible=${compat} alive=${alive} free=${free} warm_ready=${warm}  ${addr}"
  done < <(hosts_table)
  [ "$any" = 1 ] || warn "no workers in the gateway inventory"
}

# ---------------------------------------------------------------- status only
if [ "$STATUS_ONLY" = 1 ]; then
  step "Fleet state"
  print_state
  exit 0
fi

# ------------------------------------------------------------------ preflight
step "Preflight"
: "${RELEASE_BUCKET:?set RELEASE_BUCKET in config.env}"
: "${GATEWAY_TOKEN:?run control.sh deploy first (populates fleet-secrets.env)}"
: "${GATEWAY_CONTROL_TOKEN:?run control.sh deploy first}"

HEAD_SHA="$(git -C "$REPO" rev-parse --short HEAD)"
TARGET="${TARGET:-$HEAD_SHA}"
case "$TARGET" in *[!A-Za-z0-9._-]*|'') fail "release must be letters, digits, dot, underscore, dash" ;; esac

DIRTY_GO=0
if [ -n "$(git -C "$REPO" status --porcelain -- '*.go' go.mod go.sum)" ]; then DIRTY_GO=1; fi
RELEASE_URI="gs://${RELEASE_BUCKET}/releases/${TARGET}"
PUBLISHED=0
gsutil -q stat "${RELEASE_URI}/sandbox" 2>/dev/null && PUBLISHED=1 || true

info "target release   ${TARGET}$( [ "$TARGET" = "$HEAD_SHA" ] && echo '  (HEAD)')"
info "published        $( [ "$PUBLISHED" = 1 ] && echo yes || echo no )"

# The label must not lie about what is in the binary. gcs-release builds the
# WORKING TREE and labels it with HEAD's sha, so a target that isn't HEAD can
# only be rolled from an already-published artifact — never rebuilt here.
BUILD=0
if [ "$TARGET" = "$HEAD_SHA" ]; then
  if [ "$PUBLISHED" = 1 ] && [ "$FORCE_BUILD" = 0 ]; then
    info "build            skipped (already published; --force-build to rebuild)"
  else
    BUILD=1
    info "build            yes (from the working tree)"
    [ "$DIRTY_GO" = 0 ] || warn "uncommitted Go changes will be IN this release, labelled ${TARGET}"
  fi
else
  [ "$PUBLISHED" = 1 ] || fail "release ${TARGET} is not published and is not HEAD.
    Refusing to build it: gcs-release compiles the working tree and would label
    those bytes '${TARGET}'. To roll ${TARGET}, check it out first; to roll your
    current tree, run without a sha."
  info "build            skipped (rolling a published release — rollback path)"
fi

# --- what is stale? ---
GW_NOW="$(gateway_release)"
[ -n "$GW_NOW" ] || fail "cannot read the gateway's release over ssh (${CONTROL_SSH_HOST})"
NEED_GW=0; [ "$GW_NOW" = "$TARGET" ] || NEED_GW=1

NEED_WORKERS=0
WORKER_COUNT=0
while read -r _addr rel _compat alive _free _warm; do
  WORKER_COUNT=$((WORKER_COUNT + 1))
  [ "$alive" = "True" ] || continue
  [ "$rel" = "$TARGET" ] || NEED_WORKERS=1
done < <(hosts_table)
[ "$WORKER_COUNT" -gt 0 ] || NEED_WORKERS=1

info "gateway          ${GW_NOW} -> $( [ "$NEED_GW" = 1 ] && echo "${TARGET} (deploy)" || echo 'already current' )"
info "workers          ${WORKER_COUNT} host(s) -> $( [ "$NEED_WORKERS" = 1 ] && echo "${TARGET} (roll)" || echo 'already current' )"
info "smoke            ${SMOKE}"

# Workers pull a published artifact by sha, so any published release can be
# rolled to them. The gateway cannot: control.sh builds the WORKING TREE and
# labels it with HEAD, so pointing it at another sha would install the wrong
# bytes under that name. Refuse up front rather than half-deploying.
if [ "$NEED_GW" = 1 ] && [ "$TARGET" != "$HEAD_SHA" ]; then
  fail "the gateway is on ${GW_NOW} and needs ${TARGET}, but ${TARGET} is not HEAD (${HEAD_SHA}).
    control.sh deploy compiles the working tree and labels it HEAD, so it cannot
    install ${TARGET}. Run 'git checkout ${TARGET}' and re-run this script."
fi

if [ "$DRY_RUN" = 1 ]; then step "Dry run — nothing changed"; exit 0; fi
if [ "$BUILD" = 0 ] && [ "$NEED_GW" = 0 ] && [ "$NEED_WORKERS" = 0 ] && [ "$SMOKE" = none ]; then
  step "Nothing to do"; exit 0
fi
if [ "$ASSUME_YES" = 0 ] && [ -t 0 ] && { [ "$NEED_GW" = 1 ] || [ "$NEED_WORKERS" = 1 ]; }; then
  read -r -p "    proceed? [y/N] " reply
  case "$reply" in y|Y|yes) ;; *) echo "    aborted"; exit 1 ;; esac
fi

# ---------------------------------------------------------------- build/upload
if [ "$BUILD" = 1 ]; then
  step "Build + upload ${TARGET}"
  ( cd "$REPO" && make gcs-release ) >/dev/null || fail "gcs-release"
  info "published ${RELEASE_URI}/"
fi

# -------------------------------------------------------------------- gateway
# Deployed FIRST and deliberately: the gateway re-encodes create bodies through
# client.CreateOpts, so a new worker paired with an old gateway loses fields,
# while a new gateway in front of old workers is the benign direction.
if [ "$NEED_GW" = 1 ]; then
  step "Deploy gateway (${GW_NOW} -> ${TARGET})"
  ( cd "$DIR" && ./control.sh deploy ) >/dev/null 2>&1 || fail "control.sh deploy"
  got="$(gateway_release)"
  [ "$got" = "$TARGET" ] || fail "gateway reports '${got}' after deploy, wanted ${TARGET}"
  info "gateway now ${got}"
fi

# -------------------------------------------------------------------- workers
if [ "$NEED_WORKERS" = 1 ]; then
  step "Roll workers -> ${TARGET}"
  ( cd "$DIR" && ./deploy-job.sh "$TARGET" ) >/dev/null 2>&1 || fail "deploy-job.sh"
  info "nomad job submitted"
fi

# ---------------------------------------------------------------- convergence
# Ready means: every alive host on the target release, marked compatible, and
# advertising capacity. A host still building its ready pool reports free=0 and
# is intentionally unplaceable, so waiting on capacity is what makes the
# subsequent smoke test meaningful rather than racy.
step "Wait for convergence (timeout ${TIMEOUT}s)"
deadline=$(( $(date +%s) + TIMEOUT ))
last=""
while :; do
  ok=0; total=0; stale=0; nocap=0
  while read -r _addr rel compat alive free _warm; do
    [ "$alive" = "True" ] || continue
    total=$((total + 1))
    if [ "$rel" != "$TARGET" ] || [ "$compat" != "True" ]; then stale=$((stale + 1)); continue; fi
    if [ "${free:-0}" -le 0 ]; then nocap=$((nocap + 1)); continue; fi
    ok=$((ok + 1))
  done < <(hosts_table)

  msg="${ok}/${total} ready (stale=${stale} no-capacity=${nocap})"
  [ "$msg" = "$last" ] || { info "$msg"; last="$msg"; }
  if [ "$total" -gt 0 ] && [ "$ok" = "$total" ]; then break; fi
  [ "$(date +%s)" -lt "$deadline" ] || {
    print_state
    fail "fleet did not converge on ${TARGET} within ${TIMEOUT}s"
  }
  sleep 10
done

# ----------------------------------------------------------------- smoke test
if [ "$SMOKE" != none ]; then
  step "Smoke test (${SMOKE})"
  if [ "$SMOKE" = full ] && command -v npx >/dev/null 2>&1; then
    # Exercises the WebSocket PTY path too — auth there rides in the
    # Sec-WebSocket-Protocol list and is proxied differently from REST, so a
    # REST-only smoke passes happily while the interactive shell is broken.
    # Runs from HERE, so it needs a URL reachable from here: the VPC address
    # works from inside the VPC, the control VM's tailnet address from a laptop
    # (the 10.160.0.0/20 subnet route is not always approved).
    SMOKE_URL="$(client_url)" || fail "no gateway URL reachable from this machine"
    info "via ${SMOKE_URL}"
    if ! ( cd "$REPO/sdk/typescript" && \
           SANDBOX_API_URL="$SMOKE_URL" SANDBOX_API_KEY="$GATEWAY_TOKEN" \
           npx --yes tsx "$DIR/rollout-smoke.mts" ); then
      fail "smoke test"
    fi
  else
    [ "$SMOKE" = full ] && warn "npx not found — falling back to the REST-only smoke"
    id="$(sshc "curl -s -m 60 -X POST -H 'Authorization: Bearer ${GATEWAY_TOKEN}' \
        -H 'Content-Type: application/json' -d '{}' ${GW_URL}/sandboxes" \
      | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')" \
      || fail "create"
    info "created ${id}"
    out="$(sshc "curl -s -m 60 -X POST -H 'Authorization: Bearer ${GATEWAY_TOKEN}' \
        -H 'Content-Type: application/json' -d '{\"cmd\":\"echo SMOKE_OK\"}' \
        ${GW_URL}/sandboxes/${id}/exec" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("stdout",""))')" || true
    sshc "curl -s -m 60 -X DELETE -H 'Authorization: Bearer ${GATEWAY_TOKEN}' ${GW_URL}/sandboxes/${id}" >/dev/null
    case "$out" in *SMOKE_OK*) info "exec ok; destroyed ${id}" ;;
                   *) fail "exec did not return the marker (got: ${out})" ;; esac
  fi
fi

# -------------------------------------------------------------------- summary
step "Rolled ${TARGET}"
print_state
printf '\n    Drive it: SANDBOX_API_URL=%s SANDBOX_API_KEY=<gateway-token>\n' "$GW_URL"
