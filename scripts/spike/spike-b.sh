#!/usr/bin/env bash
# M0 Spike B — MMDS-driven in-guest reidentification (the collision-free clone flow).
#
# Boot a source VM with MMDS enabled + a baked guest thaw-agent (systemd). Snapshot.
# Then restore as an IDENTITY-NEUTRAL clone:
#   load(resume=false) -> PATCH /drives/rootfs {clone} -> PATCH /network-interfaces/eth0
#   {new tap} -> PUT /mmds {new ip/mac/gw, gen++} -> resume, with the new tap left
#   UNBRIDGED until the guest self-reconfigures, then attach to br-fc.
# Proves: clone comes up on a FRESH IP/MAC/tap, reachable on the new IP, old IP dead,
# and the source identity never touches the shared bridge.
#
# Source identity: .240 / ...F0 / tap spkb0   Clone identity: .241 / ...F1 / tap spkb1
set -uo pipefail

WORK=/mnt/sandbox-data/spike
KERNEL=/opt/fc/vmlinux
BASE=/mnt/sandbox-data/base/devbox-rootfs.ext4
FC=/usr/local/bin/firecracker
THAW=$WORK/thaw.sh

GW=172.16.0.1; PFX=24
SRC_IP=172.16.0.240; SRC_MAC=06:00:AC:10:00:F0; SRC_TAP=spkb0
CLONE_IP=172.16.0.241; CLONE_MAC=06:00:AC:10:00:F1; CLONE_TAP=spkb1

SRC_ROOTFS=$WORK/b-src.ext4; CLONE_ROOTFS=$WORK/b-clone.ext4
MEM=$WORK/b-mem.bin; STATE=$WORK/b-state.bin
SOCK1=$WORK/b-fc1.sock; SOCK2=$WORK/b-fc2.sock
LOG1=$WORK/b-fc1.log; LOG2=$WORK/b-fc2.log

say(){ echo -e "\n\033[1;36m== $* ==\033[0m"; }
ok(){ echo -e "  \033[1;32mPASS\033[0m $*"; }
bad(){ echo -e "  \033[1;31mFAIL\033[0m $*"; }

api(){ local sock=$1 method=$2 path=$3 body=${4:-}
  local args=(-s -w '\n%{http_code}' -X "$method" --unix-socket "$sock" "http://localhost$path")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  local out code; out=$(sudo curl "${args[@]}"); code=$(echo "$out"|tail -1); body=$(echo "$out"|sed '$d')
  if [ "$code" -ge 400 ]; then echo "  API $method $path -> HTTP $code: $body" >&2; return 1; fi
  echo "$body"; }

cleanup(){ say cleanup
  sudo pkill -f "$SOCK1" 2>/dev/null; sudo pkill -f "$SOCK2" 2>/dev/null; sleep 0.3
  sudo ip link del "$SRC_TAP" 2>/dev/null; sudo ip link del "$CLONE_TAP" 2>/dev/null
  sudo rm -f "$SOCK1" "$SOCK2"; }
trap cleanup EXIT

mk_tap_bridged(){ sudo ip link del "$1" 2>/dev/null; sudo ip tuntap add dev "$1" mode tap
  sudo ip link set "$1" master br-fc; sudo ip link set "$1" up; sudo ip link set br-fc up; }
mk_tap_unbridged(){ sudo ip link del "$1" 2>/dev/null; sudo ip tuntap add dev "$1" mode tap; sudo ip link set "$1" up; }

health(){ local ip=$1 i; for i in $(seq 1 ${2:-100}); do curl -s -m1 "http://$ip:8090/health" >/dev/null 2>&1 && return 0; sleep 0.1; done; return 1; }
gexec(){ curl -s -m10 -X POST "http://$1:8090/exec" -H 'Content-Type: application/json' -d "$(jq -nc --arg c "$2" '{cmd:$c}')" | jq -r '.stdout // ""'; }

rm -f "$SRC_ROOTFS" "$CLONE_ROOTFS" "$MEM" "$STATE" "$SOCK1" "$SOCK2"

