package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/registry/sqlite"
)

// newTempProvider opens a fresh registry in a t.TempDir-backed file. It
// closes the provider via t.Cleanup so individual tests don't have to.
func newTempProvider(t *testing.T) *sqlite.Provider {
	t.Helper()
	dir := t.TempDir()
	p, err := sqlite.New(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// stamp returns a fixed UTC time so equality comparisons in tests are
// deterministic.
func stamp(seconds int) time.Time {
	return time.Date(2026, 4, 24, 12, 0, seconds, 0, time.UTC)
}

func TestCluster_CRUD(t *testing.T) {
	p := newTempProvider(t)
	ctx := context.Background()

	// Get on missing -> ErrNotFound.
	_, err := p.GetCluster(ctx, "absent")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	c := registry.Cluster{
		Name:           "alpha",
		Provider:       "hcloud",
		Region:         "nbg1",
		Env:            "prod",
		CreatedAt:      stamp(1),
		KubeconfigPath: "/home/u/.kube/alpha.yaml",
	}
	if err := p.UpsertCluster(ctx, c); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}

	got, err := p.GetCluster(ctx, "alpha")
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if got.Name != "alpha" || got.Provider != "hcloud" || got.Region != "nbg1" || got.Env != "prod" {
		t.Fatalf("cluster fields wrong: %+v", got)
	}
	if got.KubeconfigPath != "/home/u/.kube/alpha.yaml" {
		t.Fatalf("KubeconfigPath round-trip: got %q", got.KubeconfigPath)
	}
	if !got.CreatedAt.Equal(stamp(1)) {
		t.Fatalf("CreatedAt: want %v, got %v", stamp(1), got.CreatedAt)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Fatalf("CreatedAt should be UTC, got %v", got.CreatedAt.Location())
	}
	if !got.LastSynced.IsZero() {
		t.Fatalf("LastSynced should be zero on fresh cluster, got %v", got.LastSynced)
	}

	// Update in place: change region and kubeconfig.
	c.Region = "fsn1"
	c.KubeconfigPath = "/etc/clusterbox/alpha.yaml"
	if err := p.UpsertCluster(ctx, c); err != nil {
		t.Fatalf("UpsertCluster (update): %v", err)
	}
	got, err = p.GetCluster(ctx, "alpha")
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if got.Region != "fsn1" {
		t.Fatalf("update did not stick: %+v", got)
	}
	if got.KubeconfigPath != "/etc/clusterbox/alpha.yaml" {
		t.Fatalf("KubeconfigPath update did not stick: %q", got.KubeconfigPath)
	}

	// List.
	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "beta", Provider: "hcloud", Region: "nbg1", Env: "stage", CreatedAt: stamp(2)}); err != nil {
		t.Fatalf("UpsertCluster beta: %v", err)
	}
	clusters, err := p.ListClusters(ctx)
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(clusters) != 2 {
		t.Fatalf("want 2 clusters, got %d", len(clusters))
	}

	// Delete is idempotent.
	if err := p.DeleteCluster(ctx, "absent"); err != nil {
		t.Fatalf("DeleteCluster on absent: %v", err)
	}
	if err := p.DeleteCluster(ctx, "alpha"); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	_, err = p.GetCluster(ctx, "alpha")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("after delete, want ErrNotFound, got %v", err)
	}
}

func TestNode_CRUDAndCascadeDelete(t *testing.T) {
	p := newTempProvider(t)
	ctx := context.Background()

	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(1)}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}

	n1 := registry.Node{ClusterName: "alpha", Hostname: "h1", Role: "control", JoinedAt: stamp(10)}
	n2 := registry.Node{ClusterName: "alpha", Hostname: "h2", Role: "worker", JoinedAt: stamp(11)}
	if err := p.UpsertNode(ctx, n1); err != nil {
		t.Fatalf("UpsertNode n1: %v", err)
	}
	if err := p.UpsertNode(ctx, n2); err != nil {
		t.Fatalf("UpsertNode n2: %v", err)
	}

	// Update in place: change role on h1.
	n1.Role = "worker"
	if err := p.UpsertNode(ctx, n1); err != nil {
		t.Fatalf("UpsertNode update: %v", err)
	}

	nodes, err := p.ListNodes(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(nodes))
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Hostname < nodes[j].Hostname })
	if nodes[0].Role != "worker" {
		t.Fatalf("h1 role after update: %q", nodes[0].Role)
	}
	if nodes[1].Role != "worker" {
		t.Fatalf("h2 role: %q", nodes[1].Role)
	}
	if !nodes[0].JoinedAt.Equal(stamp(10)) {
		t.Fatalf("h1 JoinedAt round-trip: got %v", nodes[0].JoinedAt)
	}
	if nodes[0].JoinedAt.Location() != time.UTC {
		t.Fatalf("JoinedAt should be UTC, got %v", nodes[0].JoinedAt.Location())
	}

	// Remove a node — idempotent on missing.
	if err := p.RemoveNode(ctx, "alpha", "absent"); err != nil {
		t.Fatalf("RemoveNode absent: %v", err)
	}
	if err := p.RemoveNode(ctx, "alpha", "h2"); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	nodes, _ = p.ListNodes(ctx, "alpha")
	if len(nodes) != 1 {
		t.Fatalf("want 1 node after remove, got %d", len(nodes))
	}

	// Cascade delete: removing the cluster removes its nodes.
	if err := p.DeleteCluster(ctx, "alpha"); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	nodes, _ = p.ListNodes(ctx, "alpha")
	if len(nodes) != 0 {
		t.Fatalf("want 0 nodes after cluster delete, got %d", len(nodes))
	}
}

