package hetzner

import (
	"bytes"
	"fmt"
	"text/template"
)

// cloudInitTemplate writes the clusterboxnode spec to /etc/clusterboxnode.yaml,
// creates the ubuntu user with the provided SSH public key, and installs
// Tailscale on first boot. After Tailscale comes up the provider SSHes in via
// the Tailscale hostname to upload and run the clusterboxnode binary.
var cloudInitTemplate = template.Must(template.New("cloud-init").Parse(`#cloud-config
users:
  - name: ubuntu
    ssh_authorized_keys:
      - {{ .SSHPubKey }}
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
write_files:
  - path: /etc/clusterboxnode.yaml
    encoding: b64
    content: {{ .ConfigB64 }}
    permissions: '0644'
runcmd:
  - bash -c 'set -euo pipefail; curl -fsSL https://tailscale.com/install.sh | sh && tailscale up {{ .TailscaleUpFlags }}'
`))

// cloudInitInput holds the inputs for cloud-init template rendering.
type cloudInitInput struct {
	SSHPubKey        string
	ConfigB64        string
	TailscaleUpFlags string
}

// RenderCloudInit returns the cloud-init user-data for a Hetzner node.
//
// sshPubKey is the authorized public key written to the ubuntu user.
// configB64 is the base64-encoded clusterboxnode spec YAML written to
// /etc/clusterboxnode.yaml. tsAuthKey is the Tailscale ephemeral auth key.
// hostname is the optional Tailscale machine hostname; when empty the
// --hostname flag is omitted.
func RenderCloudInit(sshPubKey, configB64, tsAuthKey, hostname string) (string, error) {
	if sshPubKey == "" {
		return "", fmt.Errorf("hetzner: sshPubKey must not be empty")
	}
	if configB64 == "" {
		return "", fmt.Errorf("hetzner: configB64 must not be empty")
	}
	if tsAuthKey == "" {
		return "", fmt.Errorf("hetzner: tsAuthKey must not be empty")
	}
	flags := "--authkey=" + tsAuthKey + " --accept-routes --accept-dns"
	if hostname != "" {
		flags += " --hostname=" + hostname
	}
	var buf bytes.Buffer
	if err := cloudInitTemplate.Execute(&buf, cloudInitInput{
		SSHPubKey:        sshPubKey,
		ConfigB64:        configB64,
		TailscaleUpFlags: flags,
	}); err != nil {
		return "", fmt.Errorf("hetzner: render cloud-init: %w", err)
	}
	return buf.String(), nil
}
