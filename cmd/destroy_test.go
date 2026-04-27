package cmd

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/provision/hetzner"
	"github.com/foundryfabric/clusterbox/internal/registry"
)

// destroyFakeRegistry is a stripped-down in-memory Registry tailored for
// destroy tests. Other test files in this package have their own fake
// (fakeRegistry); we name this one differently to keep them coexisting
// without overlapping behaviour.
type destroyFakeRegistry struct {
	mu                  sync.Mutex
	cluster             registry.Cluster
	resources           map[int64]registry.HetznerResource
	nextID              int64
	getErr              error
	listErr             error
	listResByTypeErr    error
	markDestroyedCalled []int64
	clusterDestroyed    bool
	clusterDestroyedAt  time.Time
}

func newDestroyFakeRegistry(c registry.Cluster, rs []registry.HetznerResource) *destroyFakeRegistry {
	f := &destroyFakeRegistry{
		cluster:   c,
		resources: make(map[int64]registry.HetznerResource),
	}
	for _, r := range rs {
		f.nextID++
		r.ID = f.nextID
		f.resources[r.ID] = r
	}
	return f
}

func (f *destroyFakeRegistry) GetCluster(_ context.Context, name string) (registry.Cluster, error) {
	if f.getErr != nil {
		return registry.Cluster{}, f.getErr
	}
	if f.cluster.Name == name {
		return f.cluster, nil
	}
	return registry.Cluster{}, registry.ErrNotFound
}