func TestDeployment_CRUDAndCascade(t *testing.T) {
	p := newTempProvider(t)
	ctx := context.Background()

	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(1)}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}

	// Get on absent -> ErrNotFound.
	_, err := p.GetDeployment(ctx, "alpha", "api")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	d := registry.Deployment{
		ClusterName: "alpha",
		Service:     "api",
		Version:     "v1.0.0",
		DeployedAt:  stamp(20),
		DeployedBy:  "alice",
		Status:      registry.StatusRolledOut,
	}
	if err := p.UpsertDeployment(ctx, d); err != nil {
		t.Fatalf("UpsertDeployment: %v", err)
	}

	got, err := p.GetDeployment(ctx, "alpha", "api")
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if got.Version != "v1.0.0" || got.Status != registry.StatusRolledOut {
		t.Fatalf("deployment fields wrong: %+v", got)
	}
	if got.DeployedBy != "alice" {
		t.Fatalf("DeployedBy round-trip: got %q", got.DeployedBy)
	}
	if !got.DeployedAt.Equal(stamp(20)) {
		t.Fatalf("DeployedAt: want %v, got %v", stamp(20), got.DeployedAt)
	}
	if got.DeployedAt.Location() != time.UTC {
		t.Fatalf("DeployedAt should be UTC, got %v", got.DeployedAt.Location())
	}

	// Update — bump version, change deployer.
	d.Version = "v1.1.0"
	d.Status = registry.StatusRolling
	d.DeployedAt = stamp(21)
	d.DeployedBy = "bob"
	if err := p.UpsertDeployment(ctx, d); err != nil {
		t.Fatalf("UpsertDeployment update: %v", err)
	}
	got, _ = p.GetDeployment(ctx, "alpha", "api")
	if got.Version != "v1.1.0" || got.Status != registry.StatusRolling {
		t.Fatalf("update didn't stick: %+v", got)
	}
	if got.DeployedBy != "bob" {
		t.Fatalf("DeployedBy update did not stick: %q", got.DeployedBy)
	}

	// Add a second deployment for List.
	if err := p.UpsertDeployment(ctx, registry.Deployment{ClusterName: "alpha", Service: "web", Version: "v0.1", DeployedAt: stamp(22), DeployedBy: "alice", Status: registry.StatusRolledOut}); err != nil {
		t.Fatalf("UpsertDeployment web: %v", err)
	}
	deps, err := p.ListDeployments(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("want 2 deployments, got %d", len(deps))
	}

	// Cascade: deleting cluster removes deployments.
	if err := p.DeleteCluster(ctx, "alpha"); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	deps, _ = p.ListDeployments(ctx, "alpha")
	if len(deps) != 0 {
		t.Fatalf("want 0 deployments after cluster delete, got %d", len(deps))
	}
}

