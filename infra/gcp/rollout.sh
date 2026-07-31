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
FAST=0
SMOKE="full"
TIMEOUT=600
POLL=10

for arg in "$@"; do
  case "$arg" in
    --dry-run)      DRY_RUN=1 ;;
    --status)       STATUS_ONLY=1 ;;
    --force-build)  FORCE_BUILD=1 ;;
    --yes|-y)       ASSUME_YES=1 ;;
    --skip-smoke)   SMOKE="none" ;;
    --smoke=*)      SMOKE="${arg#*=}" ;;
    --timeout=*)    TIMEOUT="${arg#*=}" ;;
    --fast)         FAST=1 ;;
    -h|--help)      sed -n '2,20p' "$0"; exit 0 ;;
    -*)             echo "unknown flag: $arg" >&2; exit 2 ;;
    *)              TARGET="$arg" ;;
  esac
done
case "$SMOKE" in full|http|none) ;; *) echo "--smoke must be full|http|none" >&2; exit 2 ;; esac

# --fast is the rapid dev-iteration path. The fleet side of a rollout is already
# fast (golden is ADOPTED not rebuilt, and the ready pool refills at ~1.5s per
# VM in parallel) — the cost is almost entirely bytes crossing the network from
# here plus control-plane work that a code change cannot affect. So fast mode:
#   * builds ONLY cmd/sandbox, stripped (-s -w): 30.5 MiB -> 15 MiB of egress.
#     sandboxd is inert for workers (run.sh deliberately never runs
#     install-agent, so the agent is image-pinned), and Go panic tracebacks come
#     from pclntab, not DWARF, so stripping costs delve, not stack traces.
#   * uploads to GCS and pushes the gateway binary CONCURRENTLY, since neither
#     waits on the other.
#   * touches only the gateway on the control plane (control.sh gateway) instead
#     of reinstalling nomad/prometheus/autoscaler/grafana.
#   * polls convergence every 2s instead of 10s, and runs a leaner smoke.
# It publishes under a unique "<sha>-dev-<UTC timestamp>" label so a stripped,
# agent-less dev artifact can never be mistaken for a real release, and a
# second build can never appear converged while old same-labelled bytes run.
if [ "$FAST" = 1 ]; then
  POLL=2
  [ "$SMOKE" = full ] && SMOKE=fast
fi
case "$SMOKE" in full|fast|http|none) ;; *) ;; esac

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
# BUILDABLE distinguishes "a label for the tree we can compile right now" from
# "a label naming someone else's already-published bytes". Only the former may
# be built, because the build always compiles the WORKING TREE — labelling it
# with any other sha would be a lie about what is in the binary.
if [ -n "$TARGET" ]; then
  BUILDABLE=0
else
  BUILDABLE=1
  TARGET="$HEAD_SHA"
  [ "$FAST" = 0 ] || TARGET="${HEAD_SHA}-dev-$(date -u +%Y%m%d%H%M%S)"
fi
case "$TARGET" in *[!A-Za-z0-9._-]*|'') fail "release must be letters, digits, dot, underscore, dash" ;; esac

DIRTY_GO=0
if [ -n "$(git -C "$REPO" status --porcelain -- '*.go' go.mod go.sum)" ]; then DIRTY_GO=1; fi
RELEASE_URI="gs://${RELEASE_BUCKET}/releases/${TARGET}"
PUBLISHED=0
gsutil -q stat "${RELEASE_URI}/sandbox" 2>/dev/null && PUBLISHED=1 || true

info "target release   ${TARGET}$( [ "$BUILDABLE" = 1 ] && echo '  (this working tree)')"
info "mode             $( [ "$FAST" = 1 ] && echo 'fast (dev: stripped, sandbox only, gateway-only control plane)' || echo 'full' )"
info "published        $( [ "$PUBLISHED" = 1 ] && echo yes || echo no )"

BUILD=0
if [ "$BUILDABLE" = 1 ]; then
  # A dev label is unique and always rebuilt. Besides making the label honest
  # about its bytes, uniqueness forces both Nomad and the gateway deploy path
  # to restart instead of accepting a prior dev generation as converged.
  if [ "$PUBLISHED" = 1 ] && [ "$FORCE_BUILD" = 0 ] && [ "$FAST" = 0 ]; then
    info "build            skipped (already published; --force-build to rebuild)"
  else
    BUILD=1
    info "build            yes (working tree$( [ "$FAST" = 1 ] && echo ', stripped, cmd/sandbox only' ))"
    [ "$DIRTY_GO" = 0 ] || [ "$FAST" = 1 ] || \
      warn "uncommitted Go changes will be IN this release, labelled ${TARGET}"
  fi
else
  [ "$PUBLISHED" = 1 ] || fail "release ${TARGET} is not published and is not this tree.
    Refusing to build it: the build compiles the working tree and would label
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
WORKER_REL=""
while read -r _addr rel _compat alive _free _warm; do
  WORKER_COUNT=$((WORKER_COUNT + 1))
  [ "$alive" = "True" ] || continue
  [ -n "$WORKER_REL" ] || WORKER_REL="$rel"
  [ "$rel" = "$TARGET" ] || NEED_WORKERS=1
done < <(hosts_table)
[ "$WORKER_COUNT" -gt 0 ] || NEED_WORKERS=1

info "gateway          ${GW_NOW} -> $( [ "$NEED_GW" = 1 ] && echo "${TARGET} (deploy)" || echo 'already current' )"
info "workers          ${WORKER_COUNT} host(s) -> $( [ "$NEED_WORKERS" = 1 ] && echo "${TARGET} (roll)" || echo 'already current' )"
info "smoke            ${SMOKE}"

