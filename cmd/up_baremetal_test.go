package cmd

import (
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/provision/baremetal"
)

// TestResolveProvider_BaremetalWiring verifies the baremetal entry is
// registered in the production registry and that resolveProvider returns
// a baremetal.Provider whose Name() round-trips.
func TestResolveProvider_BaremetalWiring(t *testing.T) {
	prov, err := resolveProvider(baremetal.Name, providerOptions{
		BaremetalHost:       "192.0.2.1",
		BaremetalUser:       "ops",
		BaremetalSSHKeyPath: "/tmp/key",
	}, nil)
	if err != nil {
		t.Fatalf("resolveProvider(baremetal): %v", err)
	}
	if prov == nil {
		t.Fatal("resolveProvider(baremetal): nil provider")
	}
	if prov.Name() != baremetal.Name {
		t.Errorf("Name() = %q, want %q", prov.Name(), baremetal.Name)
	}
}

// TestResolveProvider_UnknownErrorListsBoth verifies the unknown-provider
// error message lists both the hetzner AND baremetal entries.
func TestResolveProvider_UnknownErrorListsBoth(t *testing.T) {
	_, err := resolveProvider("nope", providerOptions{}, nil)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	msg := err.Error()
	for _, want := range []string{baremetal.Name, "hetzner"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must list %q in known providers: %q", want, msg)
		}
	}
}

// TestResolveProvider_BaremetalCompilesAsProvisionProvider asserts that
// the baremetal factory returns something satisfying the cloud-agnostic
// interface so cmd/destroy + cmd/sync dispatch can route through it.
func TestResolveProvider_BaremetalCompilesAsProvisionProvider(t *testing.T) {
	prov, err := resolveProvider(baremetal.Name, providerOptions{
		BaremetalHost:       "h",
		BaremetalUser:       "u",
		BaremetalSSHKeyPath: "/dev/null",
	}, nil)
	if err != nil {
		t.Fatalf("resolveProvider(baremetal): %v", err)
	}
	_ = prov
}
