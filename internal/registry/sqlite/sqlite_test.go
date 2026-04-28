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
	if got.ID == 0 {
		t.Fatalf("GetCluster: ID should be non-zero after insert, got 0")
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

	// Create clusters so AppendHistory can resolve cluster_id.
	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(1)}); err != nil {
		t.Fatalf("UpsertCluster alpha: %v", err)
	}
	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "beta", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(2)}); err != nil {
		t.Fatalf("UpsertCluster beta: %v", err)
	}

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
	// DeleteCluster removes the cluster row; AppendHistory recorded the
	// cluster_name text so ListHistory can still find the rows by name.
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

	// Create cluster so AppendHistory can resolve cluster_id.
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

func TestHetznerResources_RecordAndList(t *testing.T) {
	p := newTempProvider(t)
	ctx := context.Background()

	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(1)}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}

	srv := registry.ClusterResource{
		ClusterName:  "alpha",
		Provider:     registry.ProviderHetzner,
		ResourceType: registry.ResourceServer,
		ExternalID:   "12345",
		Hostname:     "alpha-cp-1",
		CreatedAt:    stamp(40),
		Metadata:     `{"datacenter":"nbg1-dc3"}`,
	}
	srvID, err := p.RecordResource(ctx, srv)
	if err != nil {
		t.Fatalf("RecordResource server: %v", err)
	}
	if srvID == 0 {
		t.Fatal("RecordResource returned id=0")
	}

	lb := registry.ClusterResource{
		ClusterName:  "alpha",
		Provider:     registry.ProviderHetzner,
		ResourceType: registry.ResourceLoadBalancer,
		ExternalID:   "67890",
		// Hostname intentionally empty -> persisted as NULL.
		CreatedAt: stamp(41),
	}
	lbID, err := p.RecordResource(ctx, lb)
	if err != nil {
		t.Fatalf("RecordResource lb: %v", err)
	}
	if lbID == srvID {
		t.Fatalf("RecordResource ids should be unique; got %d twice", lbID)
	}

	// List active only — both rows should be visible.
	active, err := p.ListResources(ctx, "alpha", false)
	if err != nil {
		t.Fatalf("ListResources active: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("want 2 active resources, got %d", len(active))
	}

	// Order is by created_at ASC, id ASC: server first (stamp 40), lb second (stamp 41).
	if active[0].ID != srvID || active[1].ID != lbID {
		t.Fatalf("ListResources order: got [%d,%d], want [%d,%d]", active[0].ID, active[1].ID, srvID, lbID)
	}
	if active[0].ExternalID != "12345" || active[0].Hostname != "alpha-cp-1" {
		t.Fatalf("server fields wrong: %+v", active[0])
	}
	if active[0].Provider != registry.ProviderHetzner {
		t.Fatalf("server Provider: %q", active[0].Provider)
	}
	if active[0].ResourceType != registry.ResourceServer {
		t.Fatalf("server ResourceType: %q", active[0].ResourceType)
	}
	if active[0].Metadata != `{"datacenter":"nbg1-dc3"}` {
		t.Fatalf("metadata round-trip: %q", active[0].Metadata)
	}
	if !active[0].CreatedAt.Equal(stamp(40)) {
		t.Fatalf("CreatedAt round-trip: got %v", active[0].CreatedAt)
	}
	if active[0].CreatedAt.Location() != time.UTC {
		t.Fatalf("CreatedAt should be UTC, got %v", active[0].CreatedAt.Location())
	}
	if !active[0].DestroyedAt.IsZero() {
		t.Fatalf("active row should have zero DestroyedAt, got %v", active[0].DestroyedAt)
	}
	// Empty hostname round-trips as "" (was stored as NULL).
	if active[1].Hostname != "" {
		t.Fatalf("lb hostname: want empty string, got %q", active[1].Hostname)
	}
	if active[1].Metadata != "" {
		t.Fatalf("lb metadata: want empty string, got %q", active[1].Metadata)
	}
}

