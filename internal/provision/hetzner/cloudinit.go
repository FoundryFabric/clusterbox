package hetzner

import (
	"bytes"
	"fmt"
	"text/template"
)

// cloudInitTemplate writes the clusterboxnode spec to /etc/clusterboxnode.yaml
// then downloads and runs clusterboxnode on first boot. The binary URL is
// constructed as <AgentDownloadBaseURL>/clusterboxnode-linux-${ARCH} where ARCH
// is determined at runtime via uname -m.
var cloudInitTemplate = template.Must(template.New("cloud-init").Parse(`#cloud-config
write_files:
  - path: /etc/clusterboxnode.yaml
    encoding: b64
    content: {{ .ConfigB64 }}
    permissions: '0644'
runcmd:
  - |
    set -euo pipefail
    ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
    curl -fsSL "{{ .AgentDownloadBaseURL }}/clusterboxnode-linux-${ARCH}" \
      -o /usr/local/bin/clusterboxnode
    chmod +x /usr/local/bin/clusterboxnode
    /usr/local/bin/clusterboxnode install --config /etc/clusterboxnode.yaml
`))

// cloudInitInput holds the inputs for cloud-init template rendering.
type cloudInitInput struct {
	// ConfigB64 is the base64-encoded clusterboxnode spec YAML.
	ConfigB64 string
	// AgentDownloadBaseURL is the base URL for the clusterboxnode binary.
	// The runcmd script appends /clusterboxnode-linux-${ARCH} to this URL.
	AgentDownloadBaseURL string
}

// RenderCloudInit returns the cloud-init user-data for a Hetzner node.
//
// configB64 is the base64-encoded clusterboxnode spec YAML (includes Tailscale
// auth key and k3s configuration). agentDownloadBaseURL is the root URL from
// which the per-arch clusterboxnode binary is fetched by the runcmd script.
func RenderCloudInit(configB64, agentDownloadBaseURL string) (string, error) {
	if configB64 == "" {
		return "", fmt.Errorf("hetzner: configB64 must not be empty")
	}
	if agentDownloadBaseURL == "" {
		return "", fmt.Errorf("hetzner: agentDownloadBaseURL must not be empty")
	}
	var buf bytes.Buffer
	if err := cloudInitTemplate.Execute(&buf, cloudInitInput{
		ConfigB64:            configB64,
		AgentDownloadBaseURL: agentDownloadBaseURL,
	}); err != nil {
		return "", fmt.Errorf("hetzner: render cloud-init: %w", err)
	}
	return buf.String(), nil
}
