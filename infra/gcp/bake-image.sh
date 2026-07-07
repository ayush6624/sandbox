#!/usr/bin/env bash
# Bake a reusable GCE image for autoscaled Firecracker worker hosts.
#
#   ./bake-image.sh          # build image family $WORKER_IMAGE_FAMILY
#   ./bake-image.sh clean    # delete a leftover bake VM (if a run died midway)
#
# Boots a throwaway non-spot VM (nested virt, NO data disk), runs the standard
# host bootstrap (firecracker + kernel + base rootfs) skipping the agent bake,
# installs a pinned Nomad client, cleans up, then captures the boot disk as an
# image in family $WORKER_IMAGE_FAMILY. mig.sh's instance template references
# the family, so a re-bake + rolling-replace ships a new image.
#
# Baked: firecracker, guest kernel, base rootfs at /opt/fc, nomad client.
# NOT baked: sandbox/sandboxd binaries (pulled from GCS by the Nomad job),
# per-VM config/tokens, sandboxd-in-rootfs (baked at job start). The base rootfs
# lands on the boot disk at bake time; startup-worker.sh copies it onto the
# per-instance XFS data disk on first boot.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$DIR/../.." && pwd)"
# shellcheck source=config.env
source "$DIR/config.env"

BAKE_VM="${BAKE_VM:-sandbox-bake}"
UBUNTU_FAMILY="${IMAGE_FAMILY:-ubuntu-2404-lts-amd64}"   # base image to boot the bake VM
UBUNTU_PROJECT="${IMAGE_PROJECT:-ubuntu-os-cloud}"
WORKER_FAMILY="${WORKER_IMAGE_FAMILY:-sandbox-worker}"   # family we PRODUCE
NOMAD_VERSION="${NOMAD_VERSION:-1.7.7}"
GC=(gcloud --project="$PROJECT")
sshx() { "${GC[@]}" compute ssh "${SSH_USER}@${BAKE_VM}" --zone="$ZONE" --command="$1"; }

cmd_clean() {
  "${GC[@]}" compute instances delete "$BAKE_VM" --zone="$ZONE" --quiet 2>/dev/null || true
  echo ">> removed bake VM $BAKE_VM (if it existed)"
}

cmd_bake() {
  local ts image
  ts="$(date +%Y%m%d-%H%M%S)"
  image="${WORKER_FAMILY}-${ts}"

  echo ">> [1/6] create throwaway bake VM $BAKE_VM (non-spot, nested virt, no data disk)"
  "${GC[@]}" compute instances create "$BAKE_VM" \
    --zone="$ZONE" \
    --machine-type="${WORKER_MACHINE_TYPE:-n2-standard-8}" \
    --image-family="$UBUNTU_FAMILY" \
    --image-project="$UBUNTU_PROJECT" \
    --boot-disk-size="${DISK_SIZE:-256GB}" \
    --boot-disk-type="${DISK_TYPE:-pd-ssd}" \
    --enable-nested-virtualization \
    --no-service-account --no-scopes \
    --metadata="ssh-user=${SSH_USER}${SSH_PUBLIC_KEY:+,ssh-pubkey=$SSH_PUBLIC_KEY}" \
    --metadata-from-file=startup-script="$DIR/startup.sh"

  echo ">> waiting for SSH + first-boot user provisioning"
  local tries=0
  until sshx 'true' 2>/dev/null; do
    tries=$((tries + 1)); [ "$tries" -lt 40 ] || { echo "SSH never came up"; exit 1; }
    sleep 5
  done

  echo ">> [2/6] copy repo bits (bin, configs, scripts) to the bake VM"
  local tar=/tmp/sandbox-bake-src.tgz
  ( cd "$REPO" && tar czf "$tar" bin configs scripts )
  "${GC[@]}" compute scp --zone="$ZONE" "$tar" "${SSH_USER}@${BAKE_VM}:/tmp/src.tgz"
  sshx 'mkdir -p ~/sandbox && tar xzf /tmp/src.tgz -C ~/sandbox && rm /tmp/src.tgz'

  echo ">> [3/6] host bootstrap (firecracker + kernel + rootfs, skip agent) — ~5 min"
  sshx 'cd ~/sandbox && SKIP_INSTALL_AGENT=1 bash scripts/gcp-host-bootstrap.sh'

  echo ">> [4/6] install pinned Nomad client v${NOMAD_VERSION}"
  install_nomad

  echo ">> [5/6] cleanup (build artifacts, logs, ssh host keys, machine-id)"
  sshx 'sudo bash -s' <<'CLEANUP'
set -e
rm -rf /opt/fc/rootfs-build /tmp/src.tgz "$HOME"/sandbox/bin 2>/dev/null || true
apt-get clean
truncate -s0 /var/log/startup-script.log 2>/dev/null || true
rm -f /etc/ssh/ssh_host_* 2>/dev/null || true
: > /etc/machine-id 2>/dev/null || true
CLEANUP

  echo ">> [6/6] stop VM and capture image $image (family $WORKER_FAMILY)"
  "${GC[@]}" compute instances stop "$BAKE_VM" --zone="$ZONE"
  "${GC[@]}" compute images create "$image" \
    --source-disk="$BAKE_VM" --source-disk-zone="$ZONE" \
    --family="$WORKER_FAMILY"
  "${GC[@]}" compute instances delete "$BAKE_VM" --zone="$ZONE" --quiet
  echo ">> done. Image $image is now the head of family $WORKER_FAMILY."
  echo "   mig.sh builds the instance template from --image-family=$WORKER_FAMILY."
}

# install_nomad installs the pinned Nomad client binary + a client.hcl TEMPLATE
# (nomad-server-ip filled at boot by startup-worker.sh) + a systemd unit, left
# DISABLED (startup-worker.sh enables it once the config is rendered).
install_nomad() {
  sshx "sudo NOMAD_VERSION=${NOMAD_VERSION} bash -s" <<'NOMAD'
set -euo pipefail
cd /tmp
curl -fsSL -o nomad.zip "https://releases.hashicorp.com/nomad/${NOMAD_VERSION}/nomad_${NOMAD_VERSION}_linux_amd64.zip"
command -v unzip >/dev/null || DEBIAN_FRONTEND=noninteractive apt-get install -y -qq unzip
unzip -o nomad.zip >/dev/null
install -m 0755 nomad /usr/local/bin/nomad
rm -f nomad nomad.zip
mkdir -p /etc/nomad.d /opt/nomad/data
cat >/etc/nomad.d/client.hcl <<'HCL'
# Rendered at boot by startup-worker.sh (replaces __NOMAD_SERVER_IP__).
datacenter = "dc1"
data_dir   = "/opt/nomad/data"
client {
  enabled    = true
  node_class = "sandbox-worker"
  server_join { retry_join = ["__NOMAD_SERVER_IP__"] }
}
plugin "raw_exec" { config { enabled = true } }
HCL
cat >/etc/systemd/system/nomad.service <<'UNIT'
[Unit]
Description=Nomad client
After=network-online.target
Wants=network-online.target
[Service]
ExecStart=/usr/local/bin/nomad agent -config /etc/nomad.d
Restart=always
RestartSec=2
LimitNOFILE=65536
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl disable nomad 2>/dev/null || true
NOMAD
}

case "${1:-bake}" in
  bake)  cmd_bake ;;
  clean) cmd_clean ;;
  *) echo "usage: $0 {bake|clean}" >&2; exit 1 ;;
esac
