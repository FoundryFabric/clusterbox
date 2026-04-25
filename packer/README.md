# packer/

Packer templates for the Hetzner snapshot that `clusterbox up` provisions
nodes from. Lives inside the `packer/` directory of clusterbox so the
snapshot definition and the consumer stay in lockstep.

`hetzner-base.pkr.hcl` builds a hardened Ubuntu 24.04 Hetzner snapshot
(`clusterbox-base-v<version>`) that serves as the base image for k3s
cluster nodes. k3s itself is **not** baked into the image — it is
installed at provision time by clusterbox/Pulumi.

## What the build does

| Step | Script | Description |
|---|---|---|
| 1 | `scripts/harden.sh` | OS hardening (user, sshd, UFW, fail2ban, auditd, unattended-upgrades, sysctl) |
| 2 | `scripts/install-tailscale.sh` | Install Tailscale stable, enable `tailscaled` service — does **not** call `tailscale up` |
| 3 | `scripts/cleanup.sh` | apt clean, log truncation, free-space zeroing for a smaller snapshot |

For build instructions, variables, the CI workflow, and how clusterbox
consumes the snapshot, see [`../docs/packer.md`](../docs/packer.md).
