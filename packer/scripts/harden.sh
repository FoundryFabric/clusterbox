#!/usr/bin/env bash
# harden.sh — OS hardening for clusterbox-base snapshot
#
# Responsibilities:
#   - Create non-root user `clusterbox` with sudo access
#   - Install the cluster SSH public key for `clusterbox`
#   - Lock down sshd: key-only auth, no root login, no password auth
#   - Configure UFW: default-deny in/out, allow SSH + Tailscale
#   - Install and configure fail2ban (sshd jail)
#   - Install auditd with sane defaults
#   - Install unattended-upgrades (security channel only)
#
# Environment variables:
#   CLUSTERBOX_SSH_PUBLIC_KEY — SSH public key to authorise for clusterbox user
set -euo pipefail

CLUSTERBOX_USER="clusterbox"

log() { echo "[harden] $*"; }

# ---------------------------------------------------------------------------
# 1. Package updates + base tooling
# ---------------------------------------------------------------------------
log "Updating package index and upgrading installed packages..."
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get upgrade -y -qq
apt-get install -y -qq \
  ufw \
  fail2ban \
  auditd \
  audispd-plugins \
  unattended-upgrades \
  apt-listchanges \
  ca-certificates \
  curl \
  gnupg \
  lsb-release

# ---------------------------------------------------------------------------
# 2. Create non-root user
# ---------------------------------------------------------------------------
log "Creating user ${CLUSTERBOX_USER}..."
if ! id "${CLUSTERBOX_USER}" &>/dev/null; then
  useradd \
    --create-home \
    --shell /bin/bash \
    --groups sudo \
    "${CLUSTERBOX_USER}"
fi

# Lock the password so only key auth works
passwd -l "${CLUSTERBOX_USER}"

# Install SSH public key
SSH_DIR="/home/${CLUSTERBOX_USER}/.ssh"
mkdir -p "${SSH_DIR}"
chmod 700 "${SSH_DIR}"

if [[ -z "${CLUSTERBOX_SSH_PUBLIC_KEY:-}" ]]; then
  echo "[harden] WARNING: CLUSTERBOX_SSH_PUBLIC_KEY is empty; no key installed." >&2
else
  echo "${CLUSTERBOX_SSH_PUBLIC_KEY}" >> "${SSH_DIR}/authorized_keys"
fi

chmod 600 "${SSH_DIR}/authorized_keys"
chown -R "${CLUSTERBOX_USER}:${CLUSTERBOX_USER}" "${SSH_DIR}"

# Allow passwordless sudo for clusterbox (required for k3s bootstrap later)
echo "${CLUSTERBOX_USER} ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/clusterbox
chmod 0440 /etc/sudoers.d/clusterbox

# ---------------------------------------------------------------------------
# 3. SSH daemon hardening
# ---------------------------------------------------------------------------
log "Hardening sshd configuration..."
SSHD_CONF="/etc/ssh/sshd_config"
SSHD_DROP="/etc/ssh/sshd_config.d/99-clusterbox.conf"

# Write a drop-in so we don't clobber the distro sshd_config entirely.
cat > "${SSHD_DROP}" <<'EOF'
# FoundryFabric clusterbox hardening overrides
PermitRootLogin no
PasswordAuthentication no
ChallengeResponseAuthentication no
KbdInteractiveAuthentication no
UsePAM yes
AuthenticationMethods publickey
PubkeyAuthentication yes

# Reduce attack surface
X11Forwarding no
AllowAgentForwarding no
AllowTcpForwarding no
PermitTunnel no
PrintMotd no

# Timeout idle sessions after 15 minutes (3 × 300 s)
ClientAliveInterval 300
ClientAliveCountMax 3

# Limit auth attempts
MaxAuthTries 3
MaxSessions 10

# Restrict to modern algorithms
Ciphers chacha20-poly1305@openssh.com,aes256-gcm@openssh.com,aes128-gcm@openssh.com
MACs hmac-sha2-512-etm@openssh.com,hmac-sha2-256-etm@openssh.com
KexAlgorithms curve25519-sha256,curve25519-sha256@libssh.org,diffie-hellman-group16-sha512,diffie-hellman-group18-sha512
EOF

# Ensure the Include directive is present in the main config
if ! grep -q "^Include /etc/ssh/sshd_config.d/\*\.conf" "${SSHD_CONF}" 2>/dev/null; then
  sed -i '1s|^|Include /etc/ssh/sshd_config.d/*.conf\n|' "${SSHD_CONF}"
fi

# Validate the final config
sshd -t

# ---------------------------------------------------------------------------
# 4. UFW firewall
# ---------------------------------------------------------------------------
log "Configuring UFW..."

ufw --force reset
ufw default deny incoming
ufw default allow outgoing

# SSH
ufw allow 22/tcp comment "SSH"

# Tailscale (WireGuard UDP)
ufw allow 41641/udp comment "Tailscale WireGuard"

