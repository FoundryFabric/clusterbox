package cmd

import (
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/provision/k3d"
)

// TestResolveProvider_K3dWiring verifies the k3d entry is registered
// in the production provider registry and that the returned provider's
// Name() round-trips correctly.
func TestResolveProvider_K3dWiring(t *testing.T) {
	prov, err := resolveProvider(k3d.Name, providerOptions{}, nil)
	if err != nil {
		t.Fatalf("resolveProvider(k3d): %v", err)
	}
	if prov == nil {
		t.Fatal("resolveProvider(k3d): nil provider")
	}
	if prov.Name() != k3d.Name {
		t.Errorf("Name() = %q, want %q", prov.Name(), k3d.Name)
	}
}

// TestResolveProvider_K3dCompilesAsProvisionProvider asserts that the
// k3d factory returns something satisfying the cloud-agnostic interface
// so cmd/destroy dispatch can route through it.
func TestResolveProvider_K3dCompilesAsProvisionProvider(t *testing.T) {
	prov, err := resolveProvider(k3d.Name, providerOptions{}, nil)
	if err != nil {
		t.Fatalf("resolveProvider(k3d): %v", err)
	}
	_ = prov
}

// TestResolveProvider_UnknownErrorListsK3d verifies the unknown-provider
// error message includes k3d alongside the other registered providers.
func TestResolveProvider_UnknownErrorListsK3d(t *testing.T) {
	_, err := resolveProvider("nope", providerOptions{}, nil)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if !strings.Contains(err.Error(), k3d.Name) {
		t.Errorf("error should list %q in known providers: %q", k3d.Name, err.Error())
	}
}