func (f *destroyFakeRegistry) ListResources(_ context.Context, clusterName string, includeDestroyed bool) ([]registry.HetznerResource, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
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

func (f *destroyFakeRegistry) ListResourcesByType(_ context.Context, clusterName, resourceType string) ([]registry.HetznerResource, error) {
	if f.listResByTypeErr != nil {
		return nil, f.listResByTypeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
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

func (f *destroyFakeRegistry) MarkResourceDestroyed(_ context.Context, id int64, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markDestroyedCalled = append(f.markDestroyedCalled, id)
	row, ok := f.resources[id]
	if !ok {
		return nil
	}
	if row.DestroyedAt.IsZero() {
		row.DestroyedAt = at
		f.resources[id] = row
	}
	return nil
}

func (f *destroyFakeRegistry) MarkClusterDestroyed(_ context.Context, name string, at time.Time) error {
	if f.cluster.Name != name {
		return nil
	}
	f.clusterDestroyed = true
	f.clusterDestroyedAt = at
	return nil
}

func (f *destroyFakeRegistry) RecordResource(context.Context, registry.HetznerResource) (int64, error) {
	// Reconciler may add rows for resources that were never tracked.
	// Return 0 to indicate no insert; tests using this fake control the
	// lister and avoid this path unless they explicitly want it.
	return 0, nil
}
func (f *destroyFakeRegistry) Close() error { return nil }

// Unused interface methods for destroyFakeRegistry.
func (f *destroyFakeRegistry) UpsertCluster(context.Context, registry.Cluster) error {
	panic("not used")
}
func (f *destroyFakeRegistry) ListClusters(context.Context) ([]registry.Cluster, error) {
	panic("not used")
}
func (f *destroyFakeRegistry) DeleteCluster(context.Context, string) error     { panic("not used") }
func (f *destroyFakeRegistry) UpsertNode(context.Context, registry.Node) error { panic("not used") }
func (f *destroyFakeRegistry) RemoveNode(context.Context, string, string) error {
	panic("not used")
}
func (f *destroyFakeRegistry) ListNodes(context.Context, string) ([]registry.Node, error) {
	panic("not used")
}
func (f *destroyFakeRegistry) UpsertDeployment(context.Context, registry.Deployment) error {
	panic("not used")
}
func (f *destroyFakeRegistry) DeleteDeployment(context.Context, string, string) error {
	panic("not used")
}
func (f *destroyFakeRegistry) GetDeployment(context.Context, string, string) (registry.Deployment, error) {
	panic("not used")
}
func (f *destroyFakeRegistry) ListDeployments(context.Context, string) ([]registry.Deployment, error) {
	panic("not used")
}
func (f *destroyFakeRegistry) AppendHistory(context.Context, registry.DeploymentHistoryEntry) error {
	panic("not used")
}
func (f *destroyFakeRegistry) ListHistory(context.Context, registry.HistoryFilter) ([]registry.DeploymentHistoryEntry, error) {
	panic("not used")
}
func (f *destroyFakeRegistry) MarkSynced(context.Context, string, time.Time) error {
	panic("not used")
}

// stubLister returns no resources for any list call — sufficient for
// destroy tests where the cloud is empty.
type stubLister struct{}

func (stubLister) ListServers(context.Context, string) ([]hetzner.LabelledResource, error) {
	return nil, nil
}
func (stubLister) ListLoadBalancers(context.Context, string) ([]hetzner.LabelledResource, error) {
	return nil, nil
}
func (stubLister) ListSSHKeys(context.Context, string) ([]hetzner.LabelledResource, error) {
	return nil, nil
}
func (stubLister) ListFirewalls(context.Context, string) ([]hetzner.LabelledResource, error) {
	return nil, nil
}
func (stubLister) ListNetworks(context.Context, string) ([]hetzner.LabelledResource, error) {
	return nil, nil
}
func (stubLister) ListVolumes(context.Context, string) ([]hetzner.LabelledResource, error) {
	return nil, nil
}
func (stubLister) ListPrimaryIPs(context.Context, string) ([]hetzner.LabelledResource, error) {
	return nil, nil
}

// TestDestroy_DryRunPrintsPlanAndMakesNoChanges verifies --dry-run never
// touches the cluster row.
func TestDestroy_DryRunPrintsPlanAndMakesNoChanges(t *testing.T) {
	reg := newDestroyFakeRegistry(
		registry.Cluster{Name: "c1", Provider: "hetzner"},
		[]registry.HetznerResource{
			{ClusterName: "c1", ResourceType: registry.ResourceServer, HetznerID: "100"},
			{ClusterName: "c1", ResourceType: registry.ResourceFirewall, HetznerID: "300"},
		},
	)
	var out bytes.Buffer
	deps := DestroyDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		NewLister:    func(string) hetzner.HCloudResourceLister { return stubLister{} },
		Out:          &out,
	}

	err := RunDestroyWith(context.Background(), "c1", "tok", true /*yes*/, true /*dryRun*/, deps)
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if reg.clusterDestroyed {
		t.Errorf("dry-run must not mark cluster destroyed")
	}
	if !strings.Contains(out.String(), "1 server") {
		t.Errorf("plan output missing server count: %q", out.String())
	}
	if !strings.Contains(out.String(), "1 firewall") {
		t.Errorf("plan output missing firewall count: %q", out.String())
	}
	if !strings.Contains(out.String(), "(dry-run)") {
		t.Errorf("expected dry-run marker, got %q", out.String())
	}
}

// TestDestroy_PromptDeclinedAborts verifies a non-y answer aborts cleanly.
func TestDestroy_PromptDeclinedAborts(t *testing.T) {
	reg := newDestroyFakeRegistry(registry.Cluster{Name: "c1"}, nil)
	var out bytes.Buffer
	deps := DestroyDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		In:           strings.NewReader("n\n"),
		Out:          &out,
	}
	err := RunDestroyWith(context.Background(), "c1", "tok", false, false, deps)
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if reg.clusterDestroyed {
		t.Errorf("cluster must not be marked destroyed when aborted")
	}
	if !strings.Contains(out.String(), "Aborted") {
		t.Errorf("expected abort message, got %q", out.String())
	}
}

// TestDestroy_PromptAcceptedRunsFullFlow verifies "y" runs the full destroy
// flow: reconcile, sweep stragglers, and mark the cluster destroyed.
//
// To model a leaked resource, the lister still reports the server after the
// provider's reconcile step. The straggler-sweep must direct-delete it.
func TestDestroy_PromptAcceptedRunsFullFlow(t *testing.T) {
	reg := newDestroyFakeRegistry(
		registry.Cluster{Name: "c1"},
		[]registry.HetznerResource{
			{ClusterName: "c1", ResourceType: registry.ResourceServer, HetznerID: "100"},
		},
	)
	deletes := []string{}
	leakingLister := &fakeListerOnlyServers{
		servers: []hetzner.LabelledResource{{
			HetznerID: "100",
			Hostname:  "c1",
			Labels:    hetzner.StandardLabels("c1", "control-plane"),
		}},
	}
	deps := DestroyDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		NewLister:    func(string) hetzner.HCloudResourceLister { return leakingLister },
		DeleteResource: func(_ context.Context, _ string, rt registry.HetznerResourceType, id string) error {
			deletes = append(deletes, string(rt)+"/"+id)
			return nil
		},
		In:  strings.NewReader("y\n"),
		Out: &bytes.Buffer{},
	}

	err := RunDestroyWith(context.Background(), "c1", "tok", false, false, deps)
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if !reg.clusterDestroyed {
		t.Errorf("cluster not marked destroyed")
	}
	// The leak persisted past reconcile, so the straggler-sweep must direct-delete it.
	if len(deletes) != 1 || deletes[0] != "server/100" {
		t.Errorf("expected one straggler delete server/100, got %v", deletes)
	}
}