# Workers pull a published artifact by sha, so any published release can be
# rolled to them. The gateway cannot: control.sh compiles the WORKING TREE, so
# pointing it at another sha would install the wrong bytes under that name.
# Refuse up front rather than half-deploying.
if [ "$NEED_GW" = 1 ] && [ "$BUILDABLE" = 0 ]; then
  fail "the gateway is on ${GW_NOW} and needs ${TARGET}, which is not this tree (HEAD is ${HEAD_SHA}).
    control.sh compiles the working tree, so it cannot install ${TARGET}.
    Run 'git checkout ${TARGET}' and re-run this script."
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
# Fast mode compiles once, here, and hands the same artifact to both the GCS
# upload (for workers) and control.sh gateway — the old path compiled twice.
BUILT_BIN=""
if [ "$BUILD" = 1 ]; then
  if [ "$FAST" = 1 ]; then
    step "Build ${TARGET} (stripped, cmd/sandbox only)"
    ( cd "$REPO" && mkdir -p bin && \
      GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/sandbox ./cmd/sandbox ) \
      || fail "build"
    BUILT_BIN="$REPO/bin/sandbox"
    info "$(du -h "$BUILT_BIN" | cut -f1) (unstripped+sandboxd would be ~30 MiB)"
  else
    step "Build + upload ${TARGET}"
    ( cd "$REPO" && make gcs-release ) >/dev/null || fail "gcs-release"
    info "published ${RELEASE_URI}/"
  fi
fi

# In fast mode the GCS upload (workers) and the gateway push are independent
# network transfers, so run them concurrently rather than back to back.
#
# The release prefix must ALSO contain sandboxd: serve.nomad.hcl has a second,
# mandatory artifact stanza for it and run.sh chmods both, so a prefix with only
# `sandbox` fails the roll outright. The pulled sandboxd is never executed (the
# agent is image-pinned — see the comments in serve.nomad.hcl), so fast mode
# copies it server-side from the release the workers are already running:
# in-region, effectively instant, and zero egress from here. Falling back to a
# local build+upload keeps the path working if no donor is available.
#
# sandbox-edge is in the prefix for the same reason: startup-edge.sh downloads
# releases/<sha>/sandbox-edge unconditionally, so an edge VM created or healed
# against a prefix missing it 404s, never passes its health check, and silently
# leaves the load balancer with no backends. Neither auxiliary binary is ever
# executed by a gateway-only fast roll — they are here so the prefix stays a
# complete, deployable release.
UPLOAD_PID=""
if [ "$BUILD" = 1 ] && [ "$FAST" = 1 ]; then
  step "Upload + gateway (concurrent)"
  ( set -e
    gsutil -q -m cp "$BUILT_BIN" "${RELEASE_URI}/sandbox"
    # $1 = object name, $2 = go package to build if no donor release has it.
    ensure_aux() {
      local obj="$1" pkg="$2"
      if gsutil -q stat "${RELEASE_URI}/${obj}" 2>/dev/null; then
        return 0
      fi
      if [ -n "$WORKER_REL" ] && \
         gsutil -q cp "gs://${RELEASE_BUCKET}/releases/${WORKER_REL}/${obj}" \
                      "${RELEASE_URI}/${obj}" 2>/dev/null; then
        return 0
      fi
      ( cd "$REPO" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
          go build -ldflags="-s -w" -o "bin/${obj}" "$pkg" )
      gsutil -q -m cp "$REPO/bin/${obj}" "${RELEASE_URI}/${obj}"
    }
    ensure_aux sandboxd ./cmd/sandboxd
    ensure_aux sandbox-edge ./services/sandbox-edge/cmd/sandbox-edge
  ) & UPLOAD_PID=$!
fi

# -------------------------------------------------------------------- gateway
# Deployed FIRST and deliberately: the gateway re-encodes create bodies through
# client.CreateOpts, so a new worker paired with an old gateway loses fields,
# while a new gateway in front of old workers is the benign direction.
if [ "$NEED_GW" = 1 ]; then
  [ "$FAST" = 1 ] || step "Deploy gateway (${GW_NOW} -> ${TARGET})"
  if [ "$FAST" = 1 ]; then
    SANDBOX_RELEASE="$TARGET" SANDBOX_BIN="$BUILT_BIN" \
      bash -c "cd '$DIR' && ./control.sh gateway" >/dev/null 2>&1 || fail "control.sh gateway"
  else
    ( cd "$DIR" && ./control.sh deploy ) >/dev/null 2>&1 || fail "control.sh deploy"
  fi
  got="$(gateway_release)"
  [ "$got" = "$TARGET" ] || fail "gateway reports '${got}' after deploy, wanted ${TARGET}"
  info "gateway now ${got}"
fi

# -------------------------------------------------------------------- workers
# The upload has to land before Nomad is told to fetch it, so join it here —
# after the gateway push has already overlapped with it.
if [ -n "$UPLOAD_PID" ]; then
  wait "$UPLOAD_PID" || fail "upload to ${RELEASE_URI}"
  info "uploaded ${RELEASE_URI}/sandbox"
fi

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
  sleep "$POLL"
done

# ----------------------------------------------------------------- smoke test
if [ "$SMOKE" != none ]; then
  step "Smoke test (${SMOKE})"
  if { [ "$SMOKE" = full ] || [ "$SMOKE" = fast ]; } && command -v npx >/dev/null 2>&1; then
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
           SMOKE_MODE="$SMOKE" npx --yes tsx "$DIR/rollout-smoke.mts" ); then
      fail "smoke test"
    fi
  else
    [ "$SMOKE" = http ] || warn "npx not found — falling back to the REST-only smoke"
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
