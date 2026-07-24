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
GOLDEN_BAKE_VM="${GOLDEN_BAKE_VM:-sandbox-golden-bake}"
UBUNTU_FAMILY="${IMAGE_FAMILY:-ubuntu-2404-lts-amd64}"   # base image to boot the bake VM
UBUNTU_PROJECT="${IMAGE_PROJECT:-ubuntu-os-cloud}"
WORKER_FAMILY="${WORKER_IMAGE_FAMILY:-sandbox-worker}"   # family we PRODUCE
GOLDEN_FAMILY="${GOLDEN_DATA_IMAGE_FAMILY:-sandbox-golden-data}"  # golden data-disk family we PRODUCE
NOMAD_VERSION="${NOMAD_VERSION:-1.7.7}"
GC=(gcloud --project="$PROJECT")

# Drive the bake VM over direct SSH to its external IP with the configured
# ed25519 key (SSH_PUBLIC_KEY): startup.sh rewrites authorized_keys to exactly
# that key, which clobbers the key `gcloud compute ssh` provisions — so gcloud
# ssh/scp race and fail. EXT_IP is filled once the VM is created.
EXT_IP=""
KNOWN_HOSTS="$(mktemp)"
SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=accept-new -o "UserKnownHostsFile=$KNOWN_HOSTS" -o ConnectTimeout=10)
sshx() { ssh "${SSH_OPTS[@]}" "${SSH_USER}@${EXT_IP}" "$1"; }
trap 'rm -f "$KNOWN_HOSTS"' EXIT