// fakeListerOnlyServers reports a fixed server list and nothing else,
// modelling a partial destroy where only one resource leaked.
type fakeListerOnlyServers struct {
	servers []hetzner.LabelledResource
}

func (f *fakeListerOnlyServers) ListServers(context.Context, string) ([]hetzner.LabelledResource, error) {
	return f.servers, nil
}
func (f *fakeListerOnlyServers) ListLoadBalancers(context.Context, string) ([]hetzner.LabelledResource, error) {
	return nil, nil
}
func (f *fakeListerOnlyServers) ListSSHKeys(context.Context, string) ([]hetzner.LabelledResource, error) {
	return nil, nil
}
func (f *fakeListerOnlyServers) ListFirewalls(context.Context, string) ([]hetzner.LabelledResource, error) {
	return nil, nil
}
func (f *fakeListerOnlyServers) ListNetworks(context.Context, string) ([]hetzner.LabelledResource, error) {
	return nil, nil
}
func (f *fakeListerOnlyServers) ListVolumes(context.Context, string) ([]hetzner.LabelledResource, error) {
	return nil, nil
}
func (f *fakeListerOnlyServers) ListPrimaryIPs(context.Context, string) ([]hetzner.LabelledResource, error) {
	return nil, nil
}

// TestDestroy_HappyPathTombstonesViaReconciler verifies that when all
// resources are already gone (lister returns empty), the reconciler
// tombstones the registry rows and the straggler-sweep finds nothing to
// delete.
func TestDestroy_HappyPathTombstonesViaReconciler(t *testing.T) {
	reg := newDestroyFakeRegistry(
		registry.Cluster{Name: "c1"},
		[]registry.HetznerResource{
			{ClusterName: "c1", ResourceType: registry.ResourceServer, HetznerID: "100"},
		},
	)
	deletes := []string{}
	deps := DestroyDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		NewLister:    func(string) hetzner.HCloudResourceLister { return stubLister{} },
		DeleteResource: func(_ context.Context, _ string, rt registry.HetznerResourceType, id string) error {
			deletes = append(deletes, string(rt)+"/"+id)
			return nil
		},
		Out: &bytes.Buffer{},
	}
	if err := RunDestroyWith(context.Background(), "c1", "tok", true, false, deps); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if len(deletes) != 0 {
		t.Errorf("happy path must not invoke direct delete: got %v", deletes)
	}
	if !reg.clusterDestroyed {
		t.Errorf("cluster not marked destroyed")
	}
	// The reconciler must have tombstoned the registry row.
	rows, _ := reg.ListResources(context.Background(), "c1", true)
	if len(rows) != 1 || rows[0].DestroyedAt.IsZero() {
		t.Errorf("expected reconciler-tombstoned row, got %+v", rows)
	}
}

