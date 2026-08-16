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
# The cap's authority is the PROVIDER's target size (sandbox_mig_target_size,
# polled and exported by the gateway) — the same number the gce-mig target
# compares against, so min(desired, targetSize) can only hold or shrink.
# An earlier version capped on sandbox_hosts_live, which also counts resumed
# standby workers outside that target; the autoscaler was then observed scaling
# out anyway (from=5 to=6) after a burst drained on 2026-07-28.

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

# 4. The cap must exist as a recording rule, and its FIRST term must be the
#    provider target size. A heartbeat-derived ceiling is not equivalent: it
#    counts resumed standby workers outside the MIG target and admitted a real
#    scale-out past the cap on 2026-07-28.
grep -q 'record: sandbox:workers_scale_in_ceiling' "$RULES" ||
  fail "sandbox:workers_scale_in_ceiling recording rule is missing"
grep -qE 'expr: *sum\(sandbox_mig_target_size\{job="sandbox-gateway"\}\)' "$RULES" ||
  fail "scale-in ceiling must lead with the provider target size sandbox_mig_target_size"

# 5. ...and the gateway must actually export that series, or the ceiling
#    silently falls back to the looser heartbeat form forever.
grep -q 'sandbox_mig_target_size' "$DIR/../../internal/gateway/metrics.go" ||
  fail "gateway does not export sandbox_mig_target_size"

# 6. A ceiling alone is not enough: it stops the autoscaler out-growing the
#    gateway, but nothing stopped it shrinking BELOW what the gateway just
#    asked for. The gateway sizes to live+1 on a non-empty create queue and
#    sandbox:workers_demand has no such term, so the autoscaler read the lower
#    number and purged the host the gateway had just added, mid-burst
#    (2026-08-16: 11 x `502 host ... unreachable: EOF`). The floor closes it.
grep -q 'record: sandbox:workers_scale_out_floor' "$RULES" ||
  fail "sandbox:workers_scale_out_floor recording rule is missing (autoscaler can scale in against a live gateway scale-out)"
grep -q 'sandbox_direct_scale_out_total' "$DIR/../../internal/gateway/metrics.go" ||
  fail "gateway does not export sandbox_direct_scale_out_total; the scale-out floor would never engage"

echo "PASS: gateway owns scale-out; autoscaler capped to scale-in on the provider target size, floored on a live scale-out"
