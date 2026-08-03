#!/usr/bin/env bash
# Guard: refuse to run a fleet deployment step anywhere but the control VM.
#
#   bash infra/gcp/require-control-vm.sh <what-was-invoked>
#
# Deployments run ON the control VM, never from a laptop, and this is enforced
# rather than merely documented because the failure mode is a half-deployed
# fleet. A laptop's `gcloud auth login` expires every session (Workspace Cloud
# session control) and CANNOT be replaced by a service-account key: the org
# enforces constraints/iam.disableServiceAccountKeyCreation AND
# ...disableServiceAccountKeyUpload with no project override, and we lack
# resourcemanager.projects.setIamPolicy to grant a new SA project roles anyway.
# The control VM runs as sandbox-control-sa with cloud-platform scope and takes
# credentials from the GCE metadata server: non-expiring, never prompts.
#
# Detect GCE via DMI, not a metadata HTTP call: no network, no timeout to tune,
# and the path simply does not exist on macOS.
set -euo pipefail

WHAT="${1:-this step}"

[ "${ROLLOUT_ALLOW_LOCAL:-0}" = 1 ] && exit 0
grep -qs 'Google' /sys/class/dmi/id/product_name 2>/dev/null && exit 0

cat >&2 <<EOF
error: $WHAT must run on the control VM, not this machine.

  use:  make fleet-rollout       # rsyncs this tree to the control VM, rolls there
        make fleet-status
        infra/gcp/rollout-remote.sh --fast

Why: laptop gcloud credentials expire every session and a service-account key is
banned by org policy; the control VM uses non-expiring metadata-server creds.

Override anyway (needs a live \`gcloud auth login\`):
  make fleet-rollout-local
EOF
exit 2
