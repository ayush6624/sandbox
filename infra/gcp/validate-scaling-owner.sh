#!/usr/bin/env bash
set -euo pipefail

# Single-writer invariant, inverted 2026-07-28.
#
# The GATEWAY is now the sole process allowed to GROW the production MIG: its
# queue-triggered, level-triggered direct path decides in ~1s where the
# Prometheus/Nomad-autoscaler loop needs ~10s — and ~189s when the request lands
# inside the gce-mig confirmation blackout. On the canonical 160-create held
# burst that difference was ~10s of create p95.
#
# The Nomad Autoscaler keeps the scale-IN half, capped so it should not request
# MORE nodes than the fleet already has. Two writers that both grow can ratchet
# the target far above demand, which is what the previous version of this script
# guarded against by forbidding the gateway's flags outright.
#
# KNOWN GAP: the cap is not yet airtight. It uses sandbox_hosts_live, which
# counts resumed standby workers that heartbeat to the gateway but are not part
# of the MIG targetSize the autoscaler compares against, so the autoscaler was
# still observed scaling out (from=5 to=6) after a burst drained on 2026-07-28.
# These checks therefore assert the intended wiring is present, NOT that the
# invariant holds. See docs/autoscaling-latency.md "Known gap".

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

UNIT="$DIR/control-install.sh"
POLICY="$DIR/nomad/policies/workers.hcl.tpl"
RULES="$DIR/prometheus/rules.yml.tpl"

fail() {
  echo "error: $1" >&2
  exit 1
}

# 1. The gateway must actually own scale-out.
grep -q -- '--direct-scale-mig' "$UNIT" ||
  fail "production gateway unit does not enable direct MIG scale-out"

# 2. The autoscaler must still exist as the scale-in writer.
grep -q 'target "gce-mig"' "$POLICY" ||
  fail "Nomad Autoscaler MIG target is missing"

# 3. ...and it must be capped, or it becomes a competing scale-OUT writer.
grep -q 'sandbox:workers_scale_in_ceiling' "$POLICY" ||
  fail "Nomad Autoscaler policy is not capped to scale-in only (missing sandbox:workers_scale_in_ceiling)"

# 4. The cap must exist as a recording rule and must include the gateway's
#    grow-only watermark. Capping on sandbox_hosts_live alone would scale the
#    fleet back down during the ~13s in which resized workers have not yet
#    heartbeated.
grep -q 'record: sandbox:workers_scale_in_ceiling' "$RULES" ||
  fail "sandbox:workers_scale_in_ceiling recording rule is missing"
grep -q 'sandbox_scale_out_requested' "$RULES" ||
  fail "scale-in ceiling ignores the gateway watermark sandbox_scale_out_requested"

echo "PASS: gateway scale-out wiring present; autoscaler cap configured (see KNOWN GAP above)"
