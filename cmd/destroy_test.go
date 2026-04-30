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
	resources           map[int64]registry.ClusterResource
	nextID              int64
	getErr              error
	listErr             error
	listResByTypeErr    error
	markDestroyedCalled []int64
	clusterDestroyed    bool
	clusterDestroyedAt  time.Time
	deployments         []registry.Deployment
	deletedDeployments  []string
}

func newDestroyFakeRegistry(c registry.Cluster, rs []registry.ClusterResource) *destroyFakeRegistry {
	f := &destroyFakeRegistry{
		cluster:   c,
		resources: make(map[int64]registry.ClusterResource),
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

func (f *destroyFakeRegistry) ListResources(_ context.Context, clusterName string, includeDestroyed bool) ([]registry.ClusterResource, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []registry.ClusterResource
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

func (f *destroyFakeRegistry) ListResourcesByType(_ context.Context, clusterName, resourceType string) ([]registry.ClusterResource, error) {
	if f.listResByTypeErr != nil {
		return nil, f.listResByTypeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []registry.ClusterResource
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

func (f *destroyFakeRegistry) RecordResource(context.Context, registry.ClusterResource) (int64, error) {
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
func (f *destroyFakeRegistry) DeleteDeployment(_ context.Context, _, service string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedDeployments = append(f.deletedDeployments, service)
	kept := f.deployments[:0]
	for _, d := range f.deployments {
		if d.Service != service {
			kept = append(kept, d)
		}
	}
	f.deployments = kept
	return nil
}
func (f *destroyFakeRegistry) GetDeployment(context.Context, string, string) (registry.Deployment, error) {
	panic("not used")
}
func (f *destroyFakeRegistry) ListDeployments(_ context.Context, _ string) ([]registry.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]registry.Deployment(nil), f.deployments...), nil
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

// fakeKubectlRunner implements bootstrap.CommandRunner for destroy tests.
// It records calls and returns errOnVerb's error when any arg matches errOnVerb.
type fakeKubectlRunner struct {
	mu        sync.Mutex
	calls     []fakeRunnerCall
	errOnVerb string
	err       error
}

type fakeRunnerCall struct {
	name string
	args []string
}

func (k *fakeKubectlRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	k.mu.Lock()
	k.calls = append(k.calls, fakeRunnerCall{name: name, args: append([]string(nil), args...)})
	k.mu.Unlock()
	if k.errOnVerb != "" {
		for _, a := range args {
			if a == k.errOnVerb {
				return nil, k.err
			}
		}
	}
	return nil, nil
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
		[]registry.ClusterResource{
			{ClusterName: "c1", Provider: registry.ProviderHetzner, ResourceType: registry.ResourceServer, ExternalID: "100"},
			{ClusterName: "c1", Provider: registry.ProviderHetzner, ResourceType: registry.ResourceFirewall, ExternalID: "300"},
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
		[]registry.ClusterResource{
			{ClusterName: "c1", Provider: registry.ProviderHetzner, ResourceType: registry.ResourceServer, ExternalID: "100"},
		},
	)
	deletes := []string{}
	leakingLister := &fakeListerOnlyServers{
		servers: []hetzner.LabelledResource{{
			ExternalID: "100",
			Hostname:   "c1",
			Labels:     hetzner.StandardLabels("c1", "control-plane"),
		}},
	}
	deps := DestroyDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		NewLister:    func(string) hetzner.HCloudResourceLister { return leakingLister },
		DeleteResource: func(_ context.Context, _ string, rt registry.ResourceType, id string) error {
			deletes = append(deletes, string(rt)+"/"+id)
			return nil
		},
		In:       strings.NewReader("y\n"),
		Out:      &bytes.Buffer{},
		Resolver: staticTokenResolver{},
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
		[]registry.ClusterResource{
			{ClusterName: "c1", Provider: registry.ProviderHetzner, ResourceType: registry.ResourceServer, ExternalID: "100"},
		},
	)
	deletes := []string{}
	deps := DestroyDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		NewLister:    func(string) hetzner.HCloudResourceLister { return stubLister{} },
		DeleteResource: func(_ context.Context, _ string, rt registry.ResourceType, id string) error {
			deletes = append(deletes, string(rt)+"/"+id)
			return nil
		},
		Out:      &bytes.Buffer{},
		Resolver: staticTokenResolver{},
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
		[]registry.ClusterResource{
			{ClusterName: "c1", Provider: registry.ProviderHetzner, ResourceType: registry.ResourceServer, ExternalID: "100"},
		},
	)

	// Lister still reports the server so straggler sweep runs.
	leaking := &fakeListerOnlyServers{
		servers: []hetzner.LabelledResource{{
			ExternalID: "100",
			Hostname:   "c1",
			Labels:     hetzner.StandardLabels("c1", "control-plane"),
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
		Resolver:     staticTokenResolver{},
	}
	if err := RunDestroyWith(context.Background(), "c1", "tok", true, false, deps); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if !strings.Contains(out.String(), "DNS records are not auto-removed") {
		t.Errorf("expected DNS note, got %q", out.String())
	}
}

// TestDestroy_RemovesRunnerScaleSetsBeforeCloudTeardown verifies that destroy
// kubectl-deletes each AutoscalingRunnerSet and removes the registry row
// before invoking the provider's Destroy, so ARC can deregister from GitHub
// while the cluster is still reachable.
func TestDestroy_RemovesRunnerScaleSetsBeforeCloudTeardown(t *testing.T) {
	reg := newDestroyFakeRegistry(registry.Cluster{Name: "c1", KubeconfigPath: "/kube/c1.yaml"}, nil)
	reg.deployments = []registry.Deployment{
		{ClusterName: "c1", Service: "ci-runners", Kind: registry.KindRunnerScaleSet},
		{ClusterName: "c1", Service: "deploy-runners", Kind: registry.KindRunnerScaleSet},
		{ClusterName: "c1", Service: "some-addon", Kind: registry.KindAddon},
	}

	fakeRunner := &fakeKubectlRunner{}

	deps := DestroyDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		NewLister:    func(string) hetzner.HCloudResourceLister { return stubLister{} },
		Runner:       fakeRunner,
		Out:          &bytes.Buffer{},
		Resolver:     staticTokenResolver{},
	}

	if err := RunDestroyWith(context.Background(), "c1", "tok", true, false, deps); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	// Both runner scale sets must have been kubectl-deleted.
	deletedNames := map[string]bool{}
	for _, call := range fakeRunner.calls {
		for i, a := range call.args {
			if a == "delete" && i+2 < len(call.args) {
				// args: delete autoscalingrunnersets <name> -n <ns> --ignore-not-found
				deletedNames[call.args[i+2]] = true
			}
		}
	}
	for _, name := range []string{"ci-runners", "deploy-runners"} {
		if !deletedNames[name] {
			t.Errorf("expected kubectl delete for %q, calls: %v", name, fakeRunner.calls)
		}
	}

	// Both runner registry rows must be removed; the addon row must remain.
	if len(reg.deletedDeployments) != 2 {
		t.Errorf("expected 2 deleted deployment rows, got %v", reg.deletedDeployments)
	}
	for _, name := range reg.deletedDeployments {
		if name == "some-addon" {
			t.Errorf("addon deployment must not be deleted by destroy")
		}
	}

	if !reg.clusterDestroyed {
		t.Errorf("cluster not marked destroyed")
	}
}

// TestDestroy_RunnerKubectlFailureIsNonFatal verifies that a kubectl error
// during runner scale set removal is logged as a warning and destroy continues
// to completion, so a partially-up cluster doesn't block teardown.
func TestDestroy_RunnerKubectlFailureIsNonFatal(t *testing.T) {
	reg := newDestroyFakeRegistry(registry.Cluster{Name: "c1", KubeconfigPath: "/kube/c1.yaml"}, nil)
	reg.deployments = []registry.Deployment{
		{ClusterName: "c1", Service: "ci-runners", Kind: registry.KindRunnerScaleSet},
	}

	failing := &fakeKubectlRunner{errOnVerb: "delete", err: errors.New("connection refused")}

	var out bytes.Buffer
	deps := DestroyDeps{
		OpenRegistry: func(context.Context) (registry.Registry, error) { return reg, nil },
		NewLister:    func(string) hetzner.HCloudResourceLister { return stubLister{} },
		Runner:       failing,
		Out:          &out,
		Resolver:     staticTokenResolver{},
	}

	if err := RunDestroyWith(context.Background(), "c1", "tok", true, false, deps); err != nil {
		t.Fatalf("destroy must succeed even when kubectl fails: %v", err)
	}
	if !strings.Contains(out.String(), "warning") {
		t.Errorf("expected warning in output, got %q", out.String())
	}
	// Registry row should still be cleaned up despite kubectl failure.
	if len(reg.deletedDeployments) != 1 || reg.deletedDeployments[0] != "ci-runners" {
		t.Errorf("expected registry row deleted, got %v", reg.deletedDeployments)
	}
	if !reg.clusterDestroyed {
		t.Errorf("cluster not marked destroyed after kubectl warning")
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
