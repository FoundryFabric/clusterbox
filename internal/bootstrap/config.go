// Package bootstrap provides a step that installs k3s on a remote host via
// k3sup over Tailscale SSH, then waits until the node reports Ready.
package bootstrap

// DefaultK3sVersion is the pinned k3s release used when K3sConfig.K3sVersion
// is left empty.
const DefaultK3sVersion = "v1.32.3+k3s1"

// K3sConfig holds all inputs required to bootstrap a single k3s node.
type K3sConfig struct {
	// TailscaleIP is the Tailscale IP (or hostname) of the target node.
	// k3sup connects to this address over Tailscale SSH.
	TailscaleIP string

	// K3sVersion is the exact k3s release to install, e.g. "v1.32.3+k3s1".
	// Defaults to DefaultK3sVersion when empty.
	K3sVersion string

	// User is the SSH user on the target node. Defaults to "clusterbox".
	User string

	// KubeconfigPath is the local path where k3sup writes the merged kubeconfig,
	// e.g. "/home/ops/.kube/clusterbox.yaml".
	KubeconfigPath string

	// SSHKeyPath is the path to the SSH private key used by k3sup.
	SSHKeyPath string
}

// effective returns a copy of cfg with defaults applied.
func (cfg K3sConfig) effective() K3sConfig {
	out := cfg
	if out.K3sVersion == "" {
		out.K3sVersion = DefaultK3sVersion
	}
	if out.User == "" {
		out.User = "clusterbox"
	}
	return out
}