# Enable (non-interactive)
ufw --force enable

# ---------------------------------------------------------------------------
# 5. fail2ban — sshd jail
# ---------------------------------------------------------------------------
log "Configuring fail2ban..."

# fail2ban ships with a working defaults-debian.conf; add our sshd jail on top.
cat > /etc/fail2ban/jail.d/clusterbox.conf <<'EOF'
[DEFAULT]
bantime  = 3600
findtime = 600
maxretry = 5
backend  = systemd

[sshd]
enabled  = true
port     = ssh
logpath  = %(sshd_log)s
maxretry = 3
EOF

systemctl enable fail2ban

# ---------------------------------------------------------------------------
# 6. auditd
# ---------------------------------------------------------------------------
log "Configuring auditd..."

# Sane minimal ruleset covering auth, privilege escalation, and file integrity.
cat > /etc/audit/rules.d/clusterbox.rules <<'EOF'
# Delete all existing rules
-D

# Increase the buffers to survive stress events
-b 8192

# Failure mode: 1 = print a failure message, 2 = panic
-f 1

# --- Login and authentication events ---
-w /var/log/faillog -p wa -k logins
-w /var/log/lastlog -p wa -k logins
-w /var/log/tallylog -p wa -k logins

# --- Privileged commands ---
-a always,exit -F arch=b64 -S setuid -F a0=0 -F exe=/usr/bin/su -k priv_esc
-w /usr/bin/sudo -p x -k priv_esc
-w /etc/sudoers -p wa -k sudoers
-w /etc/sudoers.d/ -p wa -k sudoers

# --- SSH ---
-w /etc/ssh/sshd_config -p wa -k sshd_config
-w /etc/ssh/sshd_config.d/ -p wa -k sshd_config

# --- User/group management ---
-w /etc/passwd -p wa -k identity
-w /etc/shadow -p wa -k identity
-w /etc/group -p wa -k identity
-w /etc/gshadow -p wa -k identity
-w /etc/security/opasswd -p wa -k identity

# --- Kernel module loading ---
-w /sbin/insmod -p x -k modules
-w /sbin/rmmod -p x -k modules
-w /sbin/modprobe -p x -k modules

# Make the configuration immutable (comment out during dev if you need to iterate)
# -e 2
EOF

systemctl enable auditd

# ---------------------------------------------------------------------------
# 7. unattended-upgrades (security channel only)
# ---------------------------------------------------------------------------
log "Configuring unattended-upgrades..."

cat > /etc/apt/apt.conf.d/50unattended-upgrades <<'EOF'
Unattended-Upgrade::Allowed-Origins {
    "${distro_id}:${distro_codename}-security";
    "${distro_id}ESMApps:${distro_codename}-apps-security";
    "${distro_id}ESM:${distro_codename}-infra-security";
};

Unattended-Upgrade::Package-Blacklist {
};

Unattended-Upgrade::AutoFixInterruptedDpkg "true";
Unattended-Upgrade::MinimalSteps "true";
Unattended-Upgrade::Remove-Unused-Kernel-Packages "true";
Unattended-Upgrade::Remove-New-Unused-Dependencies "true";
Unattended-Upgrade::Remove-Unused-Dependencies "true";
Unattended-Upgrade::Automatic-Reboot "false";
Unattended-Upgrade::Automatic-Reboot-WithUsers "false";
EOF

cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Download-Upgradeable-Packages "1";
APT::Periodic::AutocleanInterval "7";
APT::Periodic::Unattended-Upgrade "1";
EOF

systemctl enable unattended-upgrades

# ---------------------------------------------------------------------------
# 8. Miscellaneous hardening tweaks
# ---------------------------------------------------------------------------
log "Applying sysctl hardening..."

cat > /etc/sysctl.d/99-clusterbox.conf <<'EOF'
# IP spoofing protection
net.ipv4.conf.all.rp_filter = 1
net.ipv4.conf.default.rp_filter = 1

# Ignore ICMP redirects
net.ipv4.conf.all.accept_redirects = 0
net.ipv4.conf.default.accept_redirects = 0
net.ipv6.conf.all.accept_redirects = 0
net.ipv6.conf.default.accept_redirects = 0

# Ignore send redirects
net.ipv4.conf.all.send_redirects = 0
net.ipv4.conf.default.send_redirects = 0

# Block SYN attacks
net.ipv4.tcp_syncookies = 1
net.ipv4.tcp_max_syn_backlog = 2048
net.ipv4.tcp_synack_retries = 2
net.ipv4.tcp_syn_retries = 5

# Disable IPv6 if not needed (Tailscale re-enables selectively)
# Leaving IPv6 enabled; Tailscale uses it for mesh.

# Restrict dmesg to root
kernel.dmesg_restrict = 1

# Disable core dumps
fs.suid_dumpable = 0

# Hide kernel pointers
kernel.kptr_restrict = 2
EOF

sysctl --system >/dev/null

log "OS hardening complete."
