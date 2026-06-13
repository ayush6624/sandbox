#!/usr/bin/env bash
# One-time host networking setup for Firecracker sandboxes (multi-VM bridge model).
# Creates br-fc, assigns the gateway IP, enables IP forwarding, sets up NAT.
# Per-sandbox tap devices and port forwards are managed by the sandbox server.
set -euo pipefail

BRIDGE="${BRIDGE:-br-fc}"
BRIDGE_CIDR="${BRIDGE_CIDR:-172.16.0.1/24}"
GUEST_SUBNET="${GUEST_SUBNET:-172.16.0.0/24}"

HOST_IFACE=$(ip route | grep default | awk '{print $5}' | head -1)
if [ -z "$HOST_IFACE" ]; then
  echo "ERROR: could not detect default network interface"
  exit 1
fi

echo "==> Host networking setup"
echo "  Host interface: ${HOST_IFACE}"
echo "  Bridge:         ${BRIDGE} (${BRIDGE_CIDR})"
echo "  Guest subnet:   ${GUEST_SUBNET}"

# --- Create bridge ---
if ip link show "$BRIDGE" &>/dev/null; then
  echo "  ${BRIDGE} already exists, skipping creation"
else
  echo "  Creating bridge ${BRIDGE}..."
  sudo ip link add name "$BRIDGE" type bridge
  sudo ip addr add "$BRIDGE_CIDR" dev "$BRIDGE"
  sudo ip link set "$BRIDGE" up
fi

# --- IP forwarding + route_localnet ---
# route_localnet=1 lets DNATed packets with src=127.0.0.1 route out non-loopback
# interfaces (needed for host:port → guest:port DNAT to work from `curl localhost`).
echo "  Enabling IP forwarding + route_localnet..."
sudo sysctl -w net.ipv4.ip_forward=1 >/dev/null
sudo sysctl -w net.ipv4.conf.all.route_localnet=1 >/dev/null
sudo tee /etc/sysctl.d/99-firecracker.conf >/dev/null <<EOF
net.ipv4.ip_forward=1
net.ipv4.conf.all.route_localnet=1
EOF

# --- iptables NAT (masquerade guest traffic to internet) ---
echo "  Configuring iptables NAT..."
if ! sudo iptables -t nat -C POSTROUTING -s "$GUEST_SUBNET" -o "$HOST_IFACE" -j MASQUERADE 2>/dev/null; then
  sudo iptables -t nat -A POSTROUTING -s "$GUEST_SUBNET" -o "$HOST_IFACE" -j MASQUERADE
fi
# MASQUERADE traffic going TO the bridge so host→guest connections (e.g. curl localhost:HOST_PORT
# after DNAT) get their src rewritten to the bridge IP; otherwise the guest tries to reply to
# 127.0.0.1 and the connection hangs.
if ! sudo iptables -t nat -C POSTROUTING -o "$BRIDGE" -j MASQUERADE 2>/dev/null; then
  sudo iptables -t nat -A POSTROUTING -o "$BRIDGE" -j MASQUERADE
fi
if ! sudo iptables -C FORWARD -i "$BRIDGE" -o "$HOST_IFACE" -j ACCEPT 2>/dev/null; then
  sudo iptables -A FORWARD -i "$BRIDGE" -o "$HOST_IFACE" -j ACCEPT
fi
if ! sudo iptables -C FORWARD -i "$HOST_IFACE" -o "$BRIDGE" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null; then
  sudo iptables -A FORWARD -i "$HOST_IFACE" -o "$BRIDGE" -m state --state RELATED,ESTABLISHED -j ACCEPT
fi
# Allow bridge-to-bridge (so a sandbox could in principle talk to another, if needed)
if ! sudo iptables -C FORWARD -i "$BRIDGE" -o "$BRIDGE" -j ACCEPT 2>/dev/null; then
  sudo iptables -A FORWARD -i "$BRIDGE" -o "$BRIDGE" -j ACCEPT
fi

# Ensure /var/lib/sandbox exists for the registry + rootfs copies
sudo mkdir -p /var/lib/sandbox/rootfs

echo ""
echo "==> Networking ready"
echo "  Bridge:       ${BRIDGE} (${BRIDGE_CIDR})"
echo "  Per-sandbox tap devices + port forwards are added at runtime by the server."
