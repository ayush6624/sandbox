#!/usr/bin/env bash
# Build the snapshot that Harbor trials are cloned from.
#
# Provision ONE sandbox by hand — install Docker, pre-pull the base images tasks
# reach for — snapshot it, and throw the sandbox away. Every trial then clones
# that snapshot (~700 ms, rootfs is a reflink), so the install is paid once for
# the whole campaign instead of once per task.
#
# This replaces baking Docker into the base rootfs, which would cost a worker
# image rebake plus a MIG roll for every toolchain change. Here the toolchain is
# a snapshot id: rebuild it whenever, roll nothing. What it needs from the image
# is only passwordless sudo in the guest (cmd/sandbox/installagent.go).
#
# Run it from the control VM — a laptop tunnel adds hundreds of ms of RTT.
#
#     ./prepare-docker-snapshot.sh
#     ./prepare-docker-snapshot.sh --mem-mib 8192 --pull ubuntu:24.04
#
# It prints the snapshot id on stdout and nothing else there, so:
#
#     export SANDBOX_SNAPSHOT_ID="$(./prepare-docker-snapshot.sh)"
set -euo pipefail

API="${SANDBOX_API_URL:-http://10.160.0.100:9090}"
KEY="${SANDBOX_API_KEY:-}"
VCPUS=2
MEM_MIB=4096
NAME="harbor-docker-base"
# Terminal-Bench tasks overwhelmingly build FROM one of these, and a pre-pulled
# layer is the difference between a task's compose build hitting the network and
# not. Cheap to extend; each one costs snapshot size, not clone time.
PULL_IMAGES=(ubuntu:24.04 python:3.13-slim)

while [ $# -gt 0 ]; do
  case "$1" in
    --mem-mib) MEM_MIB="$2"; shift 2 ;;
    --vcpus)   VCPUS="$2"; shift 2 ;;
    --name)    NAME="$2"; shift 2 ;;
    --pull)    PULL_IMAGES+=("$2"); shift 2 ;;
    --no-pull) PULL_IMAGES=(); shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [ -z "$KEY" ]; then
  echo "SANDBOX_API_KEY is unset" >&2
  exit 2
fi

# Everything progress-ish goes to stderr so stdout carries only the snapshot id.
log() { echo ">> $*" >&2; }

api() { # api <METHOD> <PATH> [BODY]
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS --fail-with-body -m "${API_TIMEOUT:-600}" -X "$method" "${API}${path}"
              -H "Authorization: Bearer ${KEY}")
  [ -n "$body" ] && args+=(-H "Content-Type: application/json" -d "$body")
  [ "$method" != GET ] && args+=(-H "Idempotency-Key: $(cat /proc/sys/kernel/random/uuid)")
  curl "${args[@]}"
}

jget() { python3 -c 'import json,sys; print(json.load(sys.stdin)["'"$1"'"])'; }

# exec_guest runs a command in the guest and fails the script on a nonzero exit
# code. `--fail-with-body` above only catches HTTP errors; a command that runs
# fine and exits 1 is a 200, so the exit code has to be checked separately or a
# failed apt-get would be snapshotted as if it had worked.
exec_guest() { # exec_guest <sandbox-id> <timeout-sec> <command>
  local id="$1" timeout="$2" cmd="$3" out rc
  out="$(api POST "/sandboxes/${id}/exec" \
    "$(python3 -c 'import json,sys; print(json.dumps({"cmd": sys.argv[1], "timeout_sec": int(sys.argv[2])}))' \
       "$cmd" "$timeout")")"
  rc="$(printf '%s' "$out" | python3 -c 'import json,sys; d=json.load(sys.stdin); sys.stderr.write(d["stdout"][-4000:] + d["stderr"][-4000:]); print(d["exit_code"])')"
  [ "$rc" = 0 ] || { echo "guest command failed (exit $rc): $cmd" >&2; return 1; }
}

log "creating prep sandbox (${VCPUS} vcpu / ${MEM_MIB} MiB)"
# A long apt-get or docker pull is quiet on the API and reads as an idle
# sandbox, and a hibernate mid-install wakes on a new guest IP underneath
# dockerd. The legacy API spells "never hibernate" as -1, but /v1 rejects
# negative lifecycle durations (internal/apiv1/handler.go), so the only way to
# say it through the public API is a window longer than the sandbox's own TTL.
SBX="$(api POST /v1/sandboxes "$(cat <<JSON
{"name": "${NAME}-prep",
 "lifecycle": {"ttl_seconds": 7200, "idle_timeout_seconds": 86400},
 "resources": {"vcpu": ${VCPUS}, "memory_mib": ${MEM_MIB}}}
JSON
)" | jget id)"
log "prep sandbox ${SBX}"
cleanup() { log "deleting prep sandbox ${SBX}"; api DELETE "/v1/sandboxes/${SBX}" >/dev/null 2>&1 || true; }
trap cleanup EXIT

log "checking passwordless sudo"
exec_guest "$SBX" 30 'sudo -n true'

log "installing docker (~1-2 min)"
# The docker socket must be group `sandbox`, NOT the conventional `docker`
# group: sandboxd runs every exec'd command with an explicit
# Credential{Uid, Gid} and NO supplementary groups
# (cmd/sandboxd/guestuser_linux.go), so the guest's primary gid is the only
# group it ever has and `usermod -aG docker sandbox` changes nothing that the
# API can reach. Without this, every `docker` call in a task fails with
# "permission denied while trying to connect to the docker API".
#
# It has to be set on docker.SOCKET, not in daemon.json: Ubuntu's docker.io
# socket-activates dockerd, so systemd creates the socket with its own
# SocketGroup=docker and daemon.json's `group` is never consulted.
exec_guest "$SBX" 900 'export DEBIAN_FRONTEND=noninteractive
sudo apt-get update -qq
sudo apt-get install -y -qq docker.io docker-compose-v2
sudo install -d -m 0755 /etc/systemd/system/docker.socket.d
printf "[Socket]\nSocketGroup=sandbox\n" | sudo tee /etc/systemd/system/docker.socket.d/group.conf > /dev/null
sudo systemctl daemon-reload
sudo systemctl enable docker
sudo systemctl restart docker.socket
sudo systemctl restart docker'

# Verify as the ordinary guest user with a bare `docker` — that is exactly how
# Harbor's DinDComposeOps will call it, and `sudo docker` passing proves nothing.
log "verifying docker as the guest user"
exec_guest "$SBX" 300 'docker version --format "server {{.Server.Version}}"
docker compose version
docker run --rm hello-world | grep -i "working correctly"'

for image in ${PULL_IMAGES[@]+"${PULL_IMAGES[@]}"}; do
  log "pre-pulling ${image}"
  exec_guest "$SBX" 600 "docker pull ${image}"
done

log "guest disk after install"
exec_guest "$SBX" 30 'df -h / | tail -1'

log "snapshotting"
SNAP="$(api POST "/v1/sandboxes/${SBX}/snapshots" "{\"name\": \"${NAME}\"}" | jget id)"
log "snapshot ${SNAP} — clone trials from it with SANDBOX_SNAPSHOT_ID=${SNAP}"
echo "$SNAP"
