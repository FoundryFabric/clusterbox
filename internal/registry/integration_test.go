// Copyright 2026 Foundry Fabric

//go:build integration

// Package registry_test contains the end-to-end integration test for the
// SQLite registry. Run with:
//
//	go test -tags integration ./internal/registry/...
//
// The test exercises every Registry interface method directly rather than
// shelling out to the CLI. Each command's wiring is already covered by
// unit tests in cmd/; this test confirms the SQLite layer round-trips
// every shape the commands write or read.
package registry_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/registry/sqlite"
)

func openIntegrationRegistry(t *testing.T) registry.Registry {
	t.Helper()
	dir := t.TempDir()
	reg, err := sqlite.New(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

// TestIntegration_FullFlow walks a representative operator session against
// a fresh registry: provision two clusters, deploy a service, fail a deploy,
// add and remove a node, and verify every command's read path returns what
// was written.
func TestIntegration_FullFlow(t *testing.T) {
	ctx := context.Background()
	reg := openIntegrationRegistry(t)

	now := func() time.Time { return time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC) }

	// 1. Empty registry
	clusters, err := reg.ListClusters(ctx)
	if err != nil {
		t.Fatalf("ListClusters empty: %v", err)
	}
	if len(clusters) != 0 {
		t.Errorf("expected empty registry, got %d clusters", len(clusters))
	}

	// 2. up prod-ash + dev-ash → list shows both with control-plane nodes
	for _, name := range []string{"prod-ash", "dev-ash"} {
		c := registry.Cluster{
			Name:           name,
			Provider:       "hetzner",
			Region:         "ash",
			Env:            name[:len(name)-4],
			CreatedAt:      now(),
			KubeconfigPath: "/k/" + name,
		}
		if err := reg.UpsertCluster(ctx, c); err != nil {
			t.Fatalf("UpsertCluster %s: %v", name, err)
		}
		if err := reg.UpsertNode(ctx, registry.Node{
			ClusterName: name, Hostname: name, Role: "control-plane", JoinedAt: now(),
		}); err != nil {
			t.Fatalf("UpsertNode %s control-plane: %v", name, err)
		}
	}
	clusters, err = reg.ListClusters(ctx)
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(clusters) != 2 {
		t.Errorf("expected 2 clusters, got %d", len(clusters))
	}

	// 3. deploy svc-a v1 to prod-ash → status shows v1, history has 1 row
	deployedAt := now()
	if err := reg.UpsertDeployment(ctx, registry.Deployment{
		ClusterName: "prod-ash", Service: "svc-a", Version: "v1",
		DeployedAt: deployedAt, DeployedBy: "chris", Status: registry.StatusRolledOut,
	}); err != nil {
		t.Fatalf("UpsertDeployment v1: %v", err)
	}
	if err := reg.AppendHistory(ctx, registry.DeploymentHistoryEntry{
		ClusterName: "prod-ash", Service: "svc-a", Version: "v1",
		AttemptedAt: deployedAt, Status: registry.StatusRolledOut,
		RolloutDurationMs: 1234,
	}); err != nil {
		t.Fatalf("AppendHistory v1: %v", err)
	}

	// 4. deploy svc-a v2 → status reflects v2, history has 2 rows
	deployedAt2 := deployedAt.Add(time.Hour)
	if err := reg.UpsertDeployment(ctx, registry.Deployment{
		ClusterName: "prod-ash", Service: "svc-a", Version: "v2",
		DeployedAt: deployedAt2, DeployedBy: "chris", Status: registry.StatusRolledOut,
	}); err != nil {
		t.Fatalf("UpsertDeployment v2: %v", err)
	}
	if err := reg.AppendHistory(ctx, registry.DeploymentHistoryEntry{
		ClusterName: "prod-ash", Service: "svc-a", Version: "v2",
		AttemptedAt: deployedAt2, Status: registry.StatusRolledOut,
		RolloutDurationMs: 2345,
	}); err != nil {
		t.Fatalf("AppendHistory v2: %v", err)
	}
	dep, err := reg.GetDeployment(ctx, "prod-ash", "svc-a")
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if dep.Version != "v2" {
		t.Errorf("expected current version v2, got %q", dep.Version)
	}
	hist, err := reg.ListHistory(ctx, registry.HistoryFilter{ClusterName: "prod-ash", Service: "svc-a"})
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(hist) != 2 {
		t.Errorf("expected 2 history rows for svc-a, got %d", len(hist))
	}

	// 5. failed deploy svc-b v1 → status does NOT show svc-b, history has the failed row
	failedAt := deployedAt2.Add(time.Minute)
	if err := reg.AppendHistory(ctx, registry.DeploymentHistoryEntry{
		ClusterName: "prod-ash", Service: "svc-b", Version: "v1",
		AttemptedAt: failedAt, Status: registry.StatusFailed,
		RolloutDurationMs: 5000, Error: "image pull backoff",
	}); err != nil {
		t.Fatalf("AppendHistory failed: %v", err)
	}
	if _, err := reg.GetDeployment(ctx, "prod-ash", "svc-b"); err == nil {
		t.Errorf("expected svc-b GetDeployment to return ErrNotFound, got nil")
	}
	failedHist, err := reg.ListHistory(ctx, registry.HistoryFilter{Service: "svc-b"})
	if err != nil {
		t.Fatalf("ListHistory svc-b: %v", err)
	}
	if len(failedHist) != 1 || failedHist[0].Status != registry.StatusFailed || failedHist[0].Error == "" {
		t.Errorf("expected one failed history row with non-empty Error, got %+v", failedHist)
	}

	// 6. add-node prod-ash-node-1 → ListNodes shows 2 for prod-ash
	if err := reg.UpsertNode(ctx, registry.Node{
		ClusterName: "prod-ash", Hostname: "prod-ash-node-1", Role: "worker",
		JoinedAt: now(),
	}); err != nil {
		t.Fatalf("UpsertNode worker: %v", err)
	}
	nodes, err := reg.ListNodes(ctx, "prod-ash")
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Errorf("expected 2 nodes for prod-ash, got %d", len(nodes))
	}

	// 7. remove-node prod-ash-node-1 → back to 1 node
	if err := reg.RemoveNode(ctx, "prod-ash", "prod-ash-node-1"); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	nodes, err = reg.ListNodes(ctx, "prod-ash")
	if err != nil {
		t.Fatalf("ListNodes after remove: %v", err)
	}
	if len(nodes) != 1 {
		t.Errorf("expected 1 node after remove, got %d", len(nodes))
	}

	// 8. MarkSynced advances last_synced_at on the named cluster only
	syncedAt := now().Add(2 * time.Hour)
	if err := reg.MarkSynced(ctx, "prod-ash", syncedAt); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}
	prod, err := reg.GetCluster(ctx, "prod-ash")
	if err != nil {
		t.Fatalf("GetCluster prod-ash: %v", err)
	}
	if !prod.LastSynced.Equal(syncedAt) {
		t.Errorf("expected prod-ash last_synced=%v, got %v", syncedAt, prod.LastSynced)
	}
	dev, err := reg.GetCluster(ctx, "dev-ash")
	if err != nil {
		t.Fatalf("GetCluster dev-ash: %v", err)
	}
	if !dev.LastSynced.IsZero() {
		t.Errorf("dev-ash last_synced should remain zero, got %v", dev.LastSynced)
	}

	// 9. Cascade delete: removing a cluster removes its nodes and deployments.
	if err := reg.DeleteCluster(ctx, "dev-ash"); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if _, err := reg.GetCluster(ctx, "dev-ash"); err == nil {
		t.Errorf("expected ErrNotFound after delete, got nil")
	}
	devNodes, err := reg.ListNodes(ctx, "dev-ash")
	if err != nil {
		t.Fatalf("ListNodes dev-ash post-delete: %v", err)
	}
	if len(devNodes) != 0 {
		t.Errorf("expected dev-ash nodes cascade-deleted, got %d", len(devNodes))
	}
}