func TestHistory_AppendAndFilter(t *testing.T) {
	p := newTempProvider(t)
	ctx := context.Background()

	entries := []registry.DeploymentHistoryEntry{
		{ClusterName: "alpha", Service: "api", Version: "v1.0.0", Status: registry.StatusRolling, AttemptedAt: stamp(30), RolloutDurationMs: 0},
		{ClusterName: "alpha", Service: "api", Version: "v1.0.0", Status: registry.StatusRolledOut, AttemptedAt: stamp(31), RolloutDurationMs: 1500},
		{ClusterName: "alpha", Service: "web", Version: "v0.1", Status: registry.StatusFailed, Error: "image pull failed", AttemptedAt: stamp(32), RolloutDurationMs: 234},
		{ClusterName: "beta", Service: "api", Version: "v1.0.0", Status: registry.StatusRolledOut, AttemptedAt: stamp(33), RolloutDurationMs: 999},
	}
	for _, e := range entries {
		if err := p.AppendHistory(ctx, e); err != nil {
			t.Fatalf("AppendHistory: %v", err)
		}
	}

	// Empty filter: all four, most-recent-first.
	all, err := p.ListHistory(ctx, registry.HistoryFilter{})
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("want 4 entries, got %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].AttemptedAt.Before(all[i].AttemptedAt) {
			t.Fatalf("history not sorted desc: %v", all)
		}
	}
	for i, e := range all {
		if e.ID == 0 {
			t.Fatalf("history entry %d missing surrogate ID: %+v", i, e)
		}
		if e.AttemptedAt.Location() != time.UTC {
			t.Fatalf("AttemptedAt should be UTC, got %v", e.AttemptedAt.Location())
		}
	}

	// Filter by cluster.
	got, err := p.ListHistory(ctx, registry.HistoryFilter{ClusterName: "alpha"})
	if err != nil {
		t.Fatalf("ListHistory cluster: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 alpha entries, got %d", len(got))
	}

	// Filter by cluster + service.
	got, err = p.ListHistory(ctx, registry.HistoryFilter{ClusterName: "alpha", Service: "api"})
	if err != nil {
		t.Fatalf("ListHistory cluster+service: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 alpha/api entries, got %d", len(got))
	}

	// Filter by service alone.
	got, err = p.ListHistory(ctx, registry.HistoryFilter{Service: "api"})
	if err != nil {
		t.Fatalf("ListHistory service: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 api entries, got %d", len(got))
	}

	// Error and rollout duration round-trip.
	got, _ = p.ListHistory(ctx, registry.HistoryFilter{ClusterName: "alpha", Service: "web"})
	if len(got) != 1 || got[0].Error != "image pull failed" {
		t.Fatalf("error round-trip failed: %+v", got)
	}
	if got[0].RolloutDurationMs != 234 {
		t.Fatalf("RolloutDurationMs round-trip: got %d", got[0].RolloutDurationMs)
	}

	// Limit.
	got, err = p.ListHistory(ctx, registry.HistoryFilter{Limit: 2})
	if err != nil {
		t.Fatalf("ListHistory limit: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 with limit, got %d", len(got))
	}

	// Limit zero means no limit.
	got, _ = p.ListHistory(ctx, registry.HistoryFilter{Limit: 0})
	if len(got) != 4 {
		t.Fatalf("limit=0 should be no limit; got %d", len(got))
	}

	// History rows survive cluster delete (they are not cascaded).
	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(1)}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}
	if err := p.DeleteCluster(ctx, "alpha"); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	got, _ = p.ListHistory(ctx, registry.HistoryFilter{ClusterName: "alpha"})
	if len(got) != 3 {
		t.Fatalf("history should survive cluster delete; got %d", len(got))
	}
}

func TestMarkSynced_OnlyTargetCluster(t *testing.T) {
	p := newTempProvider(t)
	ctx := context.Background()

	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(1)}); err != nil {
		t.Fatalf("UpsertCluster alpha: %v", err)
	}
	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "beta", Provider: "hcloud", Region: "fsn1", Env: "prod", CreatedAt: stamp(2)}); err != nil {
		t.Fatalf("UpsertCluster beta: %v", err)
	}

	syncedAt := stamp(50)
	if err := p.MarkSynced(ctx, "alpha", syncedAt); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	a, _ := p.GetCluster(ctx, "alpha")
	b, _ := p.GetCluster(ctx, "beta")
	if !a.LastSynced.Equal(syncedAt) {
		t.Fatalf("alpha LastSynced: want %v, got %v", syncedAt, a.LastSynced)
	}
	if !b.LastSynced.IsZero() {
		t.Fatalf("beta LastSynced should still be zero, got %v", b.LastSynced)
	}

	// MarkSynced on absent cluster is a no-op (UPDATE matches zero rows).
	if err := p.MarkSynced(ctx, "absent", syncedAt); err != nil {
		t.Fatalf("MarkSynced absent: %v", err)
	}
}

