packer {
  required_version = ">= 1.10.0"

  required_plugins {
    hcloud = {
      version = ">= 1.4.0"
      source  = "github.com/hashicorp/hcloud"
    }
  }
}

locals {
  snapshot_name = "clusterbox-base-v${var.version}"
}

source "hcloud" "clusterbox_base" {
  token = var.hetzner_api_token

  # Build server spec — cpx11 is cheap (2 vCPU, 2 GB RAM) and sufficient for
  # image preparation.
  server_type = "cpx11"
  location    = "ash"
  image       = "ubuntu-24.04"

  # SSH access for provisioning
  ssh_username = "root"
  # Packer generates a temporary keypair; the permanent key is injected by the
  # harden.sh provisioner for the non-root clusterbox user.
  ssh_agent_auth = false

  snapshot_name   = local.snapshot_name
  snapshot_labels = {
    managed-by = "packer"
    version    = var.version
    base-image = "ubuntu-24.04"
    purpose    = "clusterbox-base"
  }
}

build {
  name    = "clusterbox-base"
  sources = ["source.hcloud.clusterbox_base"]

  # 1. OS hardening: non-root user, sshd lockdown, UFW, fail2ban, auditd,
  #    unattended-upgrades (security channel only).
  provisioner "shell" {
    script = "scripts/harden.sh"
    environment_vars = [
      "CLUSTERBOX_SSH_PUBLIC_KEY=${var.ssh_public_key}",
    ]
    execute_command = "chmod +x {{ .Path }}; {{ .Vars }} bash {{ .Path }}"
  }

  # 2. Install Tailscale binary + enable the systemd service.
  #    tailscale up is NOT called here; activation is performed at provision
  #    time by Pulumi.
  provisioner "shell" {
    script          = "scripts/install-tailscale.sh"
    execute_command = "chmod +x {{ .Path }}; bash {{ .Path }}"
  }

  # 3. apt clean + zero free space for a smaller snapshot.
  provisioner "shell" {
    script          = "scripts/cleanup.sh"
    execute_command = "chmod +x {{ .Path }}; bash {{ .Path }}"
  }
}