func TestHetznerResources_MarkDestroyedIdempotent(t *testing.T) {
	p := newTempProvider(t)
	ctx := context.Background()

	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(1)}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}

	id, err := p.RecordResource(ctx, registry.ClusterResource{
		ClusterName:  "alpha",
		Provider:     registry.ProviderHetzner,
		ResourceType: registry.ResourceFirewall,
		ExternalID:   "fw-1",
		CreatedAt:    stamp(40),
	})
	if err != nil {
		t.Fatalf("RecordResource: %v", err)
	}

	// First stamp.
	first := stamp(50)
	if err := p.MarkResourceDestroyed(ctx, id, first); err != nil {
		t.Fatalf("MarkResourceDestroyed: %v", err)
	}

	// Second stamp must be a no-op (preserves the original).
	if err := p.MarkResourceDestroyed(ctx, id, stamp(99)); err != nil {
		t.Fatalf("MarkResourceDestroyed (idempotent): %v", err)
	}

	all, err := p.ListResources(ctx, "alpha", true)
	if err != nil {
		t.Fatalf("ListResources includeDestroyed=true: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 row (destroyed included), got %d", len(all))
	}
	if !all[0].DestroyedAt.Equal(first) {
		t.Fatalf("DestroyedAt should still be first stamp %v, got %v", first, all[0].DestroyedAt)
	}
	if all[0].DestroyedAt.Location() != time.UTC {
		t.Fatalf("DestroyedAt should be UTC, got %v", all[0].DestroyedAt.Location())
	}

	// Active list must now exclude the destroyed row.
	active, err := p.ListResources(ctx, "alpha", false)
	if err != nil {
		t.Fatalf("ListResources active: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("want 0 active rows, got %d", len(active))
	}

	// Stamping a non-existent id is a silent no-op.
	if err := p.MarkResourceDestroyed(ctx, 999999, stamp(60)); err != nil {
		t.Fatalf("MarkResourceDestroyed unknown id: %v", err)
	}
}

