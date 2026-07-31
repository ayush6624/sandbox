#!/usr/bin/env bash
set -euo pipefail

# Autohealing and load-balancing checks have different failure semantics and
# must stay separate. The MIG check may use the private metrics listener, while
# the passthrough NLB check must probe an actually forwarded serving port.

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EDGE="$DIR/edge.sh"

fail() {
  echo "error: $1" >&2
  exit 1
}

grep -Fq 'health-checks create http "$HC"' "$EDGE" ||
  fail "edge MIG autohealing check is missing"
grep -Fq 'health-checks create tcp "$LB_HC"' "$EDGE" ||
  fail "edge load-balancer check is not a separate TCP check"
grep -Fq -- '--port=443 --enable-logging' "$EDGE" ||
  fail "edge load-balancer check must probe the forwarded TLS listener"
grep -Fq -- '--health-checks="$LB_HC"' "$EDGE" ||
  fail "edge backend does not use the dedicated load-balancer check"
grep -Fq -- '--health-check="projects/${PROJECT}/regions/${REGION}/healthChecks/${HC}"' "$EDGE" ||
  fail "edge MIG does not retain the autohealing check"

echo "PASS: edge NLB and MIG use separate serving-port and autohealing checks"
