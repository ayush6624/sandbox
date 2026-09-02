#!/usr/bin/env bash
# Run rollout.sh ON the control VM instead of from your laptop.
#
#   ./rollout-remote.sh              # roll HEAD from the control VM
#   ./rollout-remote.sh --fast       # dev-iteration path, executed there
#   ./rollout-remote.sh --dry-run    # plan only
#   ./rollout-remote.sh --status     # where the fleet is (read-only)
#
# Any flag rollout.sh accepts is forwarded verbatim.
#
# Why this exists: `gcloud auth login` on a laptop dies every session, because
# the aion.xyz Workspace enforces Cloud session control and expires the refresh
# token. The fix is NOT a service-account key — the org enforces
# constraints/iam.disableServiceAccountKeyCreation (and ...KeyUpload), so no
# JSON key can be created or uploaded, and we lack
# resourcemanager.projects.setIamPolicy to grant a new SA project roles anyway.
#
# The control VM sidesteps all of it: it runs as sandbox-control-sa with
# cloud-platform scope, so gsutil/gcloud get credentials from the GCE metadata
# server. Those never expire and never need reauth. The only grant it needed was
# bucket-scoped roles/storage.objectAdmin on $RELEASE_BUCKET (bucket IAM, which
# roles/storage.admin can set — no project admin required).
#
# Nothing about rollout.sh changes: it already talks to the fleet exclusively
# over ssh to $CONTROL_NAME plus gsutil, and the control host can ssh to itself,
# so it runs there unmodified.
#
# What THIS machine needs — the whole list, verified with gcloud off $PATH and
# CLOUDSDK_CONFIG pointed at an empty dir:
#   * ssh + a key authorized on the control VM
#   * rsync, bash, make            (NO gcloud, NO gsutil, NO local git needed —
#                                   the release sha is derived on the VM from the
#                                   .git dir we rsync)
#   * this repo, including .git and the gitignored infra/gcp/fleet-secrets.env
#   * a network path to the control VM. `sandbox-control` is a TAILSCALE name
#     here, so on a machine that isn't on the tailnet, either join it or set
#     CONTROL_SSH_HOST=<reachable addr> (the VM also has a public IP).
# Keep it bash-3.2 clean (still /bin/bash on macOS) so it runs on any dev box.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$DIR/../.." && pwd)"
CONFIG_PATH="${SANDBOX_GCP_CONFIG:-$DIR/config.env}"
CONFIG_PATH="$(cd "$(dirname "$CONFIG_PATH")" && pwd)/$(basename "$CONFIG_PATH")"
# shellcheck source=config.env
source "$CONFIG_PATH"

# The working tree is mirrored to the control VM, so carry the selected config
# with it. Without this, a local config.us.env invocation silently falls back to
# config.env when rollout.sh starts remotely.
case "$CONFIG_PATH" in
  "$REPO"/*) REMOTE_CONFIG="${CONFIG_PATH#"$REPO/"}" ;;
  *)
    echo "error: SANDBOX_GCP_CONFIG must be inside $REPO" >&2
    exit 1
    ;;
esac
case "$REMOTE_CONFIG" in
  *[!A-Za-z0-9_./-]*)
    echo "error: unsupported characters in config path: $REMOTE_CONFIG" >&2
    exit 1
    ;;
esac

SSH_HOST="${CONTROL_SSH_HOST:-$CONTROL_NAME}"
REMOTE_SRC="${REMOTE_SRC:-sandbox-src}"
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=15 -o StrictHostKeyChecking=accept-new)
TARGET="${SSH_USER}@${SSH_HOST}"

sshc() { ssh "${SSH_OPTS[@]}" "$TARGET" "$@"; }

sshc true 2>/dev/null || {
  echo "error: cannot ssh to $TARGET (tailnet up? route approved?)" >&2
  exit 1
}

# The toolchain is not part of the control VM's image, so check rather than
# assume — a missing Go shows up otherwise as an opaque `make build-linux` fail
# in the middle of a rollout.
missing="$(sshc 'for t in git make rsync; do command -v $t >/dev/null || echo $t; done
                 /usr/local/go/bin/go version >/dev/null 2>&1 || command -v go >/dev/null || echo go')"
if [ -n "$missing" ]; then
  echo "error: $SSH_HOST is missing:$(echo " $missing" | tr '\n' ' ')" >&2
  echo "  install with:" >&2
  echo "    ssh $TARGET 'sudo apt-get update && sudo apt-get install -y build-essential git rsync'" >&2
  echo "    ssh $TARGET 'curl -fsSLO https://go.dev/dl/go1.25.3.linux-amd64.tar.gz && sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.25.3.linux-amd64.tar.gz'" >&2
  exit 1
fi

# Push the working tree, not a git fetch: the repo is private and this keeps
# GitHub credentials off the VM entirely. fleet-secrets.env is gitignored but
# IS synced — rollout.sh preflight-fails without GATEWAY_TOKEN. General-purpose
# .env files are explicitly excluded so unrelated provider credentials never
# leave the operator machine. Do not delete remote files from this wrapper;
# releases are content-addressed and a deletion here is needlessly destructive.
# node_modules/bin/ are excluded (~430 MiB of the 473 MiB tree) and rebuilt or
# unneeded remotely.
echo ">> sync $REPO -> $TARGET:~/$REMOTE_SRC"
rsync -az \
  --exclude 'node_modules/' --exclude '/bin/' --exclude 'graphify-out/' \
  --exclude 'tests/results/' \
  --exclude '/.env' --exclude '/.env.*' \
  --exclude '*.tgz' --exclude '/production_*' --exclude '.DS_Store' \
  -e "ssh ${SSH_OPTS[*]}" \
  "$REPO/" "${TARGET}:${REMOTE_SRC}/"

echo ">> rollout.sh $* (on $SSH_HOST)"
# Allocate a tty only when we have one, so rollout.sh's progress streams and
# Ctrl-C reaches it interactively — but `make ... | tee` doesn't get a
# "Pseudo-terminal will not be allocated" warning on every run.
#
# Spelled as two branches rather than an optional `-t` collected in an array:
# under `set -u`, expanding an EMPTY array ("${TTY_OPT[@]}") is an unbound-variable
# error in bash 3.2, which is still /bin/bash on macOS. This script has to run on
# whatever dev machine you happen to be on, so don't reintroduce that.
REMOTE_CMD="cd ~/${REMOTE_SRC}/infra/gcp && SANDBOX_GCP_CONFIG=\$HOME/${REMOTE_SRC}/${REMOTE_CONFIG} PATH=\$PATH:/usr/local/go/bin ./rollout.sh $*"
if [ -t 0 ]; then
  ssh -t "${SSH_OPTS[@]}" "$TARGET" "$REMOTE_CMD"
else
  ssh "${SSH_OPTS[@]}" "$TARGET" "$REMOTE_CMD"
fi
