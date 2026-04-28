package hetzner_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/provision/hetzner"
)

// ---- cloud-init unit tests ----

func TestRenderCloudInit_ContainsClusterboxnode(t *testing.T) {
	configYAML := "hostname: test-cluster\ntailscale:\n  enabled: true\n  auth_key: tskey-auth-abc123\n"
	configB64 := base64.StdEncoding.EncodeToString([]byte(configYAML))
	const baseURL = "https://releases.example.com/v1.0.0"

	out, err := hetzner.RenderCloudInit(configB64, baseURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"clusterboxnode install",
		"/etc/clusterboxnode.yaml",
		configB64,
		baseURL,
		"clusterboxnode-linux-${ARCH}",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cloud-init output missing %q\ngot:\n%s", want, out)
		}
	}
	// Must NOT call tailscale up directly — that is now handled by clusterboxnode.
	if strings.Contains(out, "tailscale up") {
		t.Error("cloud-init must not call tailscale up directly; clusterboxnode handles it")
	}
}

func TestRenderCloudInit_EmptyConfigB64(t *testing.T) {
	_, err := hetzner.RenderCloudInit("", "https://releases.example.com/v1.0.0")
	if err == nil {
		t.Fatal("expected error for empty configB64")
	}
}

func TestRenderCloudInit_EmptyBaseURL(t *testing.T) {
	_, err := hetzner.RenderCloudInit("dGVzdA==", "")
	if err == nil {
		t.Fatal("expected error for empty agentDownloadBaseURL")
	}
}

// ---- CreateResult structure tests ----

// TestCreateResult_ZeroValue verifies the zero value of CreateResult is safe
// to inspect (no panics, all fields are zero).
func TestCreateResult_ZeroValue(t *testing.T) {
	var r hetzner.CreateResult
	if r.ServerID != 0 {
		t.Errorf("ServerID: want 0, got %d", r.ServerID)
	}
	if r.VolumeID != 0 {
		t.Errorf("VolumeID: want 200, got %d", r.VolumeID)
	}
	if r.FirewallID != 0 {
		t.Errorf("FirewallID: want 300, got %d", r.FirewallID)
	}
	if r.ServerIPv4 != "" {
		t.Errorf("ServerIPv4: want empty, got %q", r.ServerIPv4)
	}
}

// TestCreateResult_Fields verifies that a populated CreateResult carries
// all expected IDs and the server IPv4.
func TestCreateResult_Fields(t *testing.T) {
	r := hetzner.CreateResult{
		ServerID:   100,
		ServerIPv4: "1.2.3.4",
		VolumeID:   200,
		FirewallID: 300,
	}
	if r.ServerID != 100 {
		t.Errorf("ServerID: want 100, got %d", r.ServerID)
	}
	if r.ServerIPv4 != "1.2.3.4" {
		t.Errorf("ServerIPv4: want 1.2.3.4, got %q", r.ServerIPv4)
	}
	if r.VolumeID != 200 {
		t.Errorf("VolumeID: want 200, got %d", r.VolumeID)
	}
	if r.FirewallID != 300 {
		t.Errorf("FirewallID: want 300, got %d", r.FirewallID)
	}
}
