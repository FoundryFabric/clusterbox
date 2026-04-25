# Packer Snapshot

clusterbox provisions Hetzner VMs from a pre-baked snapshot — the OS image already has hardening applied and the Tailscale binary installed before a node ever boots. This document covers what the snapshot is, how it's built, and how clusterbox consumes it.

---

## How it works

The snapshot is a hardened Ubuntu 24.04 image, built by Packer in this repo and pushed to the Hetzner Cloud project that clusterbox provisions into. Every Hetzner VM created by `clusterbox up` boots from this snapshot, so first-boot setup is reduced to per-instance work (k3s install, Tailscale activation, secret resolution).

### What the snapshot includes

- A non-root `clusterbox` user with key-only SSH access
- sshd hardening (no root login, no passwords, modern ciphers, idle timeout, MaxAuthTries)
- UFW with default-deny inbound and only SSH (22/tcp) + Tailscale WireGuard (41641/udp) allowed
- fail2ban with an sshd jail
- auditd with a sane minimal ruleset (logins, privilege escalation, sshd config, identity files, kernel modules)
- unattended-upgrades configured for the **security channel only**
- Sysctl hardening (rp_filter, ICMP redirect handling, SYN-flood protection, dmesg restriction)
- The Tailscale binary and an enabled `tailscaled` service

### What the snapshot excludes

- **k3s** — installed by clusterbox at provision time so node role (control-plane vs. worker) and the K3s version stay decoupled from the snapshot version
- **Tailscale activation** — `tailscale up` is called by the provisioner with a per-cluster auth key; the binary is baked in but the node is not joined to a tailnet

---

## Where the template lives

```
packer/
  hetzner-base.pkr.hcl   Build definition + provisioner wiring
  variables.pkr.hcl      Variable declarations (token, ssh key, version)
  scripts/
    harden.sh            OS hardening
    install-tailscale.sh Tailscale binary install + enable
    cleanup.sh           apt clean, log truncation, free-space zeroing
  README.md              Short pointer back to this file
```

---

## Building locally

```bash
cd packer

# 1. Install the hcloud plugin
packer init hetzner-base.pkr.hcl

# 2. Build (use env vars or a .pkrvars.hcl file for secrets)
packer build \
  -var "hetzner_api_token=${HCLOUD_TOKEN}" \
  -var "ssh_public_key=$(cat ~/.ssh/clusterbox.pub)" \
  -var "version=0.1.0" \
  hetzner-base.pkr.hcl
```

For local iteration, drop a `local.pkrvars.hcl` next to the template (gitignored):

```hcl
hetzner_api_token = "hcloud-..."
ssh_public_key    = "ssh-ed25519 AAAA... you@host"
version           = "0.1.0"
```

```bash
packer build -var-file=local.pkrvars.hcl hetzner-base.pkr.hcl
```

### Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `hetzner_api_token` | yes | — | Hetzner Cloud API token (sensitive) |
| `ssh_public_key` | yes | — | Public SSH key authorised for the `clusterbox` user |
| `version` | no | `0.1.0` | Snapshot version string, appended to the snapshot name |

---

## Where the snapshot ends up

Once the build succeeds, the result is a Hetzner Cloud snapshot named:

```
clusterbox-base-v<version>     e.g. clusterbox-base-v0.1.0
```

with these labels (used by clusterbox to discover the snapshot):

```
managed-by  = "packer"
version     = <version>
base-image  = "ubuntu-24.04"
purpose     = "clusterbox-base"
```

---

## How clusterbox consumes it

clusterbox references the snapshot by name from its Pulumi config. The active snapshot version is a Pulumi config value (e.g. `clusterbox:snapshotVersion = "0.1.0"`); the Pulumi program looks up the corresponding Hetzner snapshot and uses its ID when creating servers. Bumping the snapshot version is therefore: build a new snapshot, update the Pulumi config value, run `clusterbox up`.

This is why the packer template lives in this repo: bumping the version in two places used to require coordination across two repos. Now both moves land in the same PR.

---

## CI

`.github/workflows/snapshot-build.yml` runs every Monday at 03:00 UTC, on `workflow_dispatch`, and on changes under `packer/**`. Today it only runs `packer init` and `packer validate` — the build step is **stubbed**.

To un-stub the actual build, two repository secrets are required:

- `HCLOUD_TOKEN` — Hetzner Cloud API token with read + write access
- `CLUSTERBOX_SSH_PUBLIC_KEY` — Public SSH key authorised for the `clusterbox` user on built nodes

Once those are wired, uncomment the `Packer Build` step in the workflow.

The Go CI workflow (`.github/workflows/ci.yml`) ignores changes under `packer/**` and `docs/packer.md` so the two CI lanes don't trigger each other.

---

## Future direction

Per-architecture builds (arm64 alongside amd64) and per-purpose images (a fatter image for FDB data nodes that pre-installs the FDB client, a `dev` image with extra debugging tooling) are likely next. Both are additive — a new `source` block and a small matrix in the workflow — but **not in scope today**. The single base snapshot covers every k3s node clusterbox currently provisions.
