package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/registry"
	_ "github.com/foundryfabric/clusterbox/internal/registry/sqlite"
)

// fakeRegistry is a minimal in-memory Registry used to verify what
// recordClusterInRegistry, recordNodeInRegistry, and removeNodeFromRegistry
// write. Unimplemented methods panic so accidental reliance on them shows up
// immediately.
type fakeRegistry struct {
	mu          sync.Mutex
	clusters    []registry.Cluster
	nodes       []registry.Node
	removed     [][2]string // (clusterName, hostname) pairs passed to RemoveNode
	closed      bool
	upsertErr   error
	upsertNodeE error
	removeErr   error
	closeErr    error
}

func (f *fakeRegistry) UpsertCluster(_ context.Context, c registry.Cluster) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.clusters = append(f.clusters, c)
	return nil
}

func (f *fakeRegistry) UpsertNode(_ context.Context, n registry.Node) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertNodeE != nil {
		return f.upsertNodeE
	}
	f.nodes = append(f.nodes, n)
	return nil
}

func (f *fakeRegistry) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return f.closeErr
}

// Unused interface methods.
func (f *fakeRegistry) GetCluster(context.Context, string) (registry.Cluster, error) {
	panic("not used")
}
func (f *fakeRegistry) ListClusters(context.Context) ([]registry.Cluster, error) {
	panic("not used")
}
func (f *fakeRegistry) DeleteCluster(context.Context, string) error { panic("not used") }
func (f *fakeRegistry) RemoveNode(_ context.Context, clusterName, hostname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, [2]string{clusterName, hostname})
	return nil
}
func (f *fakeRegistry) ListNodes(context.Context, string) ([]registry.Node, error) {
	panic("not used")
}
func (f *fakeRegistry) UpsertDeployment(context.Context, registry.Deployment) error {
	panic("not used")
}
func (f *fakeRegistry) GetDeployment(context.Context, string, string) (registry.Deployment, error) {
	panic("not used")
}
func (f *fakeRegistry) ListDeployments(context.Context, string) ([]registry.Deployment, error) {
	panic("not used")
}
func (f *fakeRegistry) DeleteDeployment(context.Context, string, string) error {
	panic("not used")
}
func (f *fakeRegistry) AppendHistory(context.Context, registry.DeploymentHistoryEntry) error {
	panic("not used")
}
func (f *fakeRegistry) ListHistory(context.Context, registry.HistoryFilter) ([]registry.DeploymentHistoryEntry, error) {
	panic("not used")
}
func (f *fakeRegistry) MarkSynced(context.Context, string, time.Time) error {
	panic("not used")
}
func (f *fakeRegistry) RecordResource(context.Context, registry.HetznerResource) (int64, error) {
	panic("not used")
}
func (f *fakeRegistry) MarkResourceDestroyed(context.Context, int64, time.Time) error {
	panic("not used")
}
func (f *fakeRegistry) ListResources(context.Context, string, bool) ([]registry.HetznerResource, error) {
	panic("not used")
}
func (f *fakeRegistry) ListResourcesByType(context.Context, string, string) ([]registry.HetznerResource, error) {
	panic("not used")
}
func (f *fakeRegistry) MarkClusterDestroyed(context.Context, string, time.Time) error {
	panic("not used")
}

// captureStderr redirects os.Stderr for the duration of fn and returns what
// was written to it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stderr = orig
	return <-done
}

// TestRecordClusterInRegistry_HappyPath verifies a cluster row and a single
// control-plane node row are written when the registry succeeds.
func TestRecordClusterInRegistry_HappyPath(t *testing.T) {
	fake := &fakeRegistry{}
	deps := UpDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) {
			return fake, nil
		},
	}

	recordClusterInRegistry(
		context.Background(),
		deps,
		"hetzner-ash",
		"hetzner",
		"ash",
		"prod",
		"/tmp/kube.yaml",
		[]string{"hetzner-ash"},
	)

	if !fake.closed {
		t.Errorf("registry was not closed")
	}
	if got := len(fake.clusters); got != 1 {
		t.Fatalf("expected 1 cluster, got %d", got)
	}
	c := fake.clusters[0]
	if c.Name != "hetzner-ash" || c.Provider != "hetzner" || c.Region != "ash" || c.KubeconfigPath != "/tmp/kube.yaml" {
		t.Errorf("cluster row mismatch: %+v", c)
	}
	if c.CreatedAt.IsZero() || c.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt must be set and UTC, got %v (loc=%v)", c.CreatedAt, c.CreatedAt.Location())
	}
	if !c.LastSynced.IsZero() {
		t.Errorf("LastSynced must be zero on creation, got %v", c.LastSynced)
	}

	if got := len(fake.nodes); got != 1 {
		t.Fatalf("expected 1 node, got %d", got)
	}
	n := fake.nodes[0]
	if n.ClusterName != "hetzner-ash" || n.Hostname != "hetzner-ash" || n.Role != "control-plane" {
		t.Errorf("node row mismatch: %+v", n)
	}
	if n.JoinedAt.IsZero() || n.JoinedAt.Location() != time.UTC {
		t.Errorf("JoinedAt must be set and UTC, got %v (loc=%v)", n.JoinedAt, n.JoinedAt.Location())
	}
}

