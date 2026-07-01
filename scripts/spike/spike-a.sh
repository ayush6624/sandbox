#!/usr/bin/env bash
# M0 Spike A — rootfs relocation (decides B1 PATCH-relocate vs B2 mount-namespace).
#
# Cold-boot a hand-driven Firecracker VM from the base rootfs, snapshot it, then
# restore with:  load(resume_vm=false) -> PATCH /drives/rootfs {new path} -> resume
# and prove the resumed guest's disk I/O lands on the *relocated clone*, not the
# original baked path.
#
# Isolated from the live `serve`: own API sockets, tap `spk0`, guest IP .240
# (outside the 172.16.0.10-73 pool). Cleans up after itself.
set -uo pipefail

WORK=/mnt/sandbox-data/spike
KERNEL=/opt/fc/vmlinux
BASE=/mnt/sandbox-data/base/devbox-rootfs.ext4
FC=/usr/local/bin/firecracker

TAP=spk0
GUEST_IP=172.16.0.240
GW=172.16.0.1
MASK=255.255.255.0
MAC=06:00:AC:10:00:F0

SRC_ROOTFS=$WORK/a-src.ext4
CLONE_ROOTFS=$WORK/a-clone.ext4
MEM=$WORK/a-mem.bin
STATE=$WORK/a-state.bin
SOCK1=$WORK/a-fc1.sock
SOCK2=$WORK/a-fc2.sock
LOG1=$WORK/a-fc1.log
LOG2=$WORK/a-fc2.log

say() { echo -e "\n\033[1;36m== $* ==\033[0m"; }
ok()  { echo -e "  \033[1;32mPASS\033[0m $*"; }
bad() { echo -e "  \033[1;31mFAIL\033[0m $*"; }

# api SOCK METHOD PATH [JSON] — returns body; prints http code; fails on >=400
api() {
  local sock=$1 method=$2 path=$3 body=${4:-}
  local args=(-s -w '\n%{http_code}' -X "$method" --unix-socket "$sock" "http://localhost$path")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  local out code
  out=$(sudo curl "${args[@]}"); code=$(echo "$out" | tail -1); body=$(echo "$out" | sed '$d')
  if [ "$code" -ge 400 ]; then echo "  API $method $path -> HTTP $code: $body" >&2; return 1; fi
  echo "$body"
}

cleanup() {
  say "cleanup"
  sudo pkill -f "$SOCK1" 2>/dev/null
  sudo pkill -f "$SOCK2" 2>/dev/null
  sleep 0.3
  sudo ip link del "$TAP" 2>/dev/null
  sudo umount "$WORK/mnt" 2>/dev/null
  sudo rm -f "$SOCK1" "$SOCK2"
}
trap cleanup EXIT

make_tap() {
  sudo ip link del "$TAP" 2>/dev/null
  sudo ip tuntap add dev "$TAP" mode tap
  sudo ip link set "$TAP" master br-fc
  sudo ip link set "$TAP" up
  sudo ip link set br-fc up
}

wait_health() {
  local i
  for i in $(seq 1 100); do
    if curl -s -m 1 "http://$GUEST_IP:8090/health" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  return 1
}

# exec a command in the guest via sandboxd; prints stdout
guest_exec() {
  curl -s -m 10 -X POST "http://$GUEST_IP:8090/exec" \
    -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg c "$1" '{cmd:$c}')" | jq -r '.stdout // ""'
}

rm -f "$SRC_ROOTFS" "$CLONE_ROOTFS" "$MEM" "$STATE" "$SOCK1" "$SOCK2"
mkdir -p "$WORK/mnt"

say "0. reflink base -> src rootfs"
cp --reflink=always "$BASE" "$SRC_ROOTFS" && ok "src rootfs ready ($(du -h --apparent-size "$SRC_ROOTFS" | cut -f1))"

say "1. cold boot source VM (hand-driven FC API)"
make_tap
sudo "$FC" --api-sock "$SOCK1" --id spikevm-a --no-seccomp >"$LOG1" 2>&1 &
sleep 0.5
BOOTARGS="console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw ip=$GUEST_IP::$GW:$MASK:spikevm:eth0:off"
api "$SOCK1" PUT /boot-source "$(jq -nc --arg k "$KERNEL" --arg a "$BOOTARGS" '{kernel_image_path:$k,boot_args:$a}')" >/dev/null || exit 1
api "$SOCK1" PUT /drives/rootfs "$(jq -nc --arg p "$SRC_ROOTFS" '{drive_id:"rootfs",path_on_host:$p,is_root_device:true,is_read_only:false}')" >/dev/null || exit 1
api "$SOCK1" PUT /machine-config '{"vcpu_count":2,"mem_size_mib":1024}' >/dev/null || exit 1
api "$SOCK1" PUT /network-interfaces/eth0 "$(jq -nc --arg t "$TAP" --arg m "$MAC" '{iface_id:"eth0",host_dev_name:$t,guest_mac:$m}')" >/dev/null || exit 1
api "$SOCK1" PUT /actions '{"action_type":"InstanceStart"}' >/dev/null || exit 1
if wait_health; then ok "source guest booted, sandboxd healthy at $GUEST_IP:8090"; else bad "source never became healthy"; tail -20 "$LOG1"; exit 1; fi
echo "  guest uname: $(guest_exec 'uname -a')"