say "0. reflink base -> src rootfs"
cp --reflink=always "$BASE" "$SRC_ROOTFS" && ok "src ready"

say "1. cold boot source with MMDS enabled"
mk_tap_bridged "$SRC_TAP"
sudo "$FC" --api-sock "$SOCK1" --id spikevm-b --no-seccomp >"$LOG1" 2>&1 & sleep 0.5
BOOTARGS="console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw ip=$SRC_IP::$GW:255.255.255.0:spikevm:eth0:off"
api "$SOCK1" PUT /boot-source "$(jq -nc --arg k "$KERNEL" --arg a "$BOOTARGS" '{kernel_image_path:$k,boot_args:$a}')" >/dev/null || exit 1
api "$SOCK1" PUT /drives/rootfs "$(jq -nc --arg p "$SRC_ROOTFS" '{drive_id:"rootfs",path_on_host:$p,is_root_device:true,is_read_only:false}')" >/dev/null || exit 1
api "$SOCK1" PUT /machine-config '{"vcpu_count":2,"mem_size_mib":1024}' >/dev/null || exit 1
api "$SOCK1" PUT /network-interfaces/eth0 "$(jq -nc --arg t "$SRC_TAP" --arg m "$SRC_MAC" '{iface_id:"eth0",host_dev_name:$t,guest_mac:$m}')" >/dev/null || exit 1
# MMDS: V2, attached to eth0. Seed with the source identity (gen=1).
api "$SOCK1" PUT /mmds/config "$(jq -nc '{version:"V2",network_interfaces:["eth0"]}')" >/dev/null || exit 1
api "$SOCK1" PUT /mmds "$(jq -nc --arg ip "$SRC_IP" --arg mac "$SRC_MAC" --arg gw "$GW" --arg p "$PFX" '{ip:$ip,mac:$mac,gw:$gw,prefix:$p,gen:"1"}')" >/dev/null || exit 1
api "$SOCK1" PUT /actions '{"action_type":"InstanceStart"}' >/dev/null || exit 1
if health "$SRC_IP"; then ok "source healthy at $SRC_IP"; else bad "source never healthy"; tail -20 "$LOG1"; exit 1; fi

say "2. bake the guest thaw-agent (systemd) + verify MMDS reachable from guest"
B64=$(base64 -w0 "$THAW")
gexec "$SRC_IP" "echo $B64 | base64 -d > /usr/local/bin/thaw.sh && chmod +x /usr/local/bin/thaw.sh" >/dev/null
UNIT='[Unit]
Description=spike thaw agent
[Service]
ExecStart=/usr/local/bin/thaw.sh
Restart=always
[Install]
WantedBy=multi-user.target'
gexec "$SRC_IP" "printf '%s' \"$UNIT\" > /etc/systemd/system/thaw.service && systemctl daemon-reload && systemctl enable --now thaw" >/dev/null
# MMDS V2 needs a session token; confirm the guest can read its identity doc.
MMDS_TEST=$(gexec "$SRC_IP" 'ip route add 169.254.169.254/32 dev eth0 2>/dev/null; T=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" -H "X-metadata-token-ttl-seconds: 60"); curl -s -H "X-metadata-token: $T" -H "Accept: application/json" http://169.254.169.254/')
echo "  guest MMDS read: $MMDS_TEST"
if echo "$MMDS_TEST" | grep -q '"gen"'; then ok "guest can reach MMDS (V2 token flow works)"; else bad "guest cannot read MMDS"; fi
sleep 1  # let thaw apply gen=1 and record it
echo "  thaw log: $(gexec "$SRC_IP" 'journalctl -t thaw --no-pager | tail -1')"

say "3. pause + snapshot + kill source (thaw agent frozen into memory)"
api "$SOCK1" PATCH /vm '{"state":"Paused"}' >/dev/null || exit 1
api "$SOCK1" PUT /snapshot/create "$(jq -nc --arg s "$STATE" --arg m "$MEM" '{snapshot_type:"Full",snapshot_path:$s,mem_file_path:$m}')" >/dev/null || exit 1
ok "snapshot written"
sudo pkill -f "$SOCK1"; sleep 0.5; sudo ip link del "$SRC_TAP" 2>/dev/null
ok "source killed + tap removed (old identity fully gone from host)"

