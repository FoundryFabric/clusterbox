package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/provision/hetzner"
	"github.com/foundryfabric/clusterbox/internal/registry"
)

// stubProvider is a no-op provision.Provider used to assert that the
// dispatcher routes calls to the factory selected by --provider.
type stubProvider struct {
	name           string
	provisionCalls int
	destroyCalls   int
	reconcileCalls int
}

func (s *stubProvider) Name() string { return s.name }

func (s *stubProvider) Provision(context.Context, provision.ClusterConfig) (provision.ProvisionResult, error) {
	s.provisionCalls++
	return provision.ProvisionResult{}, nil
}

func (s *stubProvider) Destroy(context.Context, registry.Cluster) error {
	s.destroyCalls++
	return nil
}

func (s *stubProvider) Reconcile(context.Context, string) (provision.ReconcileSummary, error) {
	s.reconcileCalls++
	return provision.ReconcileSummary{}, nil
}

// TestResolveProvider_DefaultHetznerWiring verifies the production
// registry exposes the canonical hetzner.Name entry. The factory must
// return a non-nil provider whose Name() round-trips the registry key.
func TestResolveProvider_DefaultHetznerWiring(t *testing.T) {
	prov, err := resolveProvider(hetzner.Name, providerOptions{}, nil)
	if err != nil {
		t.Fatalf("resolveProvider(hetzner): %v", err)
	}
	if prov == nil {
		t.Fatal("resolveProvider(hetzner): nil provider")
	}
	if prov.Name() != hetzner.Name {
		t.Errorf("Name() = %q, want %q", prov.Name(), hetzner.Name)
	}
}

// TestResolveProvider_UnknownReturnsDescriptiveError verifies a typo
// in --provider yields an error mentioning both the bad value and the
// known providers, so the operator can self-correct.
func TestResolveProvider_UnknownReturnsDescriptiveError(t *testing.T) {
	_, err := resolveProvider("baremtl", providerOptions{}, nil)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	msg := err.Error()
	if !strings.Contains(msg, "baremtl") {
		t.Errorf("error must name the bad value: %q", msg)
	}
	if !strings.Contains(msg, "hetzner") {
		t.Errorf("error must list known providers: %q", msg)
	}
	if !strings.Contains(msg, "unknown provider") {
		t.Errorf("error must say 'unknown provider': %q", msg)
	}
}

// TestResolveProvider_TestRegistryOverride verifies a non-nil
// testRegistry is consulted in place of the production map, so unit
// tests can inject stub factories without mutating package state.
func TestResolveProvider_TestRegistryOverride(t *testing.T) {
	called := 0
	stub := &stubProvider{name: "stub-prov"}
	testReg := map[string]providerFactory{
		"stub-prov": func(opts providerOptions) provision.Provider {
			called++
			return stub
		},
	}
	prov, err := resolveProvider("stub-prov", providerOptions{}, testReg)
	if err != nil {
		t.Fatalf("resolveProvider(stub-prov): %v", err)
	}
	if called != 1 {
		t.Errorf("factory call count = %d, want 1", called)
	}
	if prov != stub {
		t.Errorf("expected the stub provider instance, got %T", prov)
	}
}

// TestResolveProvider_UnknownInTestRegistry verifies the unknown-name
// branch fires against the test registry too, so callers can assert
// validation behaviour without touching the production map.
func TestResolveProvider_UnknownInTestRegistry(t *testing.T) {
	testReg := map[string]providerFactory{
		"alpha": func(providerOptions) provision.Provider { return &stubProvider{name: "alpha"} },
		"beta":  func(providerOptions) provision.Provider { return &stubProvider{name: "beta"} },
	}
	_, err := resolveProvider("gamma", providerOptions{}, testReg)
	if err == nil {
		t.Fatal("expected error for unknown provider in test registry")
	}
	msg := err.Error()
	if !strings.Contains(msg, "alpha") || !strings.Contains(msg, "beta") {
		t.Errorf("error must list every known provider: %q", msg)
	}
	if strings.Contains(msg, hetzner.Name) {
		t.Errorf("error must not leak production registry: %q", msg)
	}
}

