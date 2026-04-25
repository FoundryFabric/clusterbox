package cmd_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/cmd"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/registry/sqlite"
	"github.com/foundryfabric/clusterbox/internal/registry/sync"
)

// fakePulumi is a stub PulumiClient used by the cmd-level integration test.
// It is intentionally tiny: anything more elaborate belongs in the
// internal/registry/sync test file.
type fakePulumi struct {
	byCluster map[string][]sync.PulumiNode
}

func (f *fakePulumi) ListClusterNodes(_ context.Context, name string) ([]sync.PulumiNode, error) {
	nodes, ok := f.byCluster[name]
	if !ok {
		return nil, sync.ErrStackNotFound
	}
	return nodes, nil
}

// fakeKubectl serves canned JSON keyed on the kubeconfig path.
type fakeKubectl struct {
	byKubeconfig map[string][]byte
}

func (f *fakeKubectl) Run(_ context.Context, kubeconfig string, _ ...string) ([]byte, error) {
	if out, ok := f.byKubeconfig[kubeconfig]; ok {
		return out, nil
	}
	return []byte(`{"items":[]}`), nil
}

// noCloseRegistry wraps a Registry so RunSync's defer-Close cannot close
// the underlying sqlite handle while the test still wants to read from it.
// The real Close is registered as a t.Cleanup instead.
type noCloseRegistry struct{ registry.Registry }

func (noCloseRegistry) Close() error { return nil }

// newRegistry creates a fresh sqlite-backed registry under a tempdir,
// returning a no-close wrapper so tests can assert on it after RunSync
// returns. The real underlying handle is closed via t.Cleanup.
func newRegistry(t *testing.T) registry.Registry {
	t.Helper()
	dir := t.TempDir()
	p, err := sqlite.New(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return noCloseRegistry{Registry: p}
}

// kubeJSON is a tiny helper that builds a kubectl deployment list JSON
// document with one item.
func kubeJSON(svc, version string) []byte {
	return []byte(`{"items":[{"metadata":{"name":"` + svc + `","namespace":"default","labels":{"app.kubernetes.io/name":"` + svc + `"}},` +
		`"spec":{"template":{"spec":{"containers":[{"image":"ghcr.io/x/` + svc + `:` + version + `"}]}}},` +
		`"status":{"replicas":1,"readyReplicas":1,"updatedReplicas":1,"unavailableReplicas":0}}]}`)
}

// TestRunSync_HappyPath_RegistryUpdated wires the full RunSync flow with
// mocked Pulumi/kubectl and a real sqlite registry, then asserts that
// drift is added, the summary lands on stdout, and last_synced advances.
func TestRunSync_HappyPath_RegistryUpdated(t *testing.T) {
	reg := newRegistry(t)
	ctx := context.Background()
	if err := reg.UpsertCluster(ctx, registry.Cluster{Name: "c1", KubeconfigPath: "/k/c1"}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}

	deps := cmd.SyncDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		Pulumi: &fakePulumi{byCluster: map[string][]sync.PulumiNode{
			"c1": {{Hostname: "c1", Role: "control-plane"}},
		}},
		Kubectl: &fakeKubectl{byKubeconfig: map[string][]byte{
			"/k/c1": kubeJSON("api", "v1.0.0"),
		}},
	}

	var stdout, stderr bytes.Buffer
	if err := cmd.RunSync(ctx, "", sync.Options{}, deps, &stdout, &stderr); err != nil {
		t.Fatalf("RunSync: %v", err)
	}

	got := stdout.String()
	if !strings.Contains(got, "synced 1 cluster") {
		t.Errorf("expected summary on stdout, got: %q", got)
	}
	if !strings.Contains(got, "added 1 service") {
		t.Errorf("expected added=1 in summary, got: %q", got)
	}

	deps2, _ := reg.ListDeployments(ctx, "c1")
	if len(deps2) != 1 || deps2[0].Service != "api" || deps2[0].Version != "v1.0.0" {
		t.Errorf("expected api@v1.0.0 to be inserted; got %+v", deps2)
	}
	c, _ := reg.GetCluster(ctx, "c1")
	if c.LastSynced.IsZero() {
		t.Errorf("LastSynced should be set after a successful sync")
	}
}

