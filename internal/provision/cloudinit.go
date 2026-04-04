package provision

import (
	"bytes"
	"fmt"
	"text/template"
)

// cloudInitTemplate is the cloud-init user-data that runs on first boot.
// It installs tailscale (if not already present in the base image) and
// activates the node with the provided ephemeral auth key.
var cloudInitTemplate = template.Must(template.New("cloud-init").Parse(`#cloud-config
runcmd:
  - |
    set -euo pipefail

    # Ensure tailscale is installed (base image may already have it).
    if ! command -v tailscale >/dev/null 2>&1; then
      curl -fsSL https://tailscale.com/install.sh | sh
    fi

    # Bring the node up on the tailnet with an ephemeral key.
    tailscale up \
      --authkey={{ .AuthKey }} \
      --hostname={{ .Hostname }} \
      --accept-routes \
      --accept-dns

    # Mount the data volume at /data (formatted as ext4 by Pulumi).
    mkdir -p /data
    if ! grep -q '/data' /etc/fstab; then
      DEVICE=$(blkid -L data || echo /dev/disk/by-id/scsi-0HC_Volume_*)
      echo "${DEVICE} /data ext4 defaults,nofail 0 2" >> /etc/fstab
      mount -a
    fi
`))

// cloudInitData holds the template inputs.
type cloudInitData struct {
	AuthKey  string
	Hostname string
}

// RenderCloudInit returns the cloud-init user-data string for the given
// cluster name and ephemeral Tailscale auth key.
func RenderCloudInit(clusterName, authKey string) (string, error) {
	if clusterName == "" {
		return "", fmt.Errorf("provision: clusterName must not be empty")
	}
	if authKey == "" {
		return "", fmt.Errorf("provision: authKey must not be empty")
	}

	var buf bytes.Buffer
	if err := cloudInitTemplate.Execute(&buf, cloudInitData{
		AuthKey:  authKey,
		Hostname: clusterName,
	}); err != nil {
		return "", fmt.Errorf("provision: render cloud-init: %w", err)
	}
	return buf.String(), nil
}
