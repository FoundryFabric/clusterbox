package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
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
		Name:      "alpha",
		Provider:  "hcloud",
		Region:    "nbg1",
		Env:       "prod",
		CreatedAt: stamp(1),
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
	if !got.CreatedAt.Equal(stamp(1)) {
		t.Fatalf("CreatedAt: want %v, got %v", stamp(1), got.CreatedAt)
	}
	if !got.LastSynced.IsZero() {
		t.Fatalf("LastSynced should be zero on fresh cluster, got %v", got.LastSynced)
	}

	// Update in place: change region.
	c.Region = "fsn1"
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

	n1 := registry.Node{ClusterName: "alpha", Hostname: "h1", Roles: []string{"control", "worker"}, CreatedAt: stamp(10)}
	n2 := registry.Node{ClusterName: "alpha", Hostname: "h2", Roles: []string{"worker"}, CreatedAt: stamp(11)}
	if err := p.UpsertNode(ctx, n1); err != nil {
		t.Fatalf("UpsertNode n1: %v", err)
	}
	if err := p.UpsertNode(ctx, n2); err != nil {
		t.Fatalf("UpsertNode n2: %v", err)
	}

	// Update in place: change roles on h1.
	n1.Roles = []string{"control"}
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
	if !reflect.DeepEqual(nodes[0].Roles, []string{"control"}) {
		t.Fatalf("h1 roles after update: %v", nodes[0].Roles)
	}
	if !reflect.DeepEqual(nodes[1].Roles, []string{"worker"}) {
		t.Fatalf("h2 roles: %v", nodes[1].Roles)
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

	d := registry.Deployment{ClusterName: "alpha", Service: "api", Version: "v1.0.0", Status: registry.StatusRolledOut, UpdatedAt: stamp(20)}
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
	if !got.UpdatedAt.Equal(stamp(20)) {
		t.Fatalf("UpdatedAt: want %v, got %v", stamp(20), got.UpdatedAt)
	}

	// Update — bump version.
	d.Version = "v1.1.0"
	d.Status = registry.StatusRolling
	d.UpdatedAt = stamp(21)
	if err := p.UpsertDeployment(ctx, d); err != nil {
		t.Fatalf("UpsertDeployment update: %v", err)
	}
	got, _ = p.GetDeployment(ctx, "alpha", "api")
	if got.Version != "v1.1.0" || got.Status != registry.StatusRolling {
		t.Fatalf("update didn't stick: %+v", got)
	}

	// Add a second deployment for List.
	if err := p.UpsertDeployment(ctx, registry.Deployment{ClusterName: "alpha", Service: "web", Version: "v0.1", Status: registry.StatusRolledOut, UpdatedAt: stamp(22)}); err != nil {
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
		{ClusterName: "alpha", Service: "api", Version: "v1.0.0", Status: registry.StatusRolling, OccurredAt: stamp(30)},
		{ClusterName: "alpha", Service: "api", Version: "v1.0.0", Status: registry.StatusRolledOut, OccurredAt: stamp(31)},
		{ClusterName: "alpha", Service: "web", Version: "v0.1", Status: registry.StatusFailed, Detail: "image pull failed", OccurredAt: stamp(32)},
		{ClusterName: "beta", Service: "api", Version: "v1.0.0", Status: registry.StatusRolledOut, OccurredAt: stamp(33)},
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
		if all[i-1].OccurredAt.Before(all[i].OccurredAt) {
			t.Fatalf("history not sorted desc: %v", all)
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

	// Detail round-trips through error column.
	got, _ = p.ListHistory(ctx, registry.HistoryFilter{ClusterName: "alpha", Service: "web"})
	if len(got) != 1 || got[0].Detail != "image pull failed" {
		t.Fatalf("detail round-trip failed: %+v", got)
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
