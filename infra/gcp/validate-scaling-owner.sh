#!/usr/bin/env bash
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if grep -q -- '--direct-scale-' "$DIR/control-install.sh"; then
  echo "error: production gateway unit enables a competing direct MIG writer" >&2
  exit 1
fi
if ! grep -q 'target "gce-mig"' "$DIR/nomad/policies/workers.hcl.tpl"; then
  echo "error: Nomad Autoscaler MIG target is missing" >&2
  exit 1
fi

echo "PASS: Nomad Autoscaler is the sole configured production MIG writer"