func TestClose_Idempotent(t *testing.T) {
	dir := t.TempDir()
	p, err := sqlite.New(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestDeployment_KindRoundTrip verifies the kind column round-trips for
// every DeploymentKind constant, that an unset Kind defaults to KindApp
// (matching the SQL DEFAULT applied to rows from before the column
// existed), and that the column is read back correctly via both
// GetDeployment and ListDeployments.
func TestDeployment_KindRoundTrip(t *testing.T) {
	p := newTempProvider(t)
	ctx := context.Background()

	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(1)}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}

	cases := []struct {
		service string
		in      registry.DeploymentKind
		want    registry.DeploymentKind
	}{
		{"unset", "", registry.KindApp},
		{"app", registry.KindApp, registry.KindApp},
		{"addon", registry.KindAddon, registry.KindAddon},
		{"system", registry.KindSystem, registry.KindSystem},
	}
	for _, tc := range cases {
		d := registry.Deployment{
			ClusterName: "alpha", Service: tc.service, Version: "v1",
			DeployedAt: stamp(40), DeployedBy: "alice",
			Status: registry.StatusRolledOut, Kind: tc.in,
		}
		if err := p.UpsertDeployment(ctx, d); err != nil {
			t.Fatalf("UpsertDeployment %s: %v", tc.service, err)
		}
		got, err := p.GetDeployment(ctx, "alpha", tc.service)
		if err != nil {
			t.Fatalf("GetDeployment %s: %v", tc.service, err)
		}
		if got.Kind != tc.want {
			t.Errorf("GetDeployment %s: Kind = %q, want %q", tc.service, got.Kind, tc.want)
		}
	}

	// ListDeployments must surface the same kinds.
	deps, err := p.ListDeployments(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	byService := make(map[string]registry.DeploymentKind, len(deps))
	for _, d := range deps {
		byService[d.Service] = d.Kind
	}
	for _, tc := range cases {
		if got := byService[tc.service]; got != tc.want {
			t.Errorf("ListDeployments %s: Kind = %q, want %q", tc.service, got, tc.want)
		}
	}

	// Re-upsert flips kind, mirroring the ON CONFLICT update path.
	flip := registry.Deployment{
		ClusterName: "alpha", Service: "app", Version: "v2",
		DeployedAt: stamp(41), DeployedBy: "alice",
		Status: registry.StatusRolledOut, Kind: registry.KindAddon,
	}
	if err := p.UpsertDeployment(ctx, flip); err != nil {
		t.Fatalf("UpsertDeployment flip: %v", err)
	}
	got, _ := p.GetDeployment(ctx, "alpha", "app")
	if got.Kind != registry.KindAddon {
		t.Errorf("after re-upsert, Kind = %q, want %q", got.Kind, registry.KindAddon)
	}
}

// TestHistory_KindRoundTrip verifies the kind column on deployment_history
// round-trips for every DeploymentKind, including the zero-value default.
func TestHistory_KindRoundTrip(t *testing.T) {
	p := newTempProvider(t)
	ctx := context.Background()

	cases := []struct {
		service string
		in      registry.DeploymentKind
		want    registry.DeploymentKind
	}{
		{"unset", "", registry.KindApp},
		{"app", registry.KindApp, registry.KindApp},
		{"addon", registry.KindAddon, registry.KindAddon},
		{"system", registry.KindSystem, registry.KindSystem},
	}
	for i, tc := range cases {
		e := registry.DeploymentHistoryEntry{
			ClusterName: "alpha", Service: tc.service, Version: "v1",
			AttemptedAt: stamp(50 + i), Status: registry.StatusRolledOut,
			RolloutDurationMs: 100, Kind: tc.in,
		}
		if err := p.AppendHistory(ctx, e); err != nil {
			t.Fatalf("AppendHistory %s: %v", tc.service, err)
		}
	}

	all, err := p.ListHistory(ctx, registry.HistoryFilter{ClusterName: "alpha"})
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(all) != len(cases) {
		t.Fatalf("want %d history rows, got %d", len(cases), len(all))
	}
	byService := make(map[string]registry.DeploymentKind, len(all))
	for _, e := range all {
		byService[e.Service] = e.Kind
	}
	for _, tc := range cases {
		if got := byService[tc.service]; got != tc.want {
			t.Errorf("history %s: Kind = %q, want %q", tc.service, got, tc.want)
		}
	}
}

func TestReopen_NoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.db")

	p1, err := sqlite.New(path)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	ctx := context.Background()
	if err := p1.UpsertCluster(ctx, registry.Cluster{Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(1)}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}
	if err := p1.Close(); err != nil {
		t.Fatalf("Close p1: %v", err)
	}

	// Reopening should not throw away the prior data and migrations
	// should be a no-op the second time.
	p2, err := sqlite.New(path)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer p2.Close()

	got, err := p2.GetCluster(ctx, "alpha")
	if err != nil {
		t.Fatalf("GetCluster after reopen: %v", err)
	}
	if got.Name != "alpha" {
		t.Fatalf("reopen lost data: %+v", got)
	}
}
