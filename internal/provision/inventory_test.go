package provision_test

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/registry"
)

// fakeLister is an in-memory HCloudResourceLister keyed by resource type.
// Tests load it with LabelledResource slices; the reconciler reads them
// without making any network calls.
type fakeLister struct {
	servers       []provision.LabelledResource
	loadBalancers []provision.LabelledResource
	sshKeys       []provision.LabelledResource
	firewalls     []provision.LabelledResource
	networks      []provision.LabelledResource
	volumes       []provision.LabelledResource
	primaryIPs    []provision.LabelledResource
	err           error
}

func (f *fakeLister) ListServers(context.Context, string) ([]provision.LabelledResource, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.servers, nil
}
func (f *fakeLister) ListLoadBalancers(context.Context, string) ([]provision.LabelledResource, error) {
	return f.loadBalancers, nil
}
func (f *fakeLister) ListSSHKeys(context.Context, string) ([]provision.LabelledResource, error) {
	return f.sshKeys, nil
}
func (f *fakeLister) ListFirewalls(context.Context, string) ([]provision.LabelledResource, error) {
	return f.firewalls, nil
}
func (f *fakeLister) ListNetworks(context.Context, string) ([]provision.LabelledResource, error) {
	return f.networks, nil
}
func (f *fakeLister) ListVolumes(context.Context, string) ([]provision.LabelledResource, error) {
	return f.volumes, nil
}
func (f *fakeLister) ListPrimaryIPs(context.Context, string) ([]provision.LabelledResource, error) {
	return f.primaryIPs, nil
}

// fakeRegistry is a minimal in-memory registry keyed by row id. It
// implements only the methods Reconcile actually calls; everything else
// panics so accidental reliance shows up immediately.
type fakeRegistry struct {
	mu        sync.Mutex
	nextID    int64
	resources map[int64]registry.HetznerResource
	recordErr error
	listErr   error
	markErr   error
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{resources: make(map[int64]registry.HetznerResource)}
}

func (f *fakeRegistry) RecordResource(_ context.Context, r registry.HetznerResource) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return 0, f.recordErr
	}
	f.nextID++
	r.ID = f.nextID
	f.resources[r.ID] = r
	return r.ID, nil
}

func (f *fakeRegistry) MarkResourceDestroyed(_ context.Context, id int64, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return f.markErr
	}
	row, ok := f.resources[id]
	if !ok {
		return nil
	}
	if !row.DestroyedAt.IsZero() {
		return nil
	}
	row.DestroyedAt = at
	f.resources[id] = row
	return nil
}

