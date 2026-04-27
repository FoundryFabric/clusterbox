package hetzner_test

import (
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/provision/hetzner"
)

// ---- cloud-init unit tests ----

func TestRenderCloudInit_ContainsTailscaleUp(t *testing.T) {
	out, err := hetzner.RenderCloudInit("test-cluster", "tskey-auth-abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"tailscale up",
		"--authkey=tskey-auth-abc123",
		"--hostname=test-cluster",
		"/data",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cloud-init output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestRenderCloudInit_EmptyClusterName(t *testing.T) {
	_, err := hetzner.RenderCloudInit("", "tskey-auth-abc123")
	if err == nil {
		t.Fatal("expected error for empty clusterName")
	}
}

func TestRenderCloudInit_EmptyAuthKey(t *testing.T) {
	_, err := hetzner.RenderCloudInit("test-cluster", "")
	if err == nil {
		t.Fatal("expected error for empty authKey")
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
		t.Errorf("VolumeID: want 0, got %d", r.VolumeID)
	}
	if r.FirewallID != 0 {
		t.Errorf("FirewallID: want 0, got %d", r.FirewallID)
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
