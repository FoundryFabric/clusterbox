package sync_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/registry/sqlite"
	"github.com/foundryfabric/clusterbox/internal/registry/sync"
)

// fixedTime is the canonical "now" used by every test in this file. Pinning
// it removes flakiness from MarkSynced/UpsertNode timestamps and lets the
// "preserve JoinedAt across re-runs" assertions compare exact values.
var fixedTime = time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

// newFixedNow returns a time-source that always reports fixedTime.
func newFixedNow() func() time.Time { return func() time.Time { return fixedTime } }

// fakePulumi is a deterministic in-memory PulumiClient. Tests pre-populate
// the byCluster map and (optionally) errs to drive specific paths.
type fakePulumi struct {
	byCluster map[string][]sync.PulumiNode
	errs      map[string]error
}

func (f *fakePulumi) ListClusterNodes(_ context.Context, name string) ([]sync.PulumiNode, error) {
	if err, ok := f.errs[name]; ok {
		return nil, err
	}
	nodes, ok := f.byCluster[name]
	if !ok {
		// Distinguish "no entry" (= no Pulumi stack, drift) from "empty
		// slice" (stack exists, has zero nodes). Tests that want the
		// drift path leave name out of byCluster entirely.
		return nil, sync.ErrStackNotFound
	}
	return nodes, nil
}

// fakeKubectl is a deterministic KubectlRunner. The byKubeconfig map keys on
// the kubeconfig path passed by the Reconciler so a single Reconcile across
// multiple clusters can serve different responses per cluster.
type fakeKubectl struct {
	byKubeconfig map[string][]byte
	errs         map[string]error
}

func (f *fakeKubectl) Run(_ context.Context, kubeconfig string, _ ...string) ([]byte, error) {
	if err, ok := f.errs[kubeconfig]; ok {
		return nil, err
	}
	out, ok := f.byKubeconfig[kubeconfig]
	if !ok {
		return []byte(`{"items":[]}`), nil
	}
	return out, nil
}

// kubeJSON renders a kubectl-shaped JSON document for the given deployments.
// It avoids hand-writing JSON in every test and ensures the labels/image/
// status fields exercise the parser the same way kubectl would.
type kubeDep struct {
	Name        string
	Namespace   string
	AppLabel    string
	Image       string
	Replicas    int
	Ready       int
	Updated     int
	Unavailable int
}