func (f *fakeRegistry) ListResourcesByType(_ context.Context, clusterName, resourceType string) ([]registry.HetznerResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []registry.HetznerResource
	for _, r := range f.resources {
		if r.ClusterName != clusterName || string(r.ResourceType) != resourceType {
			continue
		}
		if !r.DestroyedAt.IsZero() {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeRegistry) ListResources(_ context.Context, clusterName string, includeDestroyed bool) ([]registry.HetznerResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []registry.HetznerResource
	for _, r := range f.resources {
		if r.ClusterName != clusterName {
			continue
		}
		if !includeDestroyed && !r.DestroyedAt.IsZero() {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Unused interface methods.
func (f *fakeRegistry) UpsertCluster(context.Context, registry.Cluster) error { panic("not used") }
func (f *fakeRegistry) GetCluster(context.Context, string) (registry.Cluster, error) {
	panic("not used")
}
func (f *fakeRegistry) ListClusters(context.Context) ([]registry.Cluster, error) {
	panic("not used")
}
func (f *fakeRegistry) DeleteCluster(context.Context, string) error     { panic("not used") }
func (f *fakeRegistry) UpsertNode(context.Context, registry.Node) error { panic("not used") }
func (f *fakeRegistry) RemoveNode(context.Context, string, string) error {
	panic("not used")
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
func (f *fakeRegistry) AppendHistory(context.Context, registry.DeploymentHistoryEntry) error {
	panic("not used")
}
func (f *fakeRegistry) ListHistory(context.Context, registry.HistoryFilter) ([]registry.DeploymentHistoryEntry, error) {
	panic("not used")
}
func (f *fakeRegistry) MarkSynced(context.Context, string, time.Time) error {
	panic("not used")
}
func (f *fakeRegistry) MarkClusterDestroyed(context.Context, string, time.Time) error {
	panic("not used")
}
func (f *fakeRegistry) Close() error { return nil }

// labelled is a small helper that builds a LabelledResource carrying the
// canonical clusterbox labels for the given cluster.
func labelled(id, name, clusterName string) provision.LabelledResource {
	return provision.LabelledResource{
		HetznerID: id,
		Hostname:  name,
		Labels:    provision.StandardLabels(clusterName, "control-plane"),
	}
}

// TestStandardLabels_ContainsRequiredKeys is a quick sanity check that
// the constant set drives the same keys the reconciler validates.
func TestStandardLabels_ContainsRequiredKeys(t *testing.T) {
	got := provision.StandardLabels("c1", "control-plane")
	if got[provision.LabelManagedBy] != provision.ManagedByValue {
		t.Errorf("missing managed-by label: %v", got)
	}
	if got[provision.LabelClusterName] != "c1" {
		t.Errorf("missing cluster-name label: %v", got)
	}
	if got[provision.LabelResourceRole] != "control-plane" {
		t.Errorf("missing resource-role label: %v", got)
	}
}

// TestStandardLabels_OmitsEmptyRole verifies that callers that haven't
// classified a resource don't pin it to an empty string role.
func TestStandardLabels_OmitsEmptyRole(t *testing.T) {
	got := provision.StandardLabels("c1", "")
	if _, ok := got[provision.LabelResourceRole]; ok {
		t.Errorf("expected no resource-role key, got %v", got)
	}
}

// TestLabelSelector_FormatMatchesHetznerSyntax is a trivial format
// pin: a regression test against accidental key reordering.
func TestLabelSelector_FormatMatchesHetznerSyntax(t *testing.T) {
	got := provision.LabelSelector("c1")
	want := "managed-by=clusterbox,cluster-name=c1"
	if got != want {
		t.Errorf("LabelSelector: got %q, want %q", got, want)
	}
}

// TestReconcile_AddsMissingRows verifies brand-new cloud resources land
// in the registry on first reconcile.
func TestReconcile_AddsMissingRows(t *testing.T) {
	reg := newFakeRegistry()
	lister := &fakeLister{
		servers:    []provision.LabelledResource{labelled("100", "c1", "c1")},
		volumes:    []provision.LabelledResource{labelled("200", "c1-data", "c1")},
		firewalls:  []provision.LabelledResource{labelled("300", "c1-fw", "c1")},
		sshKeys:    []provision.LabelledResource{labelled("400", "c1-ssh", "c1")},
		primaryIPs: []provision.LabelledResource{labelled("500", "c1-ip", "c1")},
	}
	r := &provision.Reconciler{Registry: reg, Lister: lister}

	summary, err := r.Reconcile(context.Background(), "c1")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if summary.Added != 5 {
		t.Errorf("Added: got %d, want 5", summary.Added)
	}
	if summary.Existing != 0 {
		t.Errorf("Existing: got %d, want 0", summary.Existing)
	}
	if summary.MarkedDestroyed != 0 {
		t.Errorf("MarkedDestroyed: got %d, want 0", summary.MarkedDestroyed)
	}
	if len(summary.Unmanaged) != 0 {
		t.Errorf("Unmanaged: got %v, want []", summary.Unmanaged)
	}

	rows, _ := reg.ListResources(context.Background(), "c1", false)
	if len(rows) != 5 {
		t.Errorf("expected 5 registry rows, got %d", len(rows))
	}
}

// TestReconcile_Idempotent verifies a second pass is a no-op.
func TestReconcile_Idempotent(t *testing.T) {
	reg := newFakeRegistry()
	lister := &fakeLister{
		servers: []provision.LabelledResource{labelled("100", "c1", "c1")},
	}
	r := &provision.Reconciler{Registry: reg, Lister: lister}

	if _, err := r.Reconcile(context.Background(), "c1"); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	summary, err := r.Reconcile(context.Background(), "c1")
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	if summary.Added != 0 {
		t.Errorf("Added: got %d, want 0 on second pass", summary.Added)
	}
	if summary.Existing != 1 {
		t.Errorf("Existing: got %d, want 1", summary.Existing)
	}
	if summary.MarkedDestroyed != 0 {
		t.Errorf("MarkedDestroyed: got %d, want 0", summary.MarkedDestroyed)
	}
}

// TestReconcile_TombstonesDisappearedRows verifies rows that were active
// in the registry but no longer exist in the cloud are tombstoned.
func TestReconcile_TombstonesDisappearedRows(t *testing.T) {
	reg := newFakeRegistry()
	lister := &fakeLister{
		servers: []provision.LabelledResource{labelled("100", "c1", "c1")},
	}
	r := &provision.Reconciler{Registry: reg, Lister: lister}

	if _, err := r.Reconcile(context.Background(), "c1"); err != nil {
		t.Fatalf("seed reconcile: %v", err)
	}

	// Cloud-side resource disappears.
	lister.servers = nil

	summary, err := r.Reconcile(context.Background(), "c1")
	if err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}
	if summary.MarkedDestroyed != 1 {
		t.Errorf("MarkedDestroyed: got %d, want 1", summary.MarkedDestroyed)
	}

	rows, _ := reg.ListResources(context.Background(), "c1", true)
	if len(rows) != 1 || rows[0].DestroyedAt.IsZero() {
		t.Errorf("expected one tombstoned row, got %+v", rows)
	}

	// A third reconcile must not double-tombstone.
	summary, err = r.Reconcile(context.Background(), "c1")
	if err != nil {
		t.Fatalf("reconcile second pass: %v", err)
	}
	if summary.MarkedDestroyed != 0 {
		t.Errorf("MarkedDestroyed second pass: got %d, want 0", summary.MarkedDestroyed)
	}
}

// TestReconcile_FlagsUnmanagedResources verifies a labelled resource that
// fails the strict label check (e.g. cluster-name mismatch) is surfaced
// in Summary.Unmanaged and never recorded.
func TestReconcile_FlagsUnmanagedResources(t *testing.T) {
	reg := newFakeRegistry()
	bad := provision.LabelledResource{
		HetznerID: "999",
		Hostname:  "stray",
		Labels:    map[string]string{"managed-by": "clusterbox", "cluster-name": "wrong-cluster"},
	}
	lister := &fakeLister{servers: []provision.LabelledResource{bad}}
	r := &provision.Reconciler{Registry: reg, Lister: lister}

	summary, err := r.Reconcile(context.Background(), "c1")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if summary.Added != 0 {
		t.Errorf("Added: got %d, want 0", summary.Added)
	}
	if len(summary.Unmanaged) != 1 || summary.Unmanaged[0] != "999" {
		t.Errorf("Unmanaged: got %v, want [999]", summary.Unmanaged)
	}
}

// TestReconcile_RequiresRegistryAndLister verifies misuse fails fast
// rather than panicking.
func TestReconcile_RequiresRegistryAndLister(t *testing.T) {
	r := &provision.Reconciler{}
	if _, err := r.Reconcile(context.Background(), "c1"); err == nil {
		t.Fatal("expected error when registry is nil")
	}

	r = &provision.Reconciler{Registry: newFakeRegistry()}
	if _, err := r.Reconcile(context.Background(), "c1"); err == nil {
		t.Fatal("expected error when lister is nil")
	}

	r = &provision.Reconciler{Registry: newFakeRegistry(), Lister: &fakeLister{}}
	if _, err := r.Reconcile(context.Background(), ""); err == nil {
		t.Fatal("expected error when clusterName is empty")
	}
}

// TestReconcile_ListErrorPropagates verifies a lister error is returned
// rather than silently dropped.
func TestReconcile_ListErrorPropagates(t *testing.T) {
	lister := &fakeLister{err: errors.New("hetzner API down")}
	r := &provision.Reconciler{Registry: newFakeRegistry(), Lister: lister}
	_, err := r.Reconcile(context.Background(), "c1")
	if err == nil || !errContains(err, "hetzner API down") {
		t.Errorf("expected wrapped lister error, got %v", err)
	}
}

func errContains(err error, substr string) bool {
	if err == nil {
		return false
	}
	return contains(err.Error(), substr)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