// TestRecordClusterInRegistry_MultipleNodes verifies that the first node is
// labelled control-plane and subsequent nodes are workers.
func TestRecordClusterInRegistry_MultipleNodes(t *testing.T) {
	fake := &fakeRegistry{}
	deps := UpDeps{OpenRegistry: func(context.Context) (registry.Registry, error) { return fake, nil }}

	recordClusterInRegistry(
		context.Background(),
		deps,
		"hetzner-ash",
		"hetzner",
		"ash",
		"prod",
		"/tmp/kube.yaml",
		[]string{"hetzner-ash", "hetzner-ash-node-1", "hetzner-ash-node-2"},
	)

	if got := len(fake.nodes); got != 3 {
		t.Fatalf("expected 3 nodes, got %d", got)
	}
	wantRoles := []string{"control-plane", "worker", "worker"}
	for i, want := range wantRoles {
		if fake.nodes[i].Role != want {
			t.Errorf("nodes[%d].Role: want %q, got %q", i, want, fake.nodes[i].Role)
		}
	}
}

// TestRecordClusterInRegistry_OpenFailure_WarnsAndReturns verifies that
// failing to open the registry prints a warning to stderr and returns
// without panicking — runUp must still report success.
func TestRecordClusterInRegistry_OpenFailure_WarnsAndReturns(t *testing.T) {
	deps := UpDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) {
			return nil, errors.New("disk on fire")
		},
	}

	stderr := captureStderr(t, func() {
		recordClusterInRegistry(
			context.Background(),
			deps,
			"hetzner-ash",
			"hetzner",
			"ash",
			"prod",
			"/tmp/kube.yaml",
			[]string{"hetzner-ash"},
		)
	})

	if !strings.Contains(stderr, "warning: registry write failed") {
		t.Errorf("expected warning on stderr, got %q", stderr)
	}
	if !strings.Contains(stderr, "disk on fire") {
		t.Errorf("expected underlying error in warning, got %q", stderr)
	}
}

// TestRecordClusterInRegistry_UpsertFailure_WarnsAndReturns verifies that an
// UpsertCluster error is logged and does not panic.
func TestRecordClusterInRegistry_UpsertFailure_WarnsAndReturns(t *testing.T) {
	fake := &fakeRegistry{upsertErr: errors.New("constraint violation")}
	deps := UpDeps{OpenRegistry: func(context.Context) (registry.Registry, error) { return fake, nil }}

	stderr := captureStderr(t, func() {
		recordClusterInRegistry(
			context.Background(),
			deps,
			"hetzner-ash",
			"hetzner",
			"ash",
			"prod",
			"/tmp/kube.yaml",
			[]string{"hetzner-ash"},
		)
	})

	if !fake.closed {
		t.Errorf("registry must still be closed when UpsertCluster fails")
	}
	if !strings.Contains(stderr, "constraint violation") {
		t.Errorf("expected upsert error in warning, got %q", stderr)
	}
	// Node upsert must not have been attempted after cluster upsert failed.
	if got := len(fake.nodes); got != 0 {
		t.Errorf("expected 0 nodes when cluster upsert failed, got %d", got)
	}
}

// TestRecordClusterInRegistry_CloseError_Warns verifies that a Close error
// is logged but does not change the outcome.
func TestRecordClusterInRegistry_CloseError_Warns(t *testing.T) {
	fake := &fakeRegistry{closeErr: errors.New("close exploded")}
	deps := UpDeps{OpenRegistry: func(context.Context) (registry.Registry, error) { return fake, nil }}

	stderr := captureStderr(t, func() {
		recordClusterInRegistry(
			context.Background(),
			deps,
			"hetzner-ash",
			"hetzner",
			"ash",
			"prod",
			"/tmp/kube.yaml",
			[]string{"hetzner-ash"},
		)
	})

	if !strings.Contains(stderr, "close exploded") {
		t.Errorf("expected close error in warning, got %q", stderr)
	}
	if got := len(fake.clusters); got != 1 {
		t.Errorf("cluster row should still have been written, got %d", got)
	}
}

// TestRecordClusterInRegistry_DefaultsToRealRegistry verifies that a nil
// OpenRegistry falls back to registry.NewRegistry. We point HOME at a
// tempdir so the sqlite backend writes there, then assert a cluster row
// landed in that database.
func TestRecordClusterInRegistry_DefaultsToRealRegistry(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("REGISTRY_BACKEND", "")

	recordClusterInRegistry(
		context.Background(),
		UpDeps{}, // nil OpenRegistry — should fall back to registry.NewRegistry
		"hetzner-ash",
		"hetzner",
		"ash",
		"prod",
		"/tmp/kube.yaml",
		[]string{"hetzner-ash"},
	)

	dbPath := filepath.Join(tmp, ".clusterbox", "registry.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected sqlite db at %q: %v", dbPath, err)
	}

	// Reopen and confirm the cluster row + control-plane node landed.
	reg, err := registry.NewRegistry(context.Background())
	if err != nil {
		t.Fatalf("reopen registry: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	c, err := reg.GetCluster(context.Background(), "hetzner-ash")
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if c.Provider != "hetzner" || c.Region != "ash" || c.KubeconfigPath != "/tmp/kube.yaml" {
		t.Errorf("persisted cluster row mismatch: %+v", c)
	}

	nodes, err := reg.ListNodes(context.Background(), "hetzner-ash")
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Role != "control-plane" || nodes[0].Hostname != "hetzner-ash" {
		t.Errorf("persisted node rows mismatch: %+v", nodes)
	}
}