func TestHetznerResources_ListByType(t *testing.T) {
	p := newTempProvider(t)
	ctx := context.Background()

	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(1)}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}

	rows := []registry.ClusterResource{
		{ClusterName: "alpha", Provider: registry.ProviderHetzner, ResourceType: registry.ResourceServer, ExternalID: "s1", CreatedAt: stamp(40)},
		{ClusterName: "alpha", Provider: registry.ProviderHetzner, ResourceType: registry.ResourceServer, ExternalID: "s2", CreatedAt: stamp(41)},
		{ClusterName: "alpha", Provider: registry.ProviderHetzner, ResourceType: registry.ResourceVolume, ExternalID: "v1", CreatedAt: stamp(42)},
		{ClusterName: "alpha", Provider: registry.ProviderHetzner, ResourceType: registry.ResourceServer, ExternalID: "s3", CreatedAt: stamp(43)},
	}
	ids := make([]int64, len(rows))
	for i, r := range rows {
		id, err := p.RecordResource(ctx, r)
		if err != nil {
			t.Fatalf("RecordResource %d: %v", i, err)
		}
		ids[i] = id
	}

	// Destroy the second server — ListResourcesByType filters destroyed rows.
	if err := p.MarkResourceDestroyed(ctx, ids[1], stamp(50)); err != nil {
		t.Fatalf("MarkResourceDestroyed: %v", err)
	}

	servers, err := p.ListResourcesByType(ctx, "alpha", string(registry.ResourceServer))
	if err != nil {
		t.Fatalf("ListResourcesByType server: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("want 2 active servers, got %d", len(servers))
	}
	if servers[0].ExternalID != "s1" || servers[1].ExternalID != "s3" {
		t.Fatalf("ListResourcesByType order/content wrong: %+v", servers)
	}

	volumes, err := p.ListResourcesByType(ctx, "alpha", string(registry.ResourceVolume))
	if err != nil {
		t.Fatalf("ListResourcesByType volume: %v", err)
	}
	if len(volumes) != 1 || volumes[0].ExternalID != "v1" {
		t.Fatalf("volumes wrong: %+v", volumes)
	}

	// Unknown type -> empty slice, no error.
	none, err := p.ListResourcesByType(ctx, "alpha", "ghost")
	if err != nil {
		t.Fatalf("ListResourcesByType unknown: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("unknown type should be empty, got %d", len(none))
	}
}

func TestHetznerResources_CascadeDelete(t *testing.T) {
	p := newTempProvider(t)
	ctx := context.Background()

	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(1)}); err != nil {
		t.Fatalf("UpsertCluster alpha: %v", err)
	}
	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "beta", Provider: "hcloud", Region: "fsn1", Env: "prod", CreatedAt: stamp(2)}); err != nil {
		t.Fatalf("UpsertCluster beta: %v", err)
	}

	if _, err := p.RecordResource(ctx, registry.ClusterResource{ClusterName: "alpha", Provider: registry.ProviderHetzner, ResourceType: registry.ResourceServer, ExternalID: "a1", CreatedAt: stamp(40)}); err != nil {
		t.Fatalf("RecordResource alpha: %v", err)
	}
	if _, err := p.RecordResource(ctx, registry.ClusterResource{ClusterName: "beta", Provider: registry.ProviderHetzner, ResourceType: registry.ResourceServer, ExternalID: "b1", CreatedAt: stamp(41)}); err != nil {
		t.Fatalf("RecordResource beta: %v", err)
	}

	// Cascade: deleting alpha removes its resources but not beta's.
	if err := p.DeleteCluster(ctx, "alpha"); err != nil {
		t.Fatalf("DeleteCluster alpha: %v", err)
	}
	got, err := p.ListResources(ctx, "alpha", true)
	if err != nil {
		t.Fatalf("ListResources alpha: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("alpha resources should be cascade-deleted; got %d", len(got))
	}
	got, err = p.ListResources(ctx, "beta", true)
	if err != nil {
		t.Fatalf("ListResources beta: %v", err)
	}
	if len(got) != 1 || got[0].ExternalID != "b1" {
		t.Fatalf("beta resources should be untouched; got %+v", got)
	}
}

// TestMarkClusterDestroyed_TombstoneAndIdempotent verifies that
// MarkClusterDestroyed stamps the destroyed_at column without removing the row,
// is idempotent (a second stamp preserves the original time), and that after
// destruction the cluster is no longer visible via GetCluster (which only
// returns active clusters). It also verifies that a subsequent UpsertCluster
// with the same name creates a new cluster lifetime with a different ID, and
// that unknown-name calls are silent no-ops.
func TestMarkClusterDestroyed_TombstoneAndIdempotent(t *testing.T) {
	p := newTempProvider(t)
	ctx := context.Background()

	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(1)}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}

	// Fresh cluster: GetCluster returns it with zero DestroyedAt.
	got, err := p.GetCluster(ctx, "alpha")
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if !got.DestroyedAt.IsZero() {
		t.Fatalf("fresh cluster DestroyedAt should be zero, got %v", got.DestroyedAt)
	}
	firstID := got.ID
	if firstID == 0 {
		t.Fatal("fresh cluster ID should be non-zero")
	}

	first := stamp(60)
	if err := p.MarkClusterDestroyed(ctx, "alpha", first); err != nil {
		t.Fatalf("MarkClusterDestroyed: %v", err)
	}

	// After destruction GetCluster returns ErrNotFound (only returns active clusters).
	_, err = p.GetCluster(ctx, "alpha")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("after mark-destroyed, GetCluster should return ErrNotFound, got %v", err)
	}

	// Idempotent: stamping again is a no-op (the WHERE destroyed_at IS NULL
	// clause matches zero rows on second call, which is fine).
	if err := p.MarkClusterDestroyed(ctx, "alpha", stamp(99)); err != nil {
		t.Fatalf("MarkClusterDestroyed (second): %v", err)
	}

	// UpsertCluster after destruction creates a new cluster lifetime with a
	// different ID. The name is reused but the row is distinct.
	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(1)}); err != nil {
		t.Fatalf("UpsertCluster after destroy: %v", err)
	}
	newCluster, err := p.GetCluster(ctx, "alpha")
	if err != nil {
		t.Fatalf("GetCluster after re-insert: %v", err)
	}
	if newCluster.ID == firstID {
		t.Fatalf("re-inserted cluster should have a new ID; both are %d", firstID)
	}
	if !newCluster.DestroyedAt.IsZero() {
		t.Fatalf("new cluster should be active (DestroyedAt zero), got %v", newCluster.DestroyedAt)
	}

	// Unknown cluster is a silent no-op.
	if err := p.MarkClusterDestroyed(ctx, "absent", stamp(60)); err != nil {
		t.Fatalf("MarkClusterDestroyed absent: %v", err)
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
	defer func() { _ = p2.Close() }()

	got, err := p2.GetCluster(ctx, "alpha")
	if err != nil {
		t.Fatalf("GetCluster after reopen: %v", err)
	}
	if got.Name != "alpha" {
		t.Fatalf("reopen lost data: %+v", got)
	}
}

