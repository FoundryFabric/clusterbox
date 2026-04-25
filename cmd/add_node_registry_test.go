package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/registry"
)

// TestRecordNodeInRegistry_HappyPath verifies a worker-node row is written
// when the registry succeeds.
func TestRecordNodeInRegistry_HappyPath(t *testing.T) {
	fake := &fakeRegistry{}
	deps := AddNodeDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) {
			return fake, nil
		},
	}

	recordNodeInRegistry(context.Background(), deps, "hetzner-ash", "hetzner-ash-node")

	if !fake.closed {
		t.Errorf("registry was not closed")
	}
	if got := len(fake.clusters); got != 0 {
		t.Errorf("add-node must NOT touch the cluster row; got %d cluster writes", got)
	}
	if got := len(fake.nodes); got != 1 {
		t.Fatalf("expected 1 node, got %d", got)
	}
	n := fake.nodes[0]
	if n.ClusterName != "hetzner-ash" || n.Hostname != "hetzner-ash-node" || n.Role != "worker" {
		t.Errorf("node row mismatch: %+v", n)
	}
	if n.JoinedAt.IsZero() || n.JoinedAt.Location() != time.UTC {
		t.Errorf("JoinedAt must be set and UTC, got %v (loc=%v)", n.JoinedAt, n.JoinedAt.Location())
	}
}

// TestRecordNodeInRegistry_OpenFailure_WarnsAndReturns verifies that failing
// to open the registry prints a warning to stderr and returns without
// panicking.
func TestRecordNodeInRegistry_OpenFailure_WarnsAndReturns(t *testing.T) {
	deps := AddNodeDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) {
			return nil, errors.New("disk on fire")
		},
	}

	stderr := captureStderr(t, func() {
		recordNodeInRegistry(context.Background(), deps, "hetzner-ash", "hetzner-ash-node")
	})

	if !strings.Contains(stderr, "warning: registry write failed") {
		t.Errorf("expected warning on stderr, got %q", stderr)
	}
	if !strings.Contains(stderr, "disk on fire") {
		t.Errorf("expected underlying error in warning, got %q", stderr)
	}
}

// TestRecordNodeInRegistry_UpsertFailure_WarnsAndReturns verifies that an
// UpsertNode error is logged and does not panic.
func TestRecordNodeInRegistry_UpsertFailure_WarnsAndReturns(t *testing.T) {
	fake := &fakeRegistry{upsertNodeE: errors.New("constraint violation")}
	deps := AddNodeDeps{OpenRegistry: func(context.Context) (registry.Registry, error) { return fake, nil }}

	stderr := captureStderr(t, func() {
		recordNodeInRegistry(context.Background(), deps, "hetzner-ash", "hetzner-ash-node")
	})

	if !fake.closed {
		t.Errorf("registry must still be closed when UpsertNode fails")
	}
	if !strings.Contains(stderr, "constraint violation") {
		t.Errorf("expected upsert error in warning, got %q", stderr)
	}
}

// TestRecordNodeInRegistry_CloseError_Warns verifies that a Close error is
// logged but does not change the outcome — the node row was already written.
func TestRecordNodeInRegistry_CloseError_Warns(t *testing.T) {
	fake := &fakeRegistry{closeErr: errors.New("close exploded")}
	deps := AddNodeDeps{OpenRegistry: func(context.Context) (registry.Registry, error) { return fake, nil }}

	stderr := captureStderr(t, func() {
		recordNodeInRegistry(context.Background(), deps, "hetzner-ash", "hetzner-ash-node")
	})

	if !strings.Contains(stderr, "close exploded") {
		t.Errorf("expected close error in warning, got %q", stderr)
	}
	if got := len(fake.nodes); got != 1 {
		t.Errorf("node row should still have been written, got %d", got)
	}
}

// TestRecordNodeInRegistry_DefaultsToRealRegistry verifies that a nil
// OpenRegistry falls back to registry.NewRegistry. We point HOME at a
// tempdir so the sqlite backend writes there, then assert the node row
// landed in that database.
func TestRecordNodeInRegistry_DefaultsToRealRegistry(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("REGISTRY_BACKEND", "")

	// Seed the registry with a cluster + control-plane so the node FK holds.
	recordClusterInRegistry(
		context.Background(),
		UpDeps{},
		"hetzner-ash",
		"hetzner",
		"ash",
		"/tmp/kube.yaml",
		[]string{"hetzner-ash"},
	)

	recordNodeInRegistry(
		context.Background(),
		AddNodeDeps{}, // nil OpenRegistry — should fall back to registry.NewRegistry
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
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes (control-plane + worker), got %d: %+v", len(nodes), nodes)
	}
	var worker *registry.Node
	for i := range nodes {
		if nodes[i].Hostname == "hetzner-ash-node" {
			worker = &nodes[i]
			break
		}
	}
	if worker == nil {
		t.Fatalf("worker row not found: %+v", nodes)
	}
	if worker.Role != "worker" {
		t.Errorf("worker.Role: want %q, got %q", "worker", worker.Role)
	}
}
