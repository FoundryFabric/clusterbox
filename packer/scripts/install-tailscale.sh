#!/usr/bin/env bash
# install-tailscale.sh — Install Tailscale (stable channel) and enable the
# systemd service.
#
# IMPORTANT: `tailscale up` is NOT called here.  The node joins the tailnet
# at provision time via Pulumi, which supplies the auth key and any ACL tags.
set -euo pipefail

log() { echo "[tailscale-install] $*"; }

# ---------------------------------------------------------------------------
# 1. Install via the official Tailscale apt repository (stable channel)
# ---------------------------------------------------------------------------
log "Adding Tailscale apt repository (stable)..."

# Fetch the signing key and repo definition in one shot (official method).
curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.noarm.gpg \
  | gpg --dearmor -o /usr/share/keyrings/tailscale-archive-keyring.gpg 2>/dev/null \
  || curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.gpg \
       | gpg --dearmor -o /usr/share/keyrings/tailscale-archive-keyring.gpg

curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.list \
  | tee /etc/apt/sources.list.d/tailscale.list > /dev/null

apt-get update -qq
apt-get install -y -qq tailscale

# ---------------------------------------------------------------------------
# 2. Enable the tailscaled service so it starts on boot
#    The node is NOT connected here — `tailscale up` is called by Pulumi.
# ---------------------------------------------------------------------------
log "Enabling tailscaled service (not starting)..."
systemctl enable tailscaled

# Verify the binary is present
TAILSCALE_VERSION=$(tailscale version 2>/dev/null | head -1 || echo "unknown")
log "Tailscale installed: ${TAILSCALE_VERSION}"
log "tailscaled enabled — run 'tailscale up --authkey=<key>' at provision time to activate."