// TestNode_MetadataRoundTrip verifies the inspection metadata columns
// (arch, os_version, k3s_version, agent_version, last_inspected_at) on the
// nodes table round-trip through UpsertNode/ListNodes, that an unset Node
// (zero-value metadata) reads back as zero values (NULL is mapped to ""
// and zero time), and that a re-upsert overwrites the metadata in place.
func TestNode_MetadataRoundTrip(t *testing.T) {
	p := newTempProvider(t)
	ctx := context.Background()

	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(1)}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}

	// Insert a node with no metadata — every new column should persist as
	// NULL and read back as the zero value.
	bare := registry.Node{ClusterName: "alpha", Hostname: "h-bare", Role: "worker", JoinedAt: stamp(10)}
	if err := p.UpsertNode(ctx, bare); err != nil {
		t.Fatalf("UpsertNode bare: %v", err)
	}

	// Insert a node with full metadata populated.
	inspected := stamp(70)
	full := registry.Node{
		ClusterName:     "alpha",
		Hostname:        "h-full",
		Role:            "control",
		JoinedAt:        stamp(11),
		Arch:            "arm64",
		OSVersion:       "Ubuntu 24.04",
		K3sVersion:      "v1.30.2+k3s1",
		AgentVersion:    "clusterbox v0.4.1",
		LastInspectedAt: inspected,
	}
	if err := p.UpsertNode(ctx, full); err != nil {
		t.Fatalf("UpsertNode full: %v", err)
	}

	nodes, err := p.ListNodes(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(nodes))
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Hostname < nodes[j].Hostname })

	gotBare, gotFull := nodes[0], nodes[1]

	// Bare node: every metadata field is the zero value.
	if gotBare.Arch != "" || gotBare.OSVersion != "" || gotBare.K3sVersion != "" || gotBare.AgentVersion != "" {
		t.Errorf("bare node metadata strings should be empty: %+v", gotBare)
	}
	if !gotBare.LastInspectedAt.IsZero() {
		t.Errorf("bare node LastInspectedAt should be zero, got %v", gotBare.LastInspectedAt)
	}

	// Full node: every field round-trips, time is UTC.
	if gotFull.Arch != "arm64" {
		t.Errorf("Arch: got %q", gotFull.Arch)
	}
	if gotFull.OSVersion != "Ubuntu 24.04" {
		t.Errorf("OSVersion: got %q", gotFull.OSVersion)
	}
	if gotFull.K3sVersion != "v1.30.2+k3s1" {
		t.Errorf("K3sVersion: got %q", gotFull.K3sVersion)
	}
	if gotFull.AgentVersion != "clusterbox v0.4.1" {
		t.Errorf("AgentVersion: got %q", gotFull.AgentVersion)
	}
	if !gotFull.LastInspectedAt.Equal(inspected) {
		t.Errorf("LastInspectedAt: want %v, got %v", inspected, gotFull.LastInspectedAt)
	}
	if gotFull.LastInspectedAt.Location() != time.UTC {
		t.Errorf("LastInspectedAt should be UTC, got %v", gotFull.LastInspectedAt.Location())
	}

	// Re-upsert the bare node with metadata: ON CONFLICT must overwrite.
	bare.Arch = "amd64"
	bare.OSVersion = "Ubuntu 22.04"
	bare.K3sVersion = "v1.29.5+k3s1"
	bare.AgentVersion = "clusterbox v0.3.0"
	bare.LastInspectedAt = stamp(80)
	if err := p.UpsertNode(ctx, bare); err != nil {
		t.Fatalf("UpsertNode bare update: %v", err)
	}

	// And re-upsert the full node clearing the metadata: ON CONFLICT must
	// also overwrite back to NULL when the caller passes zero values.
	full.Arch = ""
	full.OSVersion = ""
	full.K3sVersion = ""
	full.AgentVersion = ""
	full.LastInspectedAt = time.Time{}
	if err := p.UpsertNode(ctx, full); err != nil {
		t.Fatalf("UpsertNode full clear: %v", err)
	}

	nodes, err = p.ListNodes(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListNodes after update: %v", err)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Hostname < nodes[j].Hostname })
	gotBare, gotFull = nodes[0], nodes[1]

	if gotBare.Arch != "amd64" || gotBare.OSVersion != "Ubuntu 22.04" ||
		gotBare.K3sVersion != "v1.29.5+k3s1" || gotBare.AgentVersion != "clusterbox v0.3.0" {
		t.Errorf("bare node metadata after update: %+v", gotBare)
	}
	if !gotBare.LastInspectedAt.Equal(stamp(80)) {
		t.Errorf("bare LastInspectedAt after update: got %v", gotBare.LastInspectedAt)
	}

	if gotFull.Arch != "" || gotFull.OSVersion != "" || gotFull.K3sVersion != "" || gotFull.AgentVersion != "" {
		t.Errorf("full node metadata should be cleared on re-upsert: %+v", gotFull)
	}
	if !gotFull.LastInspectedAt.IsZero() {
		t.Errorf("full LastInspectedAt should be cleared, got %v", gotFull.LastInspectedAt)
	}
}

