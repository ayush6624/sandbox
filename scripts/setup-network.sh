#!/usr/bin/env bash
# One-time host networking setup for Firecracker devbox VMs.
# Creates tap0, enables IP forwarding, sets up NAT + port forwarding.
set -euo pipefail

TAP_DEV="${TAP_DEV:-tap0}"
TAP_IP="${TAP_IP:-172.16.0.1}"
TAP_CIDR="${TAP_CIDR:-172.16.0.1/24}"
GUEST_IP="${GUEST_IP:-172.16.0.2}"
VITE_PORT="${VITE_PORT:-5173}"

# Detect the default outbound interface.
HOST_IFACE=$(ip route | grep default | awk '{print $5}' | head -1)
if [ -z "$HOST_IFACE" ]; then
  echo "ERROR: could not detect default network interface"
  exit 1
fi

echo "==> Setting up networking"
echo "  Host interface: ${HOST_IFACE}"
echo "  Tap device: ${TAP_DEV} (${TAP_CIDR})"
echo "  Guest IP: ${GUEST_IP}"

# --- Create tap device ---
if ip link show "$TAP_DEV" &>/dev/null; then
  echo "  ${TAP_DEV} already exists, skipping creation"
else
  echo "  Creating ${TAP_DEV}..."
  sudo ip tuntap add dev "$TAP_DEV" mode tap
  sudo ip addr add "$TAP_CIDR" dev "$TAP_DEV"
  sudo ip link set "$TAP_DEV" up
fi

# --- Enable IP forwarding ---
echo "  Enabling IP forwarding..."
sudo sysctl -w net.ipv4.ip_forward=1 >/dev/null
if [ ! -f /etc/sysctl.d/99-firecracker.conf ]; then
  echo "net.ipv4.ip_forward=1" | sudo tee /etc/sysctl.d/99-firecracker.conf >/dev/null
fi

# --- iptables NAT (masquerade guest traffic to internet) ---
echo "  Configuring iptables NAT..."
# Avoid duplicate rules by checking first.
if ! sudo iptables -t nat -C POSTROUTING -s 172.16.0.0/24 -o "$HOST_IFACE" -j MASQUERADE 2>/dev/null; then
  sudo iptables -t nat -A POSTROUTING -s 172.16.0.0/24 -o "$HOST_IFACE" -j MASQUERADE
fi
if ! sudo iptables -C FORWARD -i "$TAP_DEV" -o "$HOST_IFACE" -j ACCEPT 2>/dev/null; then
  sudo iptables -A FORWARD -i "$TAP_DEV" -o "$HOST_IFACE" -j ACCEPT
fi
if ! sudo iptables -C FORWARD -i "$HOST_IFACE" -o "$TAP_DEV" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null; then
  sudo iptables -A FORWARD -i "$HOST_IFACE" -o "$TAP_DEV" -m state --state RELATED,ESTABLISHED -j ACCEPT
fi

# --- Port forwarding: host:VITE_PORT -> guest:VITE_PORT ---
echo "  Configuring port forwarding (host:${VITE_PORT} -> ${GUEST_IP}:${VITE_PORT})..."
if ! sudo iptables -t nat -C PREROUTING -p tcp --dport "$VITE_PORT" -j DNAT --to-destination "${GUEST_IP}:${VITE_PORT}" 2>/dev/null; then
  sudo iptables -t nat -A PREROUTING -p tcp --dport "$VITE_PORT" -j DNAT --to-destination "${GUEST_IP}:${VITE_PORT}"
fi
# Also handle connections from localhost on the host itself.
if ! sudo iptables -t nat -C OUTPUT -p tcp --dport "$VITE_PORT" -d 127.0.0.1 -j DNAT --to-destination "${GUEST_IP}:${VITE_PORT}" 2>/dev/null; then
  sudo iptables -t nat -A OUTPUT -p tcp --dport "$VITE_PORT" -d 127.0.0.1 -j DNAT --to-destination "${GUEST_IP}:${VITE_PORT}"
fi

echo ""
echo "==> Networking ready!"
echo "  Tap: ${TAP_DEV} (${TAP_CIDR})"
echo "  Guest will get: ${GUEST_IP}"
echo "  Vite dev server will be accessible at host:${VITE_PORT}"
