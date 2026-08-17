#!/usr/bin/env bash
set -euo pipefail

# Single-writer invariant, final form (2026-08-17).
#
# The GATEWAY is the only process that resizes the production MIG, in BOTH
# directions.
#
# It took two inversions to get here. First the gateway took scale-out, because
# its queue-triggered path decides in ~1s where the Prometheus/Nomad-autoscaler
# loop needs ~10s — and ~189s when the request lands inside the gce-mig
# confirmation blackout, worth ~10s of create p95 on a 160-create burst. The
# autoscaler kept scale-in, capped so it could not also grow the group.
#
# That split failed on 2026-08-16, twice over:
#
#   1. The two sides were independent implementations of "how many hosts do we
#      need" and disagreed by one host — the gateway sizes to live+1 whenever
#      the create queue is non-empty, because a queued create PROVES the fleet
#      cannot place it, and the recording rule had no such term. The autoscaler
#      applied its lower answer and, with node_purge + newest_create_index,
#      deleted the host the gateway had just added. 11 running trials died with
#      `502 host ... unreachable: EOF`.
#
#   2. Its check was returning nothing at all — count.original:0 with an empty
#      reason_history, while the identical query returned a clean 2 or 3 from
#      Prometheus on the same host. So it was never a signal-driven controller;
#      it was a constant "drive to MIG_MIN" loop, invisible while the fleet sat
#      at MIG_MIN and destructive every time the gateway grew it.
#
# Underneath both: Nomad cannot see occupancy here. Every worker runs ONE
# system-job allocation whether it holds zero sandboxes or fifty, because
# sandboxes are Firecracker VMs inside that allocation. node_selector_strategy
# was therefore a guess, and node_purge acted on the guess by destroying the
# data disk.
#
# The gateway drains instead of guessing: cordon the emptiest host, wait for it
# to actually empty, then delete THAT instance by name.

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

UNIT="$DIR/control-install.sh"
GW="$DIR/../../internal/gateway"
MIG="$DIR/mig.sh"

fail() {
  echo "error: $1" >&2
  exit 1
}

# 1. The gateway must own scale-out.
grep -q -- '--direct-scale-mig' "$UNIT" ||
  fail "production gateway unit does not enable direct MIG scale-out"

# 2. ...and scale-in. --direct-scale-min is both the floor and the enable, so
#    its absence silently leaves the fleet unable to shrink at all.
grep -q -- '--direct-scale-min' "$UNIT" ||
  fail "production gateway unit does not enable gateway-owned scale-in (--direct-scale-min)"

# 3. No second writer may exist. These templates returning is the regression.
# Spelled with explicit `if`, not `test ... && fail`: under `set -e` an AND-list
# whose left side fails is the normal (passing) path here, and getting that
# subtlety wrong makes the guard exit 0 on the very case it exists to catch.
for gone in "$DIR/nomad/policies/workers.hcl.tpl" "$DIR/nomad/autoscaler.hcl.tpl"; do
  if [ -e "$gone" ]; then
    fail "$(basename "$gone") is back: the Nomad autoscaler is a competing MIG writer"
  fi
done
if grep -q 'ExecStart=/usr/local/bin/nomad-autoscaler' "$UNIT"; then
  fail "control-install.sh installs the nomad-autoscaler service again"
fi
grep -q 'systemctl disable --now nomad-autoscaler' "$UNIT" ||
  fail "control-install.sh must keep removing a previously installed nomad-autoscaler"

# GCE's scale-out standby pool is also a second controller: it replenishes its
# reserve by suspending/stopping an arbitrary running member without knowing
# whether that host owns sandboxes. It must stay in manual mode with zero pool.
if grep -q -- '--standby-policy-mode=scale-out-pool' "$MIG"; then
  fail "mig.sh can enable GCE scale-out standby, which may suspend a busy worker"
fi
grep -q -- '--standby-policy-mode=manual' "$MIG" ||
  fail "mig.sh does not explicitly keep the MIG standby policy in manual mode"
grep -q 'STANDBY_SUSPENDED_SIZE="0"' "$DIR/config.env.example" ||
  fail "config.env.example enables suspended standby"
grep -q 'STANDBY_STOPPED_SIZE="0"' "$DIR/config.env.example" ||
  fail "config.env.example enables stopped standby"

# Updating the template under GCE's default PROACTIVE policy is itself a
# destructive writer: set-instance-template immediately substitutes live VMs.
# The non-disruptive template command must switch to OPPORTUNISTIC first.
template_policy_line="$(awk '/^cmd_template\(\)/ { in_template=1 } in_template && /update-policy-type=opportunistic/ { print NR; exit }' "$MIG")"
template_set_line="$(awk '/^cmd_template\(\)/ { in_template=1 } in_template && /managed set-instance-template/ { print NR; exit }' "$MIG")"
[ -n "$template_policy_line" ] && [ -n "$template_set_line" ] &&
  [ "$template_policy_line" -lt "$template_set_line" ] ||
  fail "mig.sh template updates must set OPPORTUNISTIC policy before changing the template"

# 4. Scale-in must remove a NAMED instance. A resize-down lets GCE choose the
#    victim, which is never the host that was drained — that is the whole bug
#    this design exists to avoid.
grep -q 'deleteInstances' "$DIR/../../internal/gcemig/scaler.go" ||
  fail "the scaler no longer deletes a specific instance; a resize-down picks its own victim"

# 5. A drained host is defined by holding nothing, and the cordon must be
#    honoured by placement or the drain never finishes.
grep -q 'func (h \*host) load()' "$GW/gateway.go" ||
  fail "host.load() is gone; scale-in cannot tell a drained host from a busy one"
grep -q 'h.draining' "$GW/gateway.go" ||
  fail "placement no longer honours the scale-in cordon"

echo "PASS: the gateway is the sole MIG writer; scale-in cordons, drains, then deletes by name"