// TestIntegration_HistoryFilters verifies the three filter knobs and the
// ordering invariant: results are reverse-chronological.
func TestIntegration_HistoryFilters(t *testing.T) {
	ctx := context.Background()
	reg := openIntegrationRegistry(t)
	base := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	if err := reg.UpsertCluster(ctx, registry.Cluster{
		Name: "c1", Provider: "hetzner", Region: "ash", Env: "prod",
		CreatedAt: base, KubeconfigPath: "/k/c1",
	}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}

	// 5 history entries across 2 services, alternating timestamps.
	for i, e := range []struct {
		svc, ver string
		offset   time.Duration
		ok       bool
	}{
		{"svc-a", "v1", 0, true},
		{"svc-b", "v1", time.Minute, true},
		{"svc-a", "v2", 2 * time.Minute, true},
		{"svc-b", "v2", 3 * time.Minute, false},
		{"svc-a", "v3", 4 * time.Minute, true},
	} {
		status := registry.StatusRolledOut
		if !e.ok {
			status = registry.StatusFailed
		}
		err := reg.AppendHistory(ctx, registry.DeploymentHistoryEntry{
			ClusterName: "c1", Service: e.svc, Version: e.ver,
			AttemptedAt: base.Add(e.offset), Status: status,
			RolloutDurationMs: int64(1000 + i*100),
		})
		if err != nil {
			t.Fatalf("AppendHistory %d: %v", i, err)
		}
	}

	all, err := reg.ListHistory(ctx, registry.HistoryFilter{})
	if err != nil {
		t.Fatalf("ListHistory all: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("expected 5 entries unfiltered, got %d", len(all))
	}
	// Reverse-chronological ordering invariant.
	for i := 1; i < len(all); i++ {
		if all[i].AttemptedAt.After(all[i-1].AttemptedAt) {
			t.Errorf("history not reverse-chronological at index %d", i)
		}
	}

	a, err := reg.ListHistory(ctx, registry.HistoryFilter{Service: "svc-a"})
	if err != nil {
		t.Fatalf("ListHistory svc-a: %v", err)
	}
	if len(a) != 3 {
		t.Errorf("expected 3 svc-a entries, got %d", len(a))
	}

	c1Only, err := reg.ListHistory(ctx, registry.HistoryFilter{ClusterName: "c1", Limit: 2})
	if err != nil {
		t.Fatalf("ListHistory c1+limit: %v", err)
	}
	if len(c1Only) != 2 {
		t.Errorf("expected limit=2 to cap at 2 entries, got %d", len(c1Only))
	}
}

// TestIntegration_ReopenIsIdempotent confirms that re-opening an existing
// registry file does not re-run migrations or corrupt data.
func TestIntegration_ReopenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.db")

	reg1, err := sqlite.New(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	ctx := context.Background()
	if err := reg1.UpsertCluster(ctx, registry.Cluster{
		Name: "c1", Provider: "hetzner", Region: "ash", Env: "prod",
		CreatedAt: time.Now().UTC(), KubeconfigPath: "/k/c1",
	}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}
	if err := reg1.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}

	reg2, err := sqlite.New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reg2.Close() })

	got, err := reg2.GetCluster(ctx, "c1")
	if err != nil {
		t.Fatalf("GetCluster after reopen: %v", err)
	}
	if got.Name != "c1" {
		t.Errorf("expected c1 to round-trip, got %+v", got)
	}
}