// TestRunSync_DryRun_NoMutation verifies the --dry-run path does not write
// to the registry but still emits a meaningful summary.
func TestRunSync_DryRun_NoMutation(t *testing.T) {
	reg := newRegistry(t)
	ctx := context.Background()
	if err := reg.UpsertCluster(ctx, registry.Cluster{Name: "c1", KubeconfigPath: "/k/c1"}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}

	deps := cmd.SyncDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		Pulumi: &fakePulumi{byCluster: map[string][]sync.PulumiNode{
			"c1": {{Hostname: "c1", Role: "control-plane"}},
		}},
		Kubectl: &fakeKubectl{byKubeconfig: map[string][]byte{
			"/k/c1": kubeJSON("api", "v1"),
		}},
	}

	var stdout, stderr bytes.Buffer
	if err := cmd.RunSync(ctx, "", sync.Options{DryRun: true}, deps, &stdout, &stderr); err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if !strings.Contains(stdout.String(), "would sync") {
		t.Errorf("expected dry-run prefix on stdout, got: %q", stdout.String())
	}
	d, _ := reg.ListDeployments(ctx, "c1")
	if len(d) != 0 {
		t.Errorf("dry-run wrote %d deployments to the registry", len(d))
	}
}

// TestRunSync_PruneDeletesGhostCluster verifies the cobra-level wiring of
// --prune produces a real DeleteCluster against the registry.
func TestRunSync_PruneDeletesGhostCluster(t *testing.T) {
	reg := newRegistry(t)
	ctx := context.Background()
	if err := reg.UpsertCluster(ctx, registry.Cluster{Name: "ghost", KubeconfigPath: "/k/ghost"}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}
	deps := cmd.SyncDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		Pulumi:       &fakePulumi{byCluster: map[string][]sync.PulumiNode{}},
		Kubectl:      &fakeKubectl{},
	}

	var stdout, stderr bytes.Buffer
	if err := cmd.RunSync(ctx, "", sync.Options{Prune: true}, deps, &stdout, &stderr); err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if _, err := reg.GetCluster(ctx, "ghost"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("expected ghost to be pruned; got %v", err)
	}
}

// TestRunSync_OpenRegistryFailure_Surfaced verifies that a registry-open
// failure aborts RunSync with a wrapped error rather than swallowing it.
func TestRunSync_OpenRegistryFailure_Surfaced(t *testing.T) {
	deps := cmd.SyncDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) {
			return nil, errors.New("disk on fire")
		},
		Pulumi:  &fakePulumi{},
		Kubectl: &fakeKubectl{},
	}
	var stdout, stderr bytes.Buffer
	err := cmd.RunSync(context.Background(), "", sync.Options{}, deps, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("expected wrapped registry error; got %v", err)
	}
}

// TestRunSync_NamedClusterTargetsOnlyThatCluster verifies that passing a
// cluster name to RunSync limits the reconcile to a single registry row.
func TestRunSync_NamedClusterTargetsOnlyThatCluster(t *testing.T) {
	reg := newRegistry(t)
	ctx := context.Background()
	for _, name := range []string{"c1", "c2"} {
		if err := reg.UpsertCluster(ctx, registry.Cluster{Name: name, KubeconfigPath: "/k/" + name}); err != nil {
			t.Fatalf("UpsertCluster %q: %v", name, err)
		}
	}

	deps := cmd.SyncDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		Pulumi: &fakePulumi{byCluster: map[string][]sync.PulumiNode{
			"c1": {{Hostname: "c1", Role: "control-plane"}},
			"c2": {{Hostname: "c2", Role: "control-plane"}},
		}},
		Kubectl: &fakeKubectl{byKubeconfig: map[string][]byte{
			"/k/c1": kubeJSON("api", "v1"),
			"/k/c2": kubeJSON("api", "v1"),
		}},
	}

	var stdout, stderr bytes.Buffer
	if err := cmd.RunSync(ctx, "c1", sync.Options{}, deps, &stdout, &stderr); err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if !strings.Contains(stdout.String(), "synced 1 cluster") {
		t.Errorf("expected summary to mention 1 cluster, got %q", stdout.String())
	}
	d2, _ := reg.ListDeployments(ctx, "c2")
	if len(d2) != 0 {
		t.Errorf("c2 must be untouched when sync targets c1; got %d deployments", len(d2))
	}
	c2, _ := reg.GetCluster(ctx, "c2")
	if !c2.LastSynced.IsZero() {
		t.Errorf("c2 LastSynced must be untouched, got %v", c2.LastSynced)
	}
}