say "4. RESTORE as identity-neutral clone (.241 / ...F1 / spkb1)"
cp --reflink=always "$SRC_ROOTFS" "$CLONE_ROOTFS"
mk_tap_unbridged "$CLONE_TAP"       # UNBRIDGED: MMDS works, but no path to br-fc yet
ok "clone rootfs reflinked; $CLONE_TAP created UNBRIDGED"
sudo "$FC" --api-sock "$SOCK2" --id spikevm-b2 --no-seccomp >"$LOG2" 2>&1 & sleep 0.5
# network_overrides remaps the baked tap -> the clone's fresh tap at load time
# (the correct primitive; PATCH /network-interfaces can't change host_dev_name).
LOADBODY=$(jq -nc --arg s "$STATE" --arg m "$MEM" --arg t "$CLONE_TAP" \
  '{snapshot_path:$s,mem_backend:{backend_type:"File",backend_path:$m},enable_diff_snapshots:false,resume_vm:false,network_overrides:[{iface_id:"eth0",host_dev_name:$t}]}')
if api "$SOCK2" PUT /snapshot/load "$LOADBODY" >/dev/null; then ok "snapshot loaded with network_overrides -> $CLONE_TAP"; else bad "load w/ network_overrides failed"; tail -20 "$LOG2"; exit 1; fi
api "$SOCK2" PATCH /drives/rootfs "$(jq -nc --arg p "$CLONE_ROOTFS" '{drive_id:"rootfs",path_on_host:$p}')" >/dev/null || { bad "drive relocate failed"; exit 1; }
# Push the clone's NEW identity into MMDS (gen bump triggers the thaw agent).
api "$SOCK2" PUT /mmds "$(jq -nc --arg ip "$CLONE_IP" --arg mac "$CLONE_MAC" --arg gw "$GW" --arg p "$PFX" '{ip:$ip,mac:$mac,gw:$gw,prefix:$p,gen:"2"}')" >/dev/null || exit 1
api "$SOCK2" PATCH /vm '{"state":"Resumed"}' >/dev/null || { bad "resume failed"; tail -30 "$LOG2"; exit 1; }
ok "clone resumed (tap still UNBRIDGED — guest reconfiguring from MMDS)"

say "5. wait for guest to self-reconfigure to the new IP, THEN bridge the tap"
# Give the in-guest thaw agent a moment to apply gen=2 (it has no host contact yet).
sleep 2
sudo ip link set "$CLONE_TAP" master br-fc; sudo ip link set "$CLONE_TAP" up
ok "$CLONE_TAP attached to br-fc"
if health "$CLONE_IP" 100; then ok "clone reachable on NEW IP $CLONE_IP"; else bad "clone NOT reachable on $CLONE_IP"; fi

say "6. verdict — identity actually changed inside the guest"
echo "  guest ip addr: $(gexec "$CLONE_IP" 'ip -o -4 addr show eth0' )"
echo "  guest mac:     $(gexec "$CLONE_IP" 'cat /sys/class/net/eth0/address')"
echo "  thaw log tail: $(gexec "$CLONE_IP" 'journalctl -t thaw --no-pager | tail -2')"
GMAC=$(gexec "$CLONE_IP" 'cat /sys/class/net/eth0/address')
if [ "$GMAC" = "$CLONE_MAC" ]; then ok "guest MAC is the NEW clone MAC ($CLONE_MAC)"; else bad "guest MAC wrong ($GMAC)"; fi
# Old IP must be dead (nothing answers on .240).
if curl -s -m2 "http://$SRC_IP:8090/health" >/dev/null 2>&1; then bad "OLD IP $SRC_IP still answering (collision risk!)"; else ok "OLD IP $SRC_IP is dead (no collision)"; fi

say "SPIKE B DONE"