// TestDeleteDeployment_RemovesRow_HistoryUnaffected verifies the addon
// uninstall pattern: DeleteDeployment removes the (cluster_id, service)
// row and leaves deployment_history rows in place for audit, including a
// preceding StatusUninstalled history entry.
func TestDeleteDeployment_RemovesRow_HistoryUnaffected(t *testing.T) {
	p := newTempProvider(t)
	ctx := context.Background()

	if err := p.UpsertCluster(ctx, registry.Cluster{Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(1)}); err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}

	d := registry.Deployment{
		ClusterName: "alpha",
		Service:     "ingress-nginx",
		Version:     "v1.0.0",
		DeployedAt:  stamp(20),
		DeployedBy:  "alice",
		Status:      registry.StatusRolledOut,
		Kind:        registry.KindAddon,
	}
	if err := p.UpsertDeployment(ctx, d); err != nil {
		t.Fatalf("UpsertDeployment: %v", err)
	}

	// Append a rolled_out history row to start.
	if err := p.AppendHistory(ctx, registry.DeploymentHistoryEntry{
		ClusterName: "alpha", Service: "ingress-nginx", Version: "v1.0.0",
		AttemptedAt: stamp(20), Status: registry.StatusRolledOut,
		RolloutDurationMs: 1234, Kind: registry.KindAddon,
	}); err != nil {
		t.Fatalf("AppendHistory rolled_out: %v", err)
	}

	// Append the uninstalled history row.
	if err := p.AppendHistory(ctx, registry.DeploymentHistoryEntry{
		ClusterName: "alpha", Service: "ingress-nginx", Version: "v1.0.0",
		AttemptedAt: stamp(30), Status: registry.StatusUninstalled,
		RolloutDurationMs: 200, Kind: registry.KindAddon,
	}); err != nil {
		t.Fatalf("AppendHistory uninstalled: %v", err)
	}

	// Now delete the deployments row.
	if err := p.DeleteDeployment(ctx, "alpha", "ingress-nginx"); err != nil {
		t.Fatalf("DeleteDeployment: %v", err)
	}
	if _, err := p.GetDeployment(ctx, "alpha", "ingress-nginx"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetDeployment after Delete: want ErrNotFound, got %v", err)
	}

	// History rows must remain.
	hist, err := p.ListHistory(ctx, registry.HistoryFilter{ClusterName: "alpha", Service: "ingress-nginx"})
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("history rows should be preserved: want 2, got %d", len(hist))
	}
	// ListHistory returns most-recent-first; verify the StatusUninstalled
	// row round-tripped correctly.
	if hist[0].Status != registry.StatusUninstalled {
		t.Errorf("most recent history Status: want %q, got %q",
			registry.StatusUninstalled, hist[0].Status)
	}

	// Idempotency: delete a non-existent row.
	if err := p.DeleteDeployment(ctx, "alpha", "ingress-nginx"); err != nil {
		t.Errorf("DeleteDeployment on missing row should be a no-op, got %v", err)
	}
	if err := p.DeleteDeployment(ctx, "alpha", "never-existed"); err != nil {
		t.Errorf("DeleteDeployment on never-existed row should be a no-op, got %v", err)
	}
}

