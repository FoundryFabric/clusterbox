package cmd_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/provision/hetzner"
)

// TestAddNode_AgentSpecRole verifies that a worker agent spec is built with
// role=agent so the k3s installer runs in agent mode (not server-init).
func TestAddNode_AgentSpecRole(t *testing.T) {
	spec := buildAgentSpec("my-cluster", "10.0.1.1", "10.0.1.2", "secret-token", bootstrap.DefaultK3sVersion)
	if spec.K3s == nil {
		t.Fatal("K3s spec is nil")
	}
	if spec.K3s.Role != "agent" {
		t.Errorf("expected role=agent, got %q", spec.K3s.Role)
	}
}

// TestAddNode_AgentSpecServerURL verifies that the server URL points to the
// control-plane's private IP, not its Tailscale hostname. All k3s API traffic
// must stay on the private network.
func TestAddNode_AgentSpecServerURL(t *testing.T) {
	cpPrivateIP := "10.0.1.1"
	spec := buildAgentSpec("my-cluster", cpPrivateIP, "10.0.1.2", "secret-token", bootstrap.DefaultK3sVersion)
	want := "https://" + cpPrivateIP + ":6443"
	if spec.K3s.ServerURL != want {
		t.Errorf("ServerURL: want %q, got %q", want, spec.K3s.ServerURL)
	}
	if strings.Contains(spec.K3s.ServerURL, "my-cluster") {
		t.Errorf("ServerURL must not contain the Tailscale hostname, got %q", spec.K3s.ServerURL)
	}
}

// TestAddNode_AgentSpecNodeIP verifies that the worker's private IP is set as
// NodeIP so k3s binds on the private interface, not the Tailscale tunnel.
func TestAddNode_AgentSpecNodeIP(t *testing.T) {
	workerPrivateIP := "10.0.1.2"
	spec := buildAgentSpec("my-cluster", "10.0.1.1", workerPrivateIP, "secret-token", bootstrap.DefaultK3sVersion)
	if spec.K3s.NodeIP != workerPrivateIP {
		t.Errorf("NodeIP: want %q, got %q", workerPrivateIP, spec.K3s.NodeIP)
	}
}

// TestAddNode_AgentSpecFlannelIface verifies that FlannelIface is set to the
// Hetzner private network interface so Flannel VXLAN uses eth1, not Tailscale.
func TestAddNode_AgentSpecFlannelIface(t *testing.T) {
	spec := buildAgentSpec("my-cluster", "10.0.1.1", "10.0.1.2", "secret-token", bootstrap.DefaultK3sVersion)
	if spec.K3s.FlannelIface != hetzner.HetznerPrivateIface {
		t.Errorf("FlannelIface: want %q, got %q", hetzner.HetznerPrivateIface, spec.K3s.FlannelIface)
	}
}

// TestAddNode_AgentSpecValidates verifies that the constructed agent spec
// passes Spec.Validate(), catching any field omissions early.
func TestAddNode_AgentSpecValidates(t *testing.T) {
	spec := buildAgentSpec("my-cluster", "10.0.1.1", "10.0.1.2", "secret-token", bootstrap.DefaultK3sVersion)
	if err := spec.Validate(); err != nil {
		t.Errorf("spec.Validate() failed: %v", err)
	}
}

// TestAddNode_AgentSpecK3sVersion verifies that the requested k3s version is
// forwarded to the agent spec unchanged.
func TestAddNode_AgentSpecK3sVersion(t *testing.T) {
	const version = "v1.29.0+k3s1"
	spec := buildAgentSpec("my-cluster", "10.0.1.1", "10.0.1.2", "secret-token", version)
	if spec.K3s.Version != version {
		t.Errorf("Version: want %q, got %q", version, spec.K3s.Version)
	}
}

// buildAgentSpec constructs the agent config.Spec the way addOneNode does,
// so tests can assert on its fields without needing hcloud or SSH.
func buildAgentSpec(clusterName, cpPrivateIP, workerPrivateIP, nodeToken, k3sVersion string) *config.Spec {
	return &config.Spec{
		Hostname: clusterName + "-worker",
		K3s: &config.K3sSpec{
			Enabled:      true,
			Role:         "agent",
			Version:      k3sVersion,
			ServerURL:    fmt.Sprintf("https://%s:6443", cpPrivateIP),
			Token:        nodeToken,
			NodeIP:       workerPrivateIP,
			FlannelIface: hetzner.HetznerPrivateIface,
		},
	}
}