// TestRunDestroyWith_DispatchesByProvider verifies cmd/destroy
// dispatches teardown through the provider selected on the cluster
// row. A stub provider lets us assert the dispatch happened without
// running real Pulumi / hcloud calls.
func TestRunDestroyWith_DispatchesByProvider(t *testing.T) {
	stub := &stubProvider{name: "stub-prov"}
	reg := newDestroyFakeRegistry(
		registry.Cluster{Name: "c1", Provider: "stub-prov"},
		nil,
	)
	deps := DestroyDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		ProviderRegistry: map[string]providerFactory{
			"stub-prov": func(providerOptions) provision.Provider { return stub },
		},
	}
	if err := RunDestroyWith(context.Background(), "c1", "tok", true /*yes*/, false /*dryRun*/, deps); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if stub.destroyCalls != 1 {
		t.Errorf("provider.Destroy: got %d calls, want 1", stub.destroyCalls)
	}
	if !reg.clusterDestroyed {
		t.Errorf("cluster row not marked destroyed")
	}
}

// TestRunDestroyWith_UnknownProviderReturnsError verifies a cluster
// row carrying an unrecognised provider value surfaces a descriptive
// error rather than silently falling back.
func TestRunDestroyWith_UnknownProviderReturnsError(t *testing.T) {
	reg := newDestroyFakeRegistry(
		registry.Cluster{Name: "c1", Provider: "made-up"},
		nil,
	)
	deps := DestroyDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		// Empty test registry: only "made-up" lookup will be attempted, and
		// that name is not registered.
		ProviderRegistry: map[string]providerFactory{},
	}
	err := RunDestroyWith(context.Background(), "c1", "tok", true, false, deps)
	if err == nil {
		t.Fatal("expected error for unknown provider on cluster row")
	}
	if !strings.Contains(err.Error(), "made-up") {
		t.Errorf("error must name the bad provider: %v", err)
	}
}

// TestRunDestroyWith_LegacyRowDefaultsToHetzner verifies a cluster row
// recorded before the multi-provider era (Provider == "") routes
// through the hetzner factory. A test override on the registry lets
// us assert the routing without standing up real Pulumi calls.
func TestRunDestroyWith_LegacyRowDefaultsToHetzner(t *testing.T) {
	stub := &stubProvider{name: hetzner.Name}
	reg := newDestroyFakeRegistry(
		registry.Cluster{Name: "c1" /* Provider intentionally empty */},
		nil,
	)
	deps := DestroyDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		ProviderRegistry: map[string]providerFactory{
			hetzner.Name: func(providerOptions) provision.Provider { return stub },
		},
	}
	if err := RunDestroyWith(context.Background(), "c1", "tok", true, false, deps); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if stub.destroyCalls != 1 {
		t.Errorf("hetzner provider.Destroy: got %d calls, want 1", stub.destroyCalls)
	}
}

// TestRunDestroyWith_ProviderErrorPropagates verifies a provider
// failure short-circuits before the cluster row is marked destroyed,
// so a re-run after fixing the underlying problem converges.
func TestRunDestroyWith_ProviderErrorPropagates(t *testing.T) {
	failing := &failingProvider{err: errors.New("boom")}
	reg := newDestroyFakeRegistry(
		registry.Cluster{Name: "c1", Provider: "failing"},
		nil,
	)
	deps := DestroyDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		ProviderRegistry: map[string]providerFactory{
			"failing": func(providerOptions) provision.Provider { return failing },
		},
	}
	err := RunDestroyWith(context.Background(), "c1", "tok", true, false, deps)
	if err == nil {
		t.Fatal("expected error from provider")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected wrapped provider error, got %v", err)
	}
	if reg.clusterDestroyed {
		t.Errorf("cluster must NOT be marked destroyed when provider fails")
	}
}

type failingProvider struct {
	err error
}

func (f *failingProvider) Name() string { return "failing" }
func (f *failingProvider) Provision(context.Context, provision.ClusterConfig) (provision.ProvisionResult, error) {
	return provision.ProvisionResult{}, f.err
}
func (f *failingProvider) Destroy(context.Context, registry.Cluster) error { return f.err }
func (f *failingProvider) Reconcile(context.Context, string) (provision.ReconcileSummary, error) {
	return provision.ReconcileSummary{}, f.err
}