func kubeJSON(deps ...kubeDep) []byte {
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i, d := range deps {
		if i > 0 {
			b.WriteString(",")
		}
		labels := ""
		if d.AppLabel != "" {
			labels = fmt.Sprintf(`"app.kubernetes.io/name":%q`, d.AppLabel)
		}
		fmt.Fprintf(&b,
			`{"metadata":{"name":%q,"namespace":%q,"labels":{%s}},`+
				`"spec":{"template":{"spec":{"containers":[{"image":%q}]}}},`+
				`"status":{"replicas":%d,"readyReplicas":%d,"updatedReplicas":%d,"unavailableReplicas":%d}}`,
			d.Name, d.Namespace, labels, d.Image,
			d.Replicas, d.Ready, d.Updated, d.Unavailable,
		)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// newRegistry returns a fresh sqlite-backed registry under a tempdir, with
// Close registered as a test cleanup so the underlying connection cannot
// leak across tests.
func newRegistry(t *testing.T) registry.Registry {
	t.Helper()
	dir := t.TempDir()
	p, err := sqlite.New(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// seedCluster inserts a cluster row plus optional nodes/deployments so each
// test starts from a known state.
func seedCluster(t *testing.T, reg registry.Registry, c registry.Cluster, nodes []registry.Node, deps []registry.Deployment) {
	t.Helper()
	ctx := context.Background()
	if err := reg.UpsertCluster(ctx, c); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}
	for _, n := range nodes {
		if err := reg.UpsertNode(ctx, n); err != nil {
			t.Fatalf("UpsertNode: %v", err)
		}
	}
	for _, d := range deps {
		if err := reg.UpsertDeployment(ctx, d); err != nil {
			t.Fatalf("UpsertDeployment: %v", err)
		}
	}
}

// ---- ParseDeployments unit tests ----

// TestParseDeployments_LabelFallback verifies the service name resolution
// order: app.kubernetes.io/name takes precedence over metadata.name.
func TestParseDeployments_LabelFallback(t *testing.T) {
	raw := kubeJSON(
		kubeDep{Name: "raw-name", Namespace: "default", AppLabel: "labelled-svc", Image: "ghcr.io/x/y:v1", Replicas: 1, Ready: 1, Updated: 1},
		kubeDep{Name: "fallback-svc", Namespace: "default", Image: "ghcr.io/x/y:v2", Replicas: 1, Ready: 1, Updated: 1},
	)
	got, err := sync.ParseDeployments(raw)
	if err != nil {
		t.Fatalf("ParseDeployments: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 deployments, got %d", len(got))
	}
	if got[0].Service != "labelled-svc" {
		t.Errorf("first service name should come from label, got %q", got[0].Service)
	}
	if got[1].Service != "fallback-svc" {
		t.Errorf("second service name should fall back to metadata.name, got %q", got[1].Service)
	}
}

// TestParseDeployments_ImageTagExtraction verifies the version is the
// substring after the last colon and that registry ports do not poison the
// result. An untagged image produces an empty version.
func TestParseDeployments_ImageTagExtraction(t *testing.T) {
	cases := []struct {
		image string
		want  string
	}{
		{"ghcr.io/foo/bar:v1.2.3", "v1.2.3"},
		{"registry.local:5000/foo/bar:v9", "v9"},
		// Port-only (no tag) yields empty version: the last ':' is
		// followed by a path, which the parser detects.
		{"registry.local:5000/foo/bar", ""},
		{"plain-image", ""},
	}
	for _, tc := range cases {
		raw := kubeJSON(kubeDep{Name: "svc", Namespace: "ns", Image: tc.image, Replicas: 1, Ready: 1, Updated: 1})
		got, err := sync.ParseDeployments(raw)
		if err != nil {
			t.Fatalf("ParseDeployments(%q): %v", tc.image, err)
		}
		if got[0].Version != tc.want {
			t.Errorf("image %q: want version %q, got %q", tc.image, tc.want, got[0].Version)
		}
	}
}

// TestParseDeployments_StatusMapping verifies the rollout status mapping for
// the three observable deployment shapes.
func TestParseDeployments_StatusMapping(t *testing.T) {
	cases := []struct {
		name string
		dep  kubeDep
		want registry.DeploymentStatus
	}{
		{"healthy", kubeDep{Name: "s", Namespace: "n", Image: "i:v1", Replicas: 3, Ready: 3, Updated: 3}, registry.StatusRolledOut},
		{"unavailable", kubeDep{Name: "s", Namespace: "n", Image: "i:v1", Replicas: 3, Ready: 1, Updated: 3, Unavailable: 2}, registry.StatusFailed},
		{"rolling", kubeDep{Name: "s", Namespace: "n", Image: "i:v1", Replicas: 3, Ready: 1, Updated: 2}, registry.StatusRolling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sync.ParseDeployments(kubeJSON(tc.dep))
			if err != nil {
				t.Fatalf("ParseDeployments: %v", err)
			}
			if got[0].Status != tc.want {
				t.Errorf("status: want %s, got %s", tc.want, got[0].Status)
			}
		})
	}
}

// ---- Reconciler integration tests ----

// TestReconcile_AddsDeploymentDiscoveredOutOfBand verifies that a service
// found in kubectl but not in the registry is inserted on sync (covers the
// "deployments made outside clusterbox" exit criterion).
func TestReconcile_AddsDeploymentDiscoveredOutOfBand(t *testing.T) {
	reg := newRegistry(t)
	seedCluster(t, reg,
		registry.Cluster{Name: "c1", Provider: "hetzner", Region: "ash", KubeconfigPath: "/k/c1"},
		[]registry.Node{{ClusterName: "c1", Hostname: "c1", Role: "control-plane", JoinedAt: fixedTime}},
		nil,
	)

	pulumi := &fakePulumi{byCluster: map[string][]sync.PulumiNode{
		"c1": {{Hostname: "c1", Role: "control-plane"}},
	}}
	kctl := &fakeKubectl{byKubeconfig: map[string][]byte{
		"/k/c1": kubeJSON(
			kubeDep{Name: "api", Namespace: "default", AppLabel: "api", Image: "ghcr.io/x/api:v1.0.0", Replicas: 2, Ready: 2, Updated: 2},
			kubeDep{Name: "worker", Namespace: "default", AppLabel: "worker", Image: "ghcr.io/x/worker:v0.5", Replicas: 1, Ready: 1, Updated: 1},
		),
	}}

	var warn bytes.Buffer
	r := &sync.Reconciler{Registry: reg, Pulumi: pulumi, Kubectl: kctl, Now: newFixedNow(), Warn: &warn}

	got, err := r.Reconcile(context.Background(), "", sync.Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got.Clusters != 1 {
		t.Errorf("clusters: want 1, got %d", got.Clusters)
	}
	if got.ServicesAdded != 2 {
		t.Errorf("services added: want 2, got %d", got.ServicesAdded)
	}
	if got.ServicesUpdated != 0 {
		t.Errorf("services updated: want 0, got %d", got.ServicesUpdated)
	}
	if got.DriftItems != 0 {
		t.Errorf("drift: want 0, got %d", got.DriftItems)
	}

	// Confirm the inserted rows landed and last_synced advanced.
	deps, _ := reg.ListDeployments(context.Background(), "c1")
	if len(deps) != 2 {
		t.Fatalf("registry deployments: want 2, got %d", len(deps))
	}
	got2, _ := reg.GetCluster(context.Background(), "c1")
	if !got2.LastSynced.Equal(fixedTime) {
		t.Errorf("LastSynced: want %v, got %v", fixedTime, got2.LastSynced)
	}
}

// TestReconcile_UpdatesVersionDrift verifies that an existing deployment row
// is updated when the kubectl version differs (covers "Service Y in registry
// at v1, kubectl has Y at v2 → row updated").
func TestReconcile_UpdatesVersionDrift(t *testing.T) {
	reg := newRegistry(t)
	earlier := fixedTime.Add(-24 * time.Hour)
	seedCluster(t, reg,
		registry.Cluster{Name: "c1", KubeconfigPath: "/k/c1"},
		[]registry.Node{{ClusterName: "c1", Hostname: "c1", Role: "control-plane", JoinedAt: earlier}},
		[]registry.Deployment{{
			ClusterName: "c1", Service: "api", Version: "v1.0.0",
			Status: registry.StatusRolledOut, DeployedAt: earlier, DeployedBy: "alice",
		}},
	)

	pulumi := &fakePulumi{byCluster: map[string][]sync.PulumiNode{
		"c1": {{Hostname: "c1", Role: "control-plane"}},
	}}
	kctl := &fakeKubectl{byKubeconfig: map[string][]byte{
		"/k/c1": kubeJSON(kubeDep{Name: "api", Namespace: "default", AppLabel: "api", Image: "ghcr.io/x/api:v2.0.0", Replicas: 1, Ready: 1, Updated: 1}),
	}}

	r := &sync.Reconciler{Registry: reg, Pulumi: pulumi, Kubectl: kctl, Now: newFixedNow()}
	got, err := r.Reconcile(context.Background(), "c1", sync.Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.ServicesUpdated != 1 {
		t.Errorf("services updated: want 1, got %d", got.ServicesUpdated)
	}
	if got.ServicesAdded != 0 {
		t.Errorf("services added: want 0, got %d", got.ServicesAdded)
	}

	d, err := reg.GetDeployment(context.Background(), "c1", "api")
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if d.Version != "v2.0.0" {
		t.Errorf("version: want v2.0.0, got %s", d.Version)
	}
	// DeployedBy should be preserved (not overwritten with "sync") on
	// updates so audit attribution survives reconciliation.
	if d.DeployedBy != "alice" {
		t.Errorf("DeployedBy must be preserved on update; got %q", d.DeployedBy)
	}
}

// TestReconcile_NoOpWhenAllAligned verifies that a fully-aligned cluster
// produces zero writes and no drift, but still advances LastSynced.
func TestReconcile_NoOpWhenAllAligned(t *testing.T) {
	reg := newRegistry(t)
	seedCluster(t, reg,
		registry.Cluster{Name: "c1", KubeconfigPath: "/k/c1"},
		[]registry.Node{{ClusterName: "c1", Hostname: "c1", Role: "control-plane", JoinedAt: fixedTime}},
		[]registry.Deployment{{
			ClusterName: "c1", Service: "api", Version: "v1",
			Status: registry.StatusRolledOut, DeployedAt: fixedTime,
		}},
	)
	pulumi := &fakePulumi{byCluster: map[string][]sync.PulumiNode{
		"c1": {{Hostname: "c1", Role: "control-plane"}},
	}}
	kctl := &fakeKubectl{byKubeconfig: map[string][]byte{
		"/k/c1": kubeJSON(kubeDep{Name: "api", Namespace: "default", AppLabel: "api", Image: "ghcr.io/x/api:v1", Replicas: 1, Ready: 1, Updated: 1}),
	}}

	r := &sync.Reconciler{Registry: reg, Pulumi: pulumi, Kubectl: kctl, Now: newFixedNow()}
	got, err := r.Reconcile(context.Background(), "", sync.Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.ServicesAdded != 0 || got.ServicesUpdated != 0 || got.DriftItems != 0 {
		t.Errorf("expected no-op summary, got %+v", got)
	}
}

// TestReconcile_PrunesNodesUnconditionally verifies that a registry node
// missing from Pulumi is removed even without --prune (nodes are
// Pulumi-owned by default).
func TestReconcile_PrunesNodesUnconditionally(t *testing.T) {
	reg := newRegistry(t)
	seedCluster(t, reg,
		registry.Cluster{Name: "c1", KubeconfigPath: "/k/c1"},
		[]registry.Node{
			{ClusterName: "c1", Hostname: "c1", Role: "control-plane", JoinedAt: fixedTime},
			{ClusterName: "c1", Hostname: "c1-stale", Role: "worker", JoinedAt: fixedTime},
		},
		nil,
	)
	pulumi := &fakePulumi{byCluster: map[string][]sync.PulumiNode{
		"c1": {{Hostname: "c1", Role: "control-plane"}},
	}}
	kctl := &fakeKubectl{}
	r := &sync.Reconciler{Registry: reg, Pulumi: pulumi, Kubectl: kctl, Now: newFixedNow()}

	got, err := r.Reconcile(context.Background(), "", sync.Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.NodesRemoved != 1 {
		t.Errorf("nodes removed: want 1, got %d", got.NodesRemoved)
	}
	nodes, _ := reg.ListNodes(context.Background(), "c1")
	if len(nodes) != 1 || nodes[0].Hostname != "c1" {
		t.Errorf("after sync the only remaining node should be c1; got %+v", nodes)
	}
}

// TestReconcile_PreservesNodeJoinedAt verifies that re-running sync does not
// rewrite an existing node's JoinedAt — idempotency requires the recorded
// timestamp to be sticky across runs.
func TestReconcile_PreservesNodeJoinedAt(t *testing.T) {
	reg := newRegistry(t)
	originalJoin := fixedTime.Add(-72 * time.Hour)
	seedCluster(t, reg,
		registry.Cluster{Name: "c1", KubeconfigPath: "/k/c1"},
		[]registry.Node{{ClusterName: "c1", Hostname: "c1", Role: "control-plane", JoinedAt: originalJoin}},
		nil,
	)
	pulumi := &fakePulumi{byCluster: map[string][]sync.PulumiNode{
		"c1": {{Hostname: "c1", Role: "control-plane"}},
	}}
	r := &sync.Reconciler{Registry: reg, Pulumi: pulumi, Kubectl: &fakeKubectl{}, Now: newFixedNow()}

	if _, err := r.Reconcile(context.Background(), "", sync.Options{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	nodes, _ := reg.ListNodes(context.Background(), "c1")
	if len(nodes) != 1 {
		t.Fatalf("want 1 node, got %d", len(nodes))
	}
	if !nodes[0].JoinedAt.Equal(originalJoin) {
		t.Errorf("JoinedAt should be preserved; want %v, got %v", originalJoin, nodes[0].JoinedAt)
	}
}

// TestReconcile_ClusterMissingFromPulumi_WarnsRetains verifies the default
// behaviour when a cluster has no Pulumi stack: emit a warning, count drift,
// and leave the registry row in place.
func TestReconcile_ClusterMissingFromPulumi_WarnsRetains(t *testing.T) {
	reg := newRegistry(t)
	seedCluster(t, reg,
		registry.Cluster{Name: "ghost", KubeconfigPath: "/k/ghost"},
		[]registry.Node{{ClusterName: "ghost", Hostname: "ghost", Role: "control-plane", JoinedAt: fixedTime}},
		nil,
	)
	pulumi := &fakePulumi{byCluster: map[string][]sync.PulumiNode{}} // no entry → ErrStackNotFound
	r := &sync.Reconciler{Registry: reg, Pulumi: pulumi, Kubectl: &fakeKubectl{}, Now: newFixedNow()}
	var warn bytes.Buffer
	r.Warn = &warn

	got, err := r.Reconcile(context.Background(), "", sync.Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.DriftItems != 1 {
		t.Errorf("drift: want 1, got %d", got.DriftItems)
	}
	if !strings.Contains(warn.String(), `cluster "ghost"`) {
		t.Errorf("expected warning to mention cluster, got: %s", warn.String())
	}
	if _, err := reg.GetCluster(context.Background(), "ghost"); err != nil {
		t.Errorf("ghost cluster should still exist without --prune; got %v", err)
	}
}

// TestReconcile_ClusterMissingFromPulumi_PruneDeletes verifies that --prune
// flips the warn-and-retain behaviour into a hard delete.
func TestReconcile_ClusterMissingFromPulumi_PruneDeletes(t *testing.T) {
	reg := newRegistry(t)
	seedCluster(t, reg,
		registry.Cluster{Name: "ghost", KubeconfigPath: "/k/ghost"},
		nil, nil,
	)
	pulumi := &fakePulumi{byCluster: map[string][]sync.PulumiNode{}}
	r := &sync.Reconciler{Registry: reg, Pulumi: pulumi, Kubectl: &fakeKubectl{}, Now: newFixedNow()}

	if _, err := r.Reconcile(context.Background(), "", sync.Options{Prune: true}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := reg.GetCluster(context.Background(), "ghost"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("expected ghost to be pruned; got %v", err)
	}
}

// TestReconcile_DryRunDoesNotMutate verifies that DryRun makes Reconcile
// produce a non-zero summary without writing anything to the registry.
func TestReconcile_DryRunDoesNotMutate(t *testing.T) {
	reg := newRegistry(t)
	seedCluster(t, reg,
		registry.Cluster{Name: "c1", KubeconfigPath: "/k/c1", LastSynced: time.Time{}},
		nil, nil,
	)
	pulumi := &fakePulumi{byCluster: map[string][]sync.PulumiNode{
		"c1": {{Hostname: "c1", Role: "control-plane"}},
	}}
	kctl := &fakeKubectl{byKubeconfig: map[string][]byte{
		"/k/c1": kubeJSON(kubeDep{Name: "api", Namespace: "default", AppLabel: "api", Image: "ghcr.io/x/api:v1", Replicas: 1, Ready: 1, Updated: 1}),
	}}
	r := &sync.Reconciler{Registry: reg, Pulumi: pulumi, Kubectl: kctl, Now: newFixedNow()}

	got, err := r.Reconcile(context.Background(), "", sync.Options{DryRun: true})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.ServicesAdded != 1 {
		t.Errorf("dry-run summary should still count work; got %+v", got)
	}
	if got.NodesUpserted != 1 {
		t.Errorf("dry-run summary should count node work; got %+v", got)
	}
	// Nothing should have actually been written.
	deps, _ := reg.ListDeployments(context.Background(), "c1")
	if len(deps) != 0 {
		t.Errorf("dry-run wrote %d deployments to the registry", len(deps))
	}
	nodes, _ := reg.ListNodes(context.Background(), "c1")
	if len(nodes) != 0 {
		t.Errorf("dry-run wrote %d nodes to the registry", len(nodes))
	}
	c, _ := reg.GetCluster(context.Background(), "c1")
	if !c.LastSynced.IsZero() {
		t.Errorf("dry-run must not advance LastSynced; got %v", c.LastSynced)
	}
}

// TestReconcile_ServiceInRegistryNotInKubectl_Warns verifies a row that has
// no kubectl counterpart is logged as drift but retained without --prune.
func TestReconcile_ServiceInRegistryNotInKubectl_Warns(t *testing.T) {
	reg := newRegistry(t)
	seedCluster(t, reg,
		registry.Cluster{Name: "c1", KubeconfigPath: "/k/c1"},
		[]registry.Node{{ClusterName: "c1", Hostname: "c1", Role: "control-plane", JoinedAt: fixedTime}},
		[]registry.Deployment{{
			ClusterName: "c1", Service: "stale", Version: "v1",
			Status: registry.StatusRolledOut, DeployedAt: fixedTime,
		}},
	)
	pulumi := &fakePulumi{byCluster: map[string][]sync.PulumiNode{
		"c1": {{Hostname: "c1", Role: "control-plane"}},
	}}
	kctl := &fakeKubectl{byKubeconfig: map[string][]byte{
		"/k/c1": []byte(`{"items":[]}`),
	}}
	var warn bytes.Buffer
	r := &sync.Reconciler{Registry: reg, Pulumi: pulumi, Kubectl: kctl, Now: newFixedNow(), Warn: &warn}

	got, err := r.Reconcile(context.Background(), "", sync.Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.DriftItems != 1 {
		t.Errorf("drift: want 1, got %d", got.DriftItems)
	}
	d, err := reg.GetDeployment(context.Background(), "c1", "stale")
	if err != nil {
		t.Errorf("stale row should still exist; got %v", err)
	}
	if d.Version != "v1" {
		t.Errorf("stale row should be unchanged; got %+v", d)
	}
	if !strings.Contains(warn.String(), "stale") {
		t.Errorf("expected warning to mention the service, got: %s", warn.String())
	}
}

// TestReconcile_KubectlError_DoesNotAdvanceLastSynced verifies that a
// kubectl failure logs and leaves the cluster's LastSynced untouched, so
// `clusterbox list` cannot misreport a partially-synced cluster as fresh.
func TestReconcile_KubectlError_DoesNotAdvanceLastSynced(t *testing.T) {
	reg := newRegistry(t)
	previous := fixedTime.Add(-time.Hour)
	seedCluster(t, reg,
		registry.Cluster{Name: "c1", KubeconfigPath: "/k/c1", LastSynced: previous},
		[]registry.Node{{ClusterName: "c1", Hostname: "c1", Role: "control-plane", JoinedAt: previous}},
		nil,
	)
	if err := reg.MarkSynced(context.Background(), "c1", previous); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}
	pulumi := &fakePulumi{byCluster: map[string][]sync.PulumiNode{
		"c1": {{Hostname: "c1", Role: "control-plane"}},
	}}
	kctl := &fakeKubectl{errs: map[string]error{"/k/c1": errors.New("kubectl: connection refused")}}
	var warn bytes.Buffer
	r := &sync.Reconciler{Registry: reg, Pulumi: pulumi, Kubectl: kctl, Now: newFixedNow(), Warn: &warn}

	if _, err := r.Reconcile(context.Background(), "", sync.Options{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	c, _ := reg.GetCluster(context.Background(), "c1")
	if !c.LastSynced.Equal(previous.UTC()) {
		t.Errorf("LastSynced must not advance on partial failure; want %v, got %v", previous.UTC(), c.LastSynced)
	}
	if !strings.Contains(warn.String(), "connection refused") {
		t.Errorf("kubectl error must be surfaced; got: %s", warn.String())
	}
}

// TestReconcile_ContextCancellation verifies that a cancelled context
// causes Reconcile to fail fast with the cancellation error rather than
// silently producing a misleading summary.
func TestReconcile_ContextCancellation(t *testing.T) {
	reg := newRegistry(t)
	seedCluster(t, reg, registry.Cluster{Name: "c1", KubeconfigPath: "/k/c1"}, nil, nil)
	pulumi := &fakePulumi{byCluster: map[string][]sync.PulumiNode{"c1": {}}}
	kctl := &fakeKubectl{errs: map[string]error{"/k/c1": context.Canceled}}
	r := &sync.Reconciler{Registry: reg, Pulumi: pulumi, Kubectl: kctl, Now: newFixedNow()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Reconcile(ctx, "", sync.Options{})
	if err == nil {
		t.Fatal("Reconcile should propagate the cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected wrapped context.Canceled; got %v", err)
	}
}

// TestReconcile_RequiresDependencies verifies the Reconciler refuses to run
// when one of its required dependencies is nil. The check protects against
// "the test forgot to wire the kubectl mock" footguns.
func TestReconcile_RequiresDependencies(t *testing.T) {
	r := &sync.Reconciler{}
	if _, err := r.Reconcile(context.Background(), "", sync.Options{}); err == nil {
		t.Error("expected error when reconciler has nil dependencies")
	}
}

// TestReconcile_NamedClusterIsolation verifies that requesting a single
// cluster does not touch the others.
func TestReconcile_NamedClusterIsolation(t *testing.T) {
	reg := newRegistry(t)
	seedCluster(t, reg, registry.Cluster{Name: "c1", KubeconfigPath: "/k/c1"}, nil, nil)
	seedCluster(t, reg, registry.Cluster{Name: "c2", KubeconfigPath: "/k/c2"}, nil, nil)

	pulumi := &fakePulumi{byCluster: map[string][]sync.PulumiNode{
		"c1": {{Hostname: "c1", Role: "control-plane"}},
		"c2": {{Hostname: "c2", Role: "control-plane"}},
	}}
	kctl := &fakeKubectl{byKubeconfig: map[string][]byte{
		"/k/c1": kubeJSON(kubeDep{Name: "api", Namespace: "default", AppLabel: "api", Image: "i:v1", Replicas: 1, Ready: 1, Updated: 1}),
		"/k/c2": kubeJSON(kubeDep{Name: "api", Namespace: "default", AppLabel: "api", Image: "i:v1", Replicas: 1, Ready: 1, Updated: 1}),
	}}
	r := &sync.Reconciler{Registry: reg, Pulumi: pulumi, Kubectl: kctl, Now: newFixedNow()}

	got, err := r.Reconcile(context.Background(), "c1", sync.Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Clusters != 1 {
		t.Errorf("clusters: want 1, got %d", got.Clusters)
	}
	deps2, _ := reg.ListDeployments(context.Background(), "c2")
	if len(deps2) != 0 {
		t.Errorf("c2 should be untouched when targeting c1, got %d deployments", len(deps2))
	}
}
