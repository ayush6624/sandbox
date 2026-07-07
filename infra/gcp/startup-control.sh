#!/usr/bin/env bash
# GCE startup-script for the sandbox control VM. Runs as root on first boot;
# idempotent. Same base as startup.sh (sudo user + Tailscale) PLUS Tailscale
# subnet-router setup so the laptop can reach sandbox forwarded ports on the
# VPC-internal worker IPs — the workers themselves are NOT on the tailnet, only
# this control VM is (it advertises the VPC subnet).
# Output: /var/log/startup-script.log
set -euxo pipefail
exec > >(tee -a /var/log/startup-script.log) 2>&1

meta() {
  curl -fsS -H "Metadata-Flavor: Google" \
    "http://metadata.google.internal/computeMetadata/v1/instance/attributes/$1" 2>/dev/null || true
}

SSH_USER="$(meta ssh-user)"; SSH_USER="${SSH_USER:-ayush}"
TS_KEY="$(meta tailscale-authkey)"
SSH_PUBKEY="$(meta ssh-pubkey)"
VPC_SUBNET_CIDR="$(meta vpc-subnet-cidr)"
INSTANCE_NAME="$(curl -fsS -H "Metadata-Flavor: Google" \
  "http://metadata.google.internal/computeMetadata/v1/instance/name" 2>/dev/null || hostname -s)"

# 1. sudo user
id "$SSH_USER" &>/dev/null || useradd --create-home --shell /bin/bash "$SSH_USER"
printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$SSH_USER" > "/etc/sudoers.d/90-$SSH_USER"
chmod 0440 "/etc/sudoers.d/90-$SSH_USER"
visudo -cf "/etc/sudoers.d/90-$SSH_USER"

# 2. optional SSH key
if [ -n "$SSH_PUBKEY" ]; then
  install -d -m 700 -o "$SSH_USER" -g "$SSH_USER" "/home/$SSH_USER/.ssh"
  printf '%s\n' "$SSH_PUBKEY" > "/home/$SSH_USER/.ssh/authorized_keys"
  chmod 600 "/home/$SSH_USER/.ssh/authorized_keys"
  chown "$SSH_USER:$SSH_USER" "/home/$SSH_USER/.ssh/authorized_keys"
fi

# 3. Tailscale as a subnet router. IP forwarding must be on for advertised
# routes to work; the route still needs one-time approval in the Tailscale
# admin console (README notes this).
if [ -n "$TS_KEY" ]; then
  command -v tailscale &>/dev/null || curl -fsSL https://tailscale.com/install.sh | sh
  echo 'net.ipv4.ip_forward=1' > /etc/sysctl.d/99-tailscale.conf
  echo 'net.ipv6.conf.all.forwarding=1' >> /etc/sysctl.d/99-tailscale.conf
  sysctl -p /etc/sysctl.d/99-tailscale.conf
  systemctl enable --now tailscaled
  ROUTES_ARG=""
  [ -n "$VPC_SUBNET_CIDR" ] && ROUTES_ARG="--advertise-routes=${VPC_SUBNET_CIDR}"
  tailscale up --authkey="$TS_KEY" --hostname="$INSTANCE_NAME" --ssh --accept-routes $ROUTES_ARG
else
  echo "No tailscale-authkey metadata — skipping Tailscale."
fi

echo "startup-control finished OK"
