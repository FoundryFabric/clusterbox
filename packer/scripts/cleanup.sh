#!/usr/bin/env bash
# cleanup.sh — apt cleanup and free-space zeroing for a smaller snapshot.
#
# Run as the last Packer provisioner step.  Zeroing free space makes the
# resulting Hetzner snapshot significantly smaller because zeroed blocks
# compress much better (or are omitted entirely by copy-on-write storage).
set -euo pipefail

log() { echo "[cleanup] $*"; }

# ---------------------------------------------------------------------------
# 1. apt / dpkg cleanup
# ---------------------------------------------------------------------------
log "Running apt cleanup..."
export DEBIAN_FRONTEND=noninteractive

apt-get autoremove -y -qq
apt-get autoclean -y -qq
apt-get clean -qq

# Remove cached package lists — they will be refreshed at first boot.
rm -rf /var/lib/apt/lists/*

# ---------------------------------------------------------------------------
# 2. Remove temporary and log files that bloat the snapshot
# ---------------------------------------------------------------------------
log "Removing temporary files and truncating logs..."

# Cloud-init artefacts from the build server (not part of the final image)
rm -rf /var/lib/cloud/instances/*
rm -rf /var/lib/cloud/data/*
rm -f  /var/log/cloud-init*.log

# Systemd journal — cleared so the first boot starts with a clean journal.
journalctl --rotate 2>/dev/null || true
journalctl --vacuum-time=1s 2>/dev/null || true
find /var/log/journal -type f -name "*.journal" -delete 2>/dev/null || true

# Other logs
find /var/log -type f -name "*.log" -delete 2>/dev/null || true
find /var/log -type f -name "*.gz"  -delete 2>/dev/null || true
find /var/log -type f -name "*.1"   -delete 2>/dev/null || true
truncate -s 0 /var/log/syslog     2>/dev/null || true
truncate -s 0 /var/log/auth.log   2>/dev/null || true
truncate -s 0 /var/log/kern.log   2>/dev/null || true
truncate -s 0 /var/log/dpkg.log   2>/dev/null || true

# Bash history
unset HISTFILE
history -c 2>/dev/null || true
rm -f /root/.bash_history
rm -f /home/*/.bash_history

# SSH host keys — regenerated on first boot by cloud-init / ssh-keygen.
# NOTE: we keep them here because Hetzner re-injects host keys at boot time.
# If you want per-instance unique keys, uncomment the line below.
# rm -f /etc/ssh/ssh_host_*

# ---------------------------------------------------------------------------
# 3. Zero free space (makes snapshot smaller via thin-provisioned storage)
# ---------------------------------------------------------------------------
log "Zeroing free space on / (this may take a moment)..."
# dd writes zeros until the partition is full, then the temp file is removed.
# The exit code 1 from dd (no space left) is expected and suppressed.
dd if=/dev/zero of=/ZERO bs=1M 2>/dev/null || true
sync
rm -f /ZERO

# Also zero /tmp
dd if=/dev/zero of=/tmp/ZERO bs=1M 2>/dev/null || true
sync
rm -f /tmp/ZERO

# ---------------------------------------------------------------------------
# 4. Remove the packer temporary SSH key from root authorized_keys
#    (Packer adds its own transient key; drop it before snapshotting)
# ---------------------------------------------------------------------------
log "Clearing root authorized_keys..."
truncate -s 0 /root/.ssh/authorized_keys 2>/dev/null || true

log "Cleanup complete. Snapshot is ready."