say "2. pause + snapshot source"
api "$SOCK1" PATCH /vm '{"state":"Paused"}' >/dev/null || exit 1
t0=$(date +%s%3N)
api "$SOCK1" PUT /snapshot/create "$(jq -nc --arg s "$STATE" --arg m "$MEM" '{snapshot_type:"Full",snapshot_path:$s,mem_file_path:$m}')" >/dev/null || exit 1
echo "  snapshot written in $(( $(date +%s%3N) - t0 ))ms  (mem=$(du -h "$MEM"|cut -f1) state=$(du -h "$STATE"|cut -f1))"
sudo pkill -f "$SOCK1"; sleep 0.5
sudo ip link del "$TAP" 2>/dev/null
ok "source killed, tap removed"

say "3. build clone rootfs + plant divergent markers offline"
cp --reflink=always "$SRC_ROOTFS" "$CLONE_ROOTFS"
# Plant a fresh file that did NOT exist at snapshot time, with different contents
# in src vs clone, so a post-resume read hits the block device (not stale cache).
sudo mount -o loop "$SRC_ROOTFS"   "$WORK/mnt"; echo "i-am-the-ORIGINAL"      | sudo tee "$WORK/mnt/root/WHICH_DISK" >/dev/null; sudo umount "$WORK/mnt"
sudo mount -o loop "$CLONE_ROOTFS" "$WORK/mnt"; echo "i-am-the-RELOCATED-CLONE"| sudo tee "$WORK/mnt/root/WHICH_DISK" >/dev/null; sudo umount "$WORK/mnt"
ok "clone reflinked; markers planted (src=ORIGINAL clone=RELOCATED-CLONE)"

say "4. RESTORE with drive relocation: load(resume=false) -> PATCH /drives -> resume"
make_tap   # reuse same tap/IP (single restore; isolating the rootfs question)
sudo "$FC" --api-sock "$SOCK2" --id spikevm-a2 --no-seccomp >"$LOG2" 2>&1 &
sleep 0.5
t0=$(date +%s%3N)
LOADBODY=$(jq -nc --arg s "$STATE" --arg m "$MEM" '{snapshot_path:$s,mem_backend:{backend_type:"File",backend_path:$m},enable_diff_snapshots:false,resume_vm:false}')
if api "$SOCK2" PUT /snapshot/load "$LOADBODY" >/dev/null; then ok "snapshot loaded (resume_vm=false)"; else bad "snapshot load failed"; tail -20 "$LOG2"; exit 1; fi

echo "  -> PATCH /drives/rootfs to CLONE path ($CLONE_ROOTFS)"
if api "$SOCK2" PATCH /drives/rootfs "$(jq -nc --arg p "$CLONE_ROOTFS" '{drive_id:"rootfs",path_on_host:$p}')" >/dev/null; then
  ok "PATCH /drives/rootfs accepted  <<< B1 relocation is supported"
  RELOCATED=yes
else
  bad "PATCH /drives/rootfs REJECTED  <<< B1 not viable, fall back to B2 (mount-ns)"
  RELOCATED=no
fi

echo "  -> resume"
if api "$SOCK2" PATCH /vm '{"state":"Resumed"}' >/dev/null; then ok "resumed in $(( $(date +%s%3N) - t0 ))ms"; else bad "resume failed"; tail -30 "$LOG2"; exit 1; fi

if wait_health; then ok "restored guest healthy at $GUEST_IP:8090"; else bad "restored guest never healthy"; tail -30 "$LOG2"; exit 1; fi

say "5. PROOF: which disk is the resumed guest actually reading/writing?"
READ_BACK=$(guest_exec 'cat /root/WHICH_DISK')
echo "  guest reads /root/WHICH_DISK = '$READ_BACK'"
guest_exec 'echo "written-by-resumed-guest-$(date +%s)" > /root/PROOF_FROM_GUEST; sync' >/dev/null
PROOF_LINE=$(guest_exec 'cat /root/PROOF_FROM_GUEST')
echo "  guest wrote /root/PROOF_FROM_GUEST = '$PROOF_LINE'"

say "6. verdict"
if [ "$RELOCATED" = yes ] && [ "$READ_BACK" = "i-am-the-RELOCATED-CLONE" ]; then
  ok "B1 CONFIRMED: guest reads the RELOCATED clone after PATCH+resume"
else
  bad "guest did NOT read the relocated clone (read='$READ_BACK', relocated=$RELOCATED)"
fi

# Offline check: the guest's write must appear in the CLONE image, not the SRC.
sudo pkill -f "$SOCK2"; sleep 0.5
sudo mount -o loop "$CLONE_ROOTFS" "$WORK/mnt"; C=$(sudo cat "$WORK/mnt/root/PROOF_FROM_GUEST" 2>/dev/null); sudo umount "$WORK/mnt"
sudo mount -o loop "$SRC_ROOTFS"   "$WORK/mnt"; S=$(sudo cat "$WORK/mnt/root/PROOF_FROM_GUEST" 2>/dev/null); sudo umount "$WORK/mnt"
echo "  offline: PROOF in CLONE image = '${C:-<absent>}'"
echo "  offline: PROOF in SRC   image = '${S:-<absent>}'"
if [ -n "$C" ] && [ -z "$S" ]; then
  ok "I/O ISOLATION CONFIRMED: guest write landed in CLONE only, SRC untouched"
else
  bad "I/O isolation NOT clean (clone='$C' src='$S')"
fi

say "SPIKE A DONE (relocation=$RELOCATED)"