// TestCluster_SurrogateID verifies the surrogate integer primary key
// behaviour: destroying a cluster and creating a new one with the same name
// produces a distinct ID, both rows exist in the underlying store
// (one destroyed, one active), and the IDs are different.
func TestCluster_SurrogateID(t *testing.T) {
	p := newTempProvider(t)
	ctx := context.Background()

	// Insert the first "alpha" cluster.
	if err := p.UpsertCluster(ctx, registry.Cluster{
		Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(1),
	}); err != nil {
		t.Fatalf("UpsertCluster first alpha: %v", err)
	}
	first, err := p.GetCluster(ctx, "alpha")
	if err != nil {
		t.Fatalf("GetCluster first: %v", err)
	}
	if first.ID == 0 {
		t.Fatal("first cluster ID should be non-zero")
	}

	// Destroy it.
	if err := p.MarkClusterDestroyed(ctx, "alpha", stamp(60)); err != nil {
		t.Fatalf("MarkClusterDestroyed: %v", err)
	}

	// GetCluster now returns ErrNotFound (no active alpha).
	_, err = p.GetCluster(ctx, "alpha")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("after destroy, want ErrNotFound, got %v", err)
	}

	// Insert a new "alpha" cluster — this is a new lifetime and should succeed.
	if err := p.UpsertCluster(ctx, registry.Cluster{
		Name: "alpha", Provider: "hcloud", Region: "nbg1", Env: "prod", CreatedAt: stamp(2),
	}); err != nil {
		t.Fatalf("UpsertCluster second alpha: %v", err)
	}
	second, err := p.GetCluster(ctx, "alpha")
	if err != nil {
		t.Fatalf("GetCluster second: %v", err)
	}
	if second.ID == 0 {
		t.Fatal("second cluster ID should be non-zero")
	}
	if second.ID == first.ID {
		t.Fatalf("second cluster must have a distinct ID: both are %d", first.ID)
	}
	if !second.DestroyedAt.IsZero() {
		t.Fatalf("second cluster should be active, got DestroyedAt=%v", second.DestroyedAt)
	}

	// ListClusters should only show the active (second) cluster.
	list, err := p.ListClusters(ctx)
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 active cluster, got %d", len(list))
	}
	if list[0].ID != second.ID {
		t.Fatalf("ListClusters returned wrong cluster: got ID=%d, want %d", list[0].ID, second.ID)
	}
}
