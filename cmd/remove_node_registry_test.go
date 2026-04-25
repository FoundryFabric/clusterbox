package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/registry"
)

// TestRemoveNodeFromRegistry_HappyPath verifies that the (clusterName,
// hostname) pair is forwarded to RemoveNode.
func TestRemoveNodeFromRegistry_HappyPath(t *testing.T) {
	fake := &fakeRegistry{}
	deps := RemoveNodeDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) {
			return fake, nil
		},
	}

	removeNodeFromRegistry(context.Background(), deps, "hetzner-ash", "hetzner-ash-node")

	if !fake.closed {
		t.Errorf("registry was not closed")
	}
	if got := len(fake.clusters); got != 0 {
		t.Errorf("remove-node must NOT touch the cluster row; got %d cluster writes", got)
	}
	if got := len(fake.removed); got != 1 {
		t.Fatalf("expected 1 RemoveNode call, got %d", got)
	}
	if fake.removed[0] != [2]string{"hetzner-ash", "hetzner-ash-node"} {
		t.Errorf("RemoveNode args mismatch: %+v", fake.removed[0])
	}
}

// TestRemoveNodeFromRegistry_OpenFailure_WarnsAndReturns verifies that
// failing to open the registry prints a warning to stderr and returns
// without panicking.
func TestRemoveNodeFromRegistry_OpenFailure_WarnsAndReturns(t *testing.T) {
	deps := RemoveNodeDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) {
			return nil, errors.New("disk on fire")
		},
	}

	stderr := captureStderr(t, func() {
		removeNodeFromRegistry(context.Background(), deps, "hetzner-ash", "hetzner-ash-node")
	})

	if !strings.Contains(stderr, "warning: registry write failed") {
		t.Errorf("expected warning on stderr, got %q", stderr)
	}
	if !strings.Contains(stderr, "disk on fire") {
		t.Errorf("expected underlying error in warning, got %q", stderr)
	}
}

// TestRemoveNodeFromRegistry_RemoveFailure_WarnsAndReturns verifies that a
// RemoveNode error is logged and does not panic.
func TestRemoveNodeFromRegistry_RemoveFailure_WarnsAndReturns(t *testing.T) {
	fake := &fakeRegistry{removeErr: errors.New("locked")}
	deps := RemoveNodeDeps{OpenRegistry: func(context.Context) (registry.Registry, error) { return fake, nil }}

	stderr := captureStderr(t, func() {
		removeNodeFromRegistry(context.Background(), deps, "hetzner-ash", "hetzner-ash-node")
	})

	if !fake.closed {
		t.Errorf("registry must still be closed when RemoveNode fails")
	}
	if !strings.Contains(stderr, "locked") {
		t.Errorf("expected remove error in warning, got %q", stderr)
	}
}

// TestRemoveNodeFromRegistry_CloseError_Warns verifies that a Close error is
// logged but does not undo the RemoveNode call.
func TestRemoveNodeFromRegistry_CloseError_Warns(t *testing.T) {
	fake := &fakeRegistry{closeErr: errors.New("close exploded")}
	deps := RemoveNodeDeps{OpenRegistry: func(context.Context) (registry.Registry, error) { return fake, nil }}

	stderr := captureStderr(t, func() {
		removeNodeFromRegistry(context.Background(), deps, "hetzner-ash", "hetzner-ash-node")
	})

	if !strings.Contains(stderr, "close exploded") {
		t.Errorf("expected close error in warning, got %q", stderr)
	}
	if got := len(fake.removed); got != 1 {
		t.Errorf("RemoveNode should still have been called, got %d", got)
	}
}

// TestRemoveNodeFromRegistry_DefaultsToRealRegistry verifies that a nil
// OpenRegistry falls back to registry.NewRegistry. We seed a cluster and
// worker node, then assert removeNodeFromRegistry actually deletes the row.
func TestRemoveNodeFromRegistry_DefaultsToRealRegistry(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("REGISTRY_BACKEND", "")

	// Seed: cluster + control-plane + worker.
	recordClusterInRegistry(
		context.Background(),
		UpDeps{},
		"hetzner-ash",
		"hetzner",
		"ash",
		"/tmp/kube.yaml",
		[]string{"hetzner-ash", "hetzner-ash-node"},
	)

	removeNodeFromRegistry(
		context.Background(),
		RemoveNodeDeps{}, // nil OpenRegistry — fall back to registry.NewRegistry
		"hetzner-ash",
		"hetzner-ash-node",
	)

	reg, err := registry.NewRegistry(context.Background())
	if err != nil {
		t.Fatalf("reopen registry: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	nodes, err := reg.ListNodes(context.Background(), "hetzner-ash")
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 remaining node (control-plane), got %d: %+v", len(nodes), nodes)
	}
	if nodes[0].Hostname != "hetzner-ash" {
		t.Errorf("control-plane row was unexpectedly removed; remaining: %+v", nodes[0])
	}

	// Cluster row must still exist — remove-node never touches it.
	if _, err := reg.GetCluster(context.Background(), "hetzner-ash"); err != nil {
		t.Errorf("cluster row should still exist: %v", err)
	}
}