cmd_clean() {
  "${GC[@]}" compute instances delete "$BAKE_VM" --zone="$ZONE" --quiet 2>/dev/null || true
  "${GC[@]}" compute instances delete "$GOLDEN_BAKE_VM" --zone="$ZONE" --quiet 2>/dev/null || true
  echo ">> removed bake VMs $BAKE_VM / $GOLDEN_BAKE_VM (if they existed)"
  echo ">> NOTE: leftover golden-bake data disks (sandbox-golden-bake-data-*) are"
  echo "   auto-delete=no; list + delete with: gcloud compute disks list --filter=name~golden-bake-data"
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

  EXT_IP="$("${GC[@]}" compute instances describe "$BAKE_VM" --zone="$ZONE" \
    --format='value(networkInterfaces[0].accessConfigs[0].natIP)')"
  echo ">> bake VM external IP $EXT_IP"

  echo ">> waiting for SSH + first-boot user provisioning (startup.sh authorizes the key)"
  local tries=0
  until sshx 'true' 2>/dev/null; do
    tries=$((tries + 1)); [ "$tries" -lt 60 ] || { echo "SSH never came up"; exit 1; }
    sleep 5
  done

  echo ">> [2/6] copy repo bits (bin, configs, scripts) to the bake VM"
  local tar=/tmp/sandbox-bake-src.tgz
  ( cd "$REPO" && tar czf "$tar" bin configs scripts )
  scp "${SSH_OPTS[@]}" "$tar" "${SSH_USER}@${EXT_IP}:/tmp/src.tgz"
  sshx 'mkdir -p ~/sandbox && tar xzf /tmp/src.tgz -C ~/sandbox && rm /tmp/src.tgz'

  echo ">> [3/6] host bootstrap (firecracker + kernel + rootfs, skip agent) — ~5 min"
  sshx 'cd ~/sandbox && SKIP_INSTALL_AGENT=1 bash scripts/gcp-host-bootstrap.sh'

  echo ">> [3b/6] bake sandboxd into the /opt/fc base rootfs (+ .agent-stamp)"
  # configs/devbox.json points rootfs_base at /opt/fc/devbox-rootfs.ext4 (the
  # bake asset dir; there's no data disk at bake time). Baking the agent HERE —
  # instead of at every host boot in the Nomad job — makes the boot-time
  # install-agent a true no-op (the stamp records the rootfs mtime+size and
  # short-circuits before mount) and, crucially, freezes the base rootfs mtime
  # so a baked golden snapshot stays adoptable. startup-worker.sh copies the
  # rootfs AND its .agent-stamp with --preserve=timestamps to keep this true.
  sshx 'cd ~/sandbox && sudo ./bin/sandbox install-agent --config configs/devbox.json --agent ./bin/sandboxd'

  echo ">> [4/6] install pinned Nomad client v${NOMAD_VERSION}"
  install_nomad

  echo ">> [5/6] cleanup (build artifacts, logs, ssh host keys, machine-id)"
  sshx 'sudo bash -s' <<'CLEANUP'
set -e
rm -rf /opt/fc/rootfs-build /tmp/src.tgz /home/*/sandbox/bin 2>/dev/null || true
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

# cmd_golden builds the golden DATA-DISK image (family $GOLDEN_FAMILY): a
# pre-populated XFS data disk carrying the base rootfs (sandboxd already baked)
# AND a pre-built golden snapshot + its golden.json manifest. A fresh worker
# whose data disk is created from this image ADOPTS the golden (imports the
# manifest, validates, clones) instead of copying the 2 GB rootfs and
# cold-building a golden — removing both from the scale-up path along with the
# slots_free=0 warming window.
#
# Run AFTER `./bake-image.sh bake`: it boots a throwaway VM from the worker
# image (which already has sandboxd baked into /opt/fc via step [3b/6]) with a
# named data disk, stages the rootfs, runs `serve` once standalone (no Nomad, no
# gateway, snapshot_bucket cleared so it stays offline) to build the golden +
# write the manifest onto the data disk, stops serve, then captures the DATA
# disk as an image. The VM boot-disk registry.db also gets a golden row, but it
# is discarded — the data-disk manifest is the sole source of truth, which keeps
# the worker boot image stateless.
cmd_golden() {
  local ts image disk
  ts="$(date +%Y%m%d-%H%M%S)"
  image="${GOLDEN_FAMILY}-${ts}"
  disk="sandbox-golden-bake-data-${ts}"

  echo ">> [1/5] create golden-bake VM $GOLDEN_BAKE_VM from family $WORKER_FAMILY (+ named data disk $disk)"
  "${GC[@]}" compute instances create "$GOLDEN_BAKE_VM" \
    --zone="$ZONE" \
    --machine-type="${WORKER_MACHINE_TYPE:-n2-standard-8}" \
    --image-family="$WORKER_FAMILY" --image-project="$PROJECT" \
    --boot-disk-size="${WORKER_BOOT_DISK_SIZE:-256GB}" --boot-disk-type=pd-ssd \
    --create-disk="name=${disk},device-name=sandbox-xfs,size=${WORKER_DATA_DISK_SIZE:-256GB},type=pd-ssd,auto-delete=no" \
    --enable-nested-virtualization \
    --no-service-account --no-scopes \
    --metadata="ssh-user=${SSH_USER}${SSH_PUBLIC_KEY:+,ssh-pubkey=$SSH_PUBLIC_KEY}" \
    --metadata-from-file=startup-script="$DIR/startup.sh"

  EXT_IP="$("${GC[@]}" compute instances describe "$GOLDEN_BAKE_VM" --zone="$ZONE" \
    --format='value(networkInterfaces[0].accessConfigs[0].natIP)')"
  echo ">> golden-bake VM external IP $EXT_IP"

  echo ">> waiting for SSH + first-boot user provisioning"
  local tries=0
  until sshx 'true' 2>/dev/null; do
    tries=$((tries + 1)); [ "$tries" -lt 60 ] || { echo "SSH never came up"; exit 1; }
    sleep 5
  done

  echo ">> [2/5] copy repo bits (bin, configs) to /opt/sandbox-bake"
  local tar=/tmp/sandbox-golden-src.tgz
  ( cd "$REPO" && tar czf "$tar" bin configs )
  scp "${SSH_OPTS[@]}" "$tar" "${SSH_USER}@${EXT_IP}:/tmp/gsrc.tgz"
  sshx 'sudo rm -rf /opt/sandbox-bake && sudo mkdir -p /opt/sandbox-bake && sudo tar xzf /tmp/gsrc.tgz -C /opt/sandbox-bake && rm /tmp/gsrc.tgz'

  echo ">> [3/5] stage rootfs + build golden on the data disk (serve standalone, ~1 min)"
  sshx 'sudo bash -s' <<'GOLDEN'
set -euxo pipefail
XFS_DEV=/dev/disk/by-id/google-sandbox-xfs
[ -e "$XFS_DEV" ] || { echo "FATAL: data disk $XFS_DEV not attached"; exit 1; }
mkfs.xfs -f "$XFS_DEV"
mkdir -p /mnt/sandbox-data
mount "$XFS_DEV" /mnt/sandbox-data
mkdir -p /mnt/sandbox-data/base /mnt/sandbox-data/rootfs /mnt/sandbox-data/snapshots
# Stage the base rootfs (sandboxd baked at image time) preserving mtime + the
# .agent-stamp, so its recorded BaseMtime stays valid on adopting workers and
# their boot-time install-agent stays a no-op. The stamp is REQUIRED here: a
# golden data disk without it would make adopters re-bake (bumping mtime) and
# invalidate the very golden they're adopting — so fail loudly if it's absent.
cp --sparse=always --preserve=mode,timestamps /opt/fc/devbox-rootfs.ext4 /mnt/sandbox-data/base/devbox-rootfs.ext4
cp --preserve=mode,timestamps /opt/fc/devbox-rootfs.ext4.agent-stamp /mnt/sandbox-data/base/devbox-rootfs.ext4.agent-stamp
# Offline golden build: clear snapshot_bucket so serve never reaches for GCS.
mkdir -p /var/lib/sandbox
sed 's#"snapshot_bucket":[[:space:]]*"[^"]*"#"snapshot_bucket": ""#' \
  /opt/sandbox-bake/configs/devbox-gcp.json > /tmp/golden-bake.json
cd /opt/sandbox-bake
chmod +x bin/sandbox bin/sandboxd
nohup ./bin/sandbox serve --config /tmp/golden-bake.json > /tmp/golden-serve.log 2>&1 &
SRV=$!
ok=0
for _ in $(seq 1 180); do
  if grep -q "creates are hot" /tmp/golden-serve.log; then ok=1; break; fi
  kill -0 "$SRV" 2>/dev/null || { echo "serve exited before golden was ready"; break; }
  sleep 1
done
[ "$ok" = 1 ] || { echo "=== golden-serve.log ==="; cat /tmp/golden-serve.log; exit 1; }
# Graceful stop so the golden artifacts + manifest are fully flushed to the disk.
./bin/sandbox stop-server || kill -TERM "$SRV" || true
for _ in $(seq 1 60); do kill -0 "$SRV" 2>/dev/null || break; sleep 1; done
test -f /mnt/sandbox-data/snapshots/golden.json || { echo "FATAL: golden.json manifest missing"; ls -la /mnt/sandbox-data/snapshots; exit 1; }
echo "=== golden manifest ==="; cat /mnt/sandbox-data/snapshots/golden.json
sync
umount /mnt/sandbox-data
echo "GOLDEN BAKE OK"
GOLDEN

  echo ">> [4/5] stop VM + capture DATA disk as image $image (family $GOLDEN_FAMILY)"
  "${GC[@]}" compute instances stop "$GOLDEN_BAKE_VM" --zone="$ZONE"
  "${GC[@]}" compute images create "$image" \
    --source-disk="$disk" --source-disk-zone="$ZONE" \
    --family="$GOLDEN_FAMILY"

  echo ">> [5/5] delete golden-bake VM + data disk"
  "${GC[@]}" compute instances delete "$GOLDEN_BAKE_VM" --zone="$ZONE" --quiet
  "${GC[@]}" compute disks delete "$disk" --zone="$ZONE" --quiet 2>/dev/null || true
  echo ">> done. Image $image is now the head of family $GOLDEN_FAMILY."
  echo "   mig.sh creates each worker's data disk from this family — ./mig.sh roll to adopt."
}

case "${1:-bake}" in
  bake)   cmd_bake ;;
  golden) cmd_golden ;;
  clean)  cmd_clean ;;
  *) echo "usage: $0 {bake|golden|clean}" >&2; exit 1 ;;
esac