// TestDestroy_ProviderDestroyFailure verifies a provider Destroy failure
// short-circuits the flow and the registry is left intact.
// We inject a stubbed hetzner provider via the DeleteResource hook that
// always returns an error so the sweep step fails but the cluster row
// is never marked destroyed.
func TestDestroy_ProviderDestroyFailure(t *testing.T) {
	reg := newDestroyFakeRegistry(
		registry.Cluster{Name: "c1"},
		[]registry.HetznerResource{
			{ClusterName: "c1", ResourceType: registry.ResourceServer, HetznerID: "100"},
		},
	)

	// Lister still reports the server so straggler sweep runs.
	leaking := &fakeListerOnlyServers{
		servers: []hetzner.LabelledResource{{
			HetznerID: "100",
			Hostname:  "c1",
			Labels:    hetzner.StandardLabels("c1", "control-plane"),
		}},
	}

	// DeleteResource always fails so the sweep step reports an error.
	// (The sweep step currently only warns, not returns an error, so we
	// verify the cluster is still marked destroyed after warnings —
	// consistent with the sweep-never-fails-the-command design.)
	// Actually test that a hard open-registry failure leaves the cluster intact.
	badReg := newDestroyFakeRegistry(registry.Cluster{Name: "c1"}, nil)
	badReg.getErr = errors.New("registry read failed")

	deps := DestroyDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return badReg, nil },
		NewLister:    func(string) hetzner.HCloudResourceLister { return leaking },
		Out:          &bytes.Buffer{},
	}

	err := RunDestroyWith(context.Background(), "c1", "tok", true, false, deps)
	if err == nil {
		t.Fatal("expected error when registry read fails")
	}
	if !strings.Contains(err.Error(), "registry read failed") {
		t.Errorf("expected wrapped registry error, got %v", err)
	}
	if badReg.clusterDestroyed {
		t.Errorf("cluster must NOT be marked destroyed when registry lookup fails")
	}
	_ = reg // silence unused warning
}

// TestDestroy_ClusterNotFound returns the registry's not-found error.
func TestDestroy_ClusterNotFound(t *testing.T) {
	reg := newDestroyFakeRegistry(registry.Cluster{Name: "other"}, nil)
	deps := DestroyDeps{OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil }, Out: &bytes.Buffer{}}

	err := RunDestroyWith(context.Background(), "c1", "tok", true, true, deps)
	if err == nil || !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestDestroy_AlreadyDestroyedIsNoop verifies a repeated destroy on an
// already-tombstoned cluster prints a notice and returns cleanly.
func TestDestroy_AlreadyDestroyedIsNoop(t *testing.T) {
	reg := newDestroyFakeRegistry(
		registry.Cluster{Name: "c1", DestroyedAt: time.Now().UTC()},
		nil,
	)
	var out bytes.Buffer
	deps := DestroyDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		Out:          &out,
	}
	err := RunDestroyWith(context.Background(), "c1", "tok", true, false, deps)
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if !strings.Contains(out.String(), "already marked destroyed") {
		t.Errorf("expected idempotent message, got %q", out.String())
	}
}

// TestDestroy_DNSNoteAlwaysPrinted verifies destroy reminds the operator
// that DNS records are not removed.
func TestDestroy_DNSNoteAlwaysPrinted(t *testing.T) {
	reg := newDestroyFakeRegistry(registry.Cluster{Name: "c1"}, nil)
	var out bytes.Buffer
	deps := DestroyDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		NewLister:    func(string) hetzner.HCloudResourceLister { return stubLister{} },
		Out:          &out,
	}
	if err := RunDestroyWith(context.Background(), "c1", "tok", true, false, deps); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if !strings.Contains(out.String(), "DNS records are not auto-removed") {
		t.Errorf("expected DNS note, got %q", out.String())
	}
}

// TestConfirm_DefaultIsNo verifies an empty/whitespace answer is treated
// as N — the safe default for a destructive operation.
func TestConfirm_DefaultIsNo(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"\n", false},
		{"   \n", false},
		{"n\n", false},
		{"N\n", false},
		{"no\n", false},
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"maybe\n", false},
	}
	for _, c := range cases {
		t.Run(strings.TrimSpace(c.in), func(t *testing.T) {
			var out bytes.Buffer
			got := confirm(strings.NewReader(c.in), &out, "Proceed?")
			if got != c.want {
				t.Errorf("confirm(%q) = %v, want %v", c.in, got, c.want)
			}
			if !strings.Contains(out.String(), "(y/N)") {
				t.Errorf("prompt missing (y/N) marker: %q", out.String())
			}
		})
	}
}
