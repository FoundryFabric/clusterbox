package addon

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/registry"
)

// ---------------------------------------------------------------------------
// Test doubles.
// ---------------------------------------------------------------------------

// fakeResolver implements secrets.Resolver. resolved is the map every call
// returns; err short-circuits and is returned untouched.
type fakeResolver struct {
	resolved map[string]string
	err      error

	calls []resolveCall
}

type resolveCall struct {
	app, env, provider, region string
}

func (r *fakeResolver) Resolve(_ context.Context, app, env, provider, region string) (map[string]string, error) {
	r.calls = append(r.calls, resolveCall{app, env, provider, region})
	if r.err != nil {
		return nil, r.err
	}
	// Return a defensive copy so the installer cannot mutate the seed.
	out := make(map[string]string, len(r.resolved))
	for k, v := range r.resolved {
		out[k] = v
	}
	return out, nil
}

// recordedRun captures one invocation of fakeKubectl.
type recordedRun struct {
	name string
	args []string
}

// fakeKubectl implements secrets.CommandRunner. It records every call and
// optionally returns a canned error for invocations whose first arg matches
// errOnVerb (e.g. "apply"). nil errOnVerb means every call succeeds.
type fakeKubectl struct {
	mu sync.Mutex

	runs   []recordedRun
	errs   map[string]error
	output []byte
}

func newFakeKubectl() *fakeKubectl { return &fakeKubectl{errs: map[string]error{}} }

func (k *fakeKubectl) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	cp := append([]string(nil), args...)
	k.runs = append(k.runs, recordedRun{name: name, args: cp})
	verb := firstVerb(args)
	if err, ok := k.errs[verb]; ok {
		return nil, err
	}
	return k.output, nil
}

// firstVerb returns the first arg that is not a flag or a flag's value.
// kubectl args look like "--kubeconfig <path> apply -f <file>" so the verb
// is the third arg. We scan for the first non-flag-following token.
func firstVerb(args []string) string {
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(a, "--") {
			// Flags like --kubeconfig take a value as the next arg; flags
			// like --ignore-not-found do not. The conservative thing is to
			// assume a value when the flag has no '=' embedded.
			if !strings.Contains(a, "=") {
				skipNext = true
			}
			continue
		}
		return a
	}
	return ""
}

// fakeRegistry is an in-memory registry.Registry that records every call. We
// implement only the methods the installer touches; the rest panic so a
// test-induced regression shows up immediately rather than masquerading as a
// silent no-op.
type fakeRegistry struct {
	mu sync.Mutex

	clusters    map[string]registry.Cluster
	deployments map[string]registry.Deployment
	history     []registry.DeploymentHistoryEntry

	getClusterErr   error
	getDeploymentEr error
	listDeploysErr  error
	upsertErr       error
	appendErr       error
	deleteDepErr    error
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{
		clusters:    map[string]registry.Cluster{},
		deployments: map[string]registry.Deployment{},
	}
}

func depKey(cluster, service string) string { return cluster + "\x00" + service }

func (f *fakeRegistry) GetCluster(_ context.Context, name string) (registry.Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getClusterErr != nil {
		return registry.Cluster{}, f.getClusterErr
	}
	c, ok := f.clusters[name]
	if !ok {
		return registry.Cluster{}, registry.ErrNotFound
	}
	return c, nil
}

func (f *fakeRegistry) GetDeployment(_ context.Context, cluster, service string) (registry.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getDeploymentEr != nil {
		return registry.Deployment{}, f.getDeploymentEr
	}
	d, ok := f.deployments[depKey(cluster, service)]
	if !ok {
		return registry.Deployment{}, registry.ErrNotFound
	}
	return d, nil
}

func (f *fakeRegistry) ListDeployments(_ context.Context, cluster string) ([]registry.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listDeploysErr != nil {
		return nil, f.listDeploysErr
	}
	var out []registry.Deployment
	for _, d := range f.deployments {
		if d.ClusterName == cluster {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeRegistry) UpsertDeployment(_ context.Context, d registry.Deployment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.deployments[depKey(d.ClusterName, d.Service)] = d
	return nil
}

func (f *fakeRegistry) DeleteDeployment(_ context.Context, cluster, service string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteDepErr != nil {
		return f.deleteDepErr
	}
	delete(f.deployments, depKey(cluster, service))
	return nil
}

func (f *fakeRegistry) AppendHistory(_ context.Context, e registry.DeploymentHistoryEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendErr != nil {
		return f.appendErr
	}
	f.history = append(f.history, e)
	return nil
}

// Methods we don't exercise — these must panic so a future code path that
// accidentally relies on them surfaces immediately.
func (f *fakeRegistry) UpsertCluster(context.Context, registry.Cluster) error {
	panic("fakeRegistry.UpsertCluster: not used")
}
func (f *fakeRegistry) ListClusters(context.Context) ([]registry.Cluster, error) {
	panic("fakeRegistry.ListClusters: not used")
}
func (f *fakeRegistry) DeleteCluster(context.Context, string) error {
	panic("fakeRegistry.DeleteCluster: not used")
}
func (f *fakeRegistry) UpsertNode(context.Context, registry.Node) error {
	panic("fakeRegistry.UpsertNode: not used")
}
func (f *fakeRegistry) RemoveNode(context.Context, string, string) error {
	panic("fakeRegistry.RemoveNode: not used")
}
func (f *fakeRegistry) ListNodes(context.Context, string) ([]registry.Node, error) {
	panic("fakeRegistry.ListNodes: not used")
}
func (f *fakeRegistry) ListHistory(context.Context, registry.HistoryFilter) ([]registry.DeploymentHistoryEntry, error) {
	panic("fakeRegistry.ListHistory: not used")
}
func (f *fakeRegistry) MarkSynced(context.Context, string, time.Time) error {
	panic("fakeRegistry.MarkSynced: not used")
}
func (f *fakeRegistry) RecordResource(context.Context, registry.HetznerResource) (int64, error) {
	panic("fakeRegistry.RecordResource: not used")
}
func (f *fakeRegistry) MarkResourceDestroyed(context.Context, int64, time.Time) error {
	panic("fakeRegistry.MarkResourceDestroyed: not used")
}
func (f *fakeRegistry) ListResources(context.Context, string, bool) ([]registry.HetznerResource, error) {
	panic("fakeRegistry.ListResources: not used")
}
func (f *fakeRegistry) ListResourcesByType(context.Context, string, string) ([]registry.HetznerResource, error) {
	panic("fakeRegistry.ListResourcesByType: not used")
}
func (f *fakeRegistry) MarkClusterDestroyed(context.Context, string, time.Time) error {
	panic("fakeRegistry.MarkClusterDestroyed: not used")
}
func (f *fakeRegistry) Close() error { return nil }

// ---------------------------------------------------------------------------
// Catalog helpers — the installer takes a *Catalog whose load() drives the
// embedded addons FS. To avoid coupling the tests to the embedded directory
// layout, we synthesize a Catalog whose addons map is pre-populated.
// ---------------------------------------------------------------------------

// newTestCatalog returns a Catalog seeded with the supplied addons. It marks
// the loadOnce as already consumed so subsequent Get/List calls hit the seed
// rather than re-loading from the embedded FS.
func newTestCatalog(addons map[string]*Addon) *Catalog {
	c := &Catalog{addons: addons}
	c.loadOnce.Do(func() {})
	return c
}

func mkAddon(name, version string, secrets []Secret, requires []string, manifests map[string][]byte) *Addon {
	return &Addon{
		Name:        name,
		Version:     version,
		Description: name + " test addon",
		Strategy:    StrategyManifests,
		Secrets:     secrets,
		Requires:    requires,
		Manifests:   manifests,
	}
}

// installerForTest builds a fully-wired Installer with deterministic Now and
// DeployedBy.
func installerForTest(t *testing.T, cat *Catalog, sec *fakeResolver, kub *fakeKubectl, reg *fakeRegistry) *Installer {
	t.Helper()
	clock := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	tick := 0
	return &Installer{
		Catalog:  cat,
		Secrets:  sec,
		Kubectl:  kub,
		Registry: reg,
		Now: func() time.Time {
			tick++
			return clock.Add(time.Duration(tick) * time.Second)
		},
		DeployedBy: func() string { return "test-user" },
	}
}

// findApply scans a recorded kubectl args slice and returns the path argument
// passed after `-f`, or "" if no apply call was made.
func findFile(args []string) string {
	for i, a := range args {
		if a == "-f" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// readManifestFromKubectlCall returns the contents of the temp file passed
// into a kubectl apply/delete call. Because the installer cleans up the
// temp file on exit, this only works while the call is in flight; tests
// instead inspect via a kubectl runner that reads the file before returning.
type readingKubectl struct {
	*fakeKubectl

	captured []byte
}

func newReadingKubectl() *readingKubectl {
	return &readingKubectl{fakeKubectl: newFakeKubectl()}
}

func (k *readingKubectl) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if path := findFile(args); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			k.mu.Lock()
			k.captured = append(k.captured[:0], data...)
			k.mu.Unlock()
		}
	}
	return k.fakeKubectl.Run(ctx, name, args...)
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

func TestInstall_HappyPath_AppliesAndRecords(t *testing.T) {
	manifest := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\ndata:\n  token: ${API_TOKEN}\n")
	a := mkAddon("test-addon", "v1.0.0",
		[]Secret{{Key: "API_TOKEN", Required: true}},
		nil,
		map[string][]byte{"manifests/cm.yaml": manifest},
	)
	cat := newTestCatalog(map[string]*Addon{a.Name: a})
	sec := &fakeResolver{resolved: map[string]string{"API_TOKEN": "s3cret"}}
	kub := newReadingKubectl()
	reg := newFakeRegistry()
	reg.clusters["alpha"] = registry.Cluster{
		Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}

	inst := installerForTest(t, cat, sec, kub.fakeKubectl, reg)
	inst.Kubectl = kub // override with the reading variant

	if err := inst.Install(context.Background(), "test-addon", "alpha"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// kubectl was called exactly once with apply against the cluster's kubeconfig.
	if got := len(kub.runs); got != 1 {
		t.Fatalf("kubectl call count: want 1, got %d (calls=%+v)", got, kub.runs)
	}
	got := kub.runs[0]
	if got.name != "kubectl" {
		t.Errorf("kubectl binary: got %q", got.name)
	}
	wantPrefix := []string{"--kubeconfig", "/tmp/alpha.yaml", "apply", "-f"}
	for i, want := range wantPrefix {
		if i >= len(got.args) || got.args[i] != want {
			t.Errorf("kubectl args[%d]: want %q, got %v", i, want, got.args)
			break
		}
	}

	// Rendered manifest must have the placeholder substituted.
	if !strings.Contains(string(kub.captured), "token: s3cret") {
		t.Errorf("placeholder substitution missing; rendered=%q", kub.captured)
	}
	if strings.Contains(string(kub.captured), "${API_TOKEN}") {
		t.Errorf("placeholder still present after render; rendered=%q", kub.captured)
	}

	// Temp file must be cleaned up after Install returns.
	if path := findFile(got.args); path != "" {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("temp manifest file should be removed; stat err=%v", err)
		}
	}

	// Secret resolver was called with the cluster's path.
	if len(sec.calls) != 1 {
		t.Fatalf("resolver call count: want 1, got %d", len(sec.calls))
	}
	wantCall := resolveCall{app: "test-addon", env: "prod", provider: "hetzner", region: "nbg1"}
	if sec.calls[0] != wantCall {
		t.Errorf("resolver call: want %+v, got %+v", wantCall, sec.calls[0])
	}

	// Registry recorded the install.
	d, ok := reg.deployments[depKey("alpha", "test-addon")]
	if !ok {
		t.Fatal("deployments row was not written")
	}
	if d.Kind != registry.KindAddon {
		t.Errorf("Kind: want addon, got %q", d.Kind)
	}
	if d.Status != registry.StatusRolledOut {
		t.Errorf("Status: want rolled_out, got %q", d.Status)
	}
	if d.Version != "v1.0.0" {
		t.Errorf("Version: want v1.0.0, got %q", d.Version)
	}
	if d.DeployedBy != "test-user" {
		t.Errorf("DeployedBy: want test-user, got %q", d.DeployedBy)
	}

	if len(reg.history) != 1 {
		t.Fatalf("history rows: want 1, got %d", len(reg.history))
	}
	h := reg.history[0]
	if h.Status != registry.StatusRolledOut {
		t.Errorf("history Status: want rolled_out, got %q", h.Status)
	}
	if h.Kind != registry.KindAddon {
		t.Errorf("history Kind: want addon, got %q", h.Kind)
	}
	if h.RolloutDurationMs <= 0 {
		t.Errorf("RolloutDurationMs: want > 0, got %d", h.RolloutDurationMs)
	}
	if h.Error != "" {
		t.Errorf("history Error must be empty on success, got %q", h.Error)
	}
}

func TestInstall_MissingRequiredSecrets_ListsAll_NoKubectl(t *testing.T) {
	a := mkAddon("net-stack", "v0.1.0",
		[]Secret{
			{Key: "API_TOKEN", Required: true},
			{Key: "WEBHOOK_SECRET", Required: true},
			{Key: "OPTIONAL_KEY", Required: false},
		},
		nil,
		map[string][]byte{"manifests/cm.yaml": []byte("apiVersion: v1\nkind: ConfigMap\n")},
	)
	cat := newTestCatalog(map[string]*Addon{a.Name: a})
	sec := &fakeResolver{resolved: map[string]string{}} // both required keys absent
	kub := newFakeKubectl()
	reg := newFakeRegistry()
	reg.clusters["alpha"] = registry.Cluster{
		Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}

	inst := installerForTest(t, cat, sec, kub, reg)

	err := inst.Install(context.Background(), "net-stack", "alpha")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	for _, want := range []string{"API_TOKEN", "WEBHOOK_SECRET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must list %q; got %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "OPTIONAL_KEY") {
		t.Errorf("optional secret must not appear in error; got %v", err)
	}

	if len(kub.runs) != 0 {
		t.Errorf("kubectl must not be invoked when secrets are missing; calls=%+v", kub.runs)
	}
	if _, ok := reg.deployments[depKey("alpha", "net-stack")]; ok {
		t.Errorf("deployments row must not be written on missing-secret failure")
	}
	if len(reg.history) != 1 {
		t.Fatalf("expected 1 failed history row, got %d", len(reg.history))
	}
	if reg.history[0].Status != registry.StatusFailed {
		t.Errorf("history Status: want failed, got %q", reg.history[0].Status)
	}
}

func TestInstall_MissingEmptyValueTreatedAsMissing(t *testing.T) {
	a := mkAddon("api", "v0.1.0",
		[]Secret{{Key: "API_TOKEN", Required: true}},
		nil,
		map[string][]byte{"manifests/cm.yaml": []byte("apiVersion: v1\n")},
	)
	cat := newTestCatalog(map[string]*Addon{a.Name: a})
	sec := &fakeResolver{resolved: map[string]string{"API_TOKEN": ""}}
	kub := newFakeKubectl()
	reg := newFakeRegistry()
	reg.clusters["alpha"] = registry.Cluster{
		Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}

	inst := installerForTest(t, cat, sec, kub, reg)
	err := inst.Install(context.Background(), "api", "alpha")
	if err == nil {
		t.Fatal("expected error for empty required secret")
	}
	if !strings.Contains(err.Error(), "API_TOKEN") {
		t.Errorf("error must mention key; got %v", err)
	}
}

func TestInstall_MissingRequires_ListsAll_NoKubectl(t *testing.T) {
	a := mkAddon("ingress-nginx", "v0.1.0", nil,
		[]string{"cert-manager", "external-dns"},
		map[string][]byte{"manifests/cm.yaml": []byte("apiVersion: v1\n")},
	)
	cat := newTestCatalog(map[string]*Addon{a.Name: a})
	sec := &fakeResolver{resolved: map[string]string{}}
	kub := newFakeKubectl()
	reg := newFakeRegistry()
	reg.clusters["alpha"] = registry.Cluster{
		Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}
	// Pre-existing app deployment with the same name as a required addon
	// must NOT satisfy the requirement (it is kind=app, not kind=addon).
	reg.deployments[depKey("alpha", "cert-manager")] = registry.Deployment{
		ClusterName: "alpha", Service: "cert-manager", Version: "v1.0",
		Status: registry.StatusRolledOut, Kind: registry.KindApp,
	}

	inst := installerForTest(t, cat, sec, kub, reg)
	err := inst.Install(context.Background(), "ingress-nginx", "alpha")
	if err == nil {
		t.Fatal("expected error from missing requires")
	}
	for _, want := range []string{"cert-manager", "external-dns"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must list %q; got %v", want, err)
		}
	}
	if len(kub.runs) != 0 {
		t.Errorf("kubectl must not be invoked when requires are missing; calls=%+v", kub.runs)
	}
}

func TestInstall_RequiresSatisfiedByAddonKind(t *testing.T) {
	a := mkAddon("ingress-nginx", "v0.1.0", nil,
		[]string{"cert-manager"},
		map[string][]byte{"manifests/cm.yaml": []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: ingress\n")},
	)
	cat := newTestCatalog(map[string]*Addon{a.Name: a})
	sec := &fakeResolver{resolved: map[string]string{}}
	kub := newFakeKubectl()
	reg := newFakeRegistry()
	reg.clusters["alpha"] = registry.Cluster{
		Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}
	reg.deployments[depKey("alpha", "cert-manager")] = registry.Deployment{
		ClusterName: "alpha", Service: "cert-manager", Version: "v1.0",
		Status: registry.StatusRolledOut, Kind: registry.KindAddon,
	}

	inst := installerForTest(t, cat, sec, kub, reg)
	if err := inst.Install(context.Background(), "ingress-nginx", "alpha"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(kub.runs) != 1 {
		t.Errorf("kubectl call count: want 1, got %d", len(kub.runs))
	}
}

func TestInstall_KubectlFailure_NoRegistryWrite(t *testing.T) {
	a := mkAddon("test-addon", "v1.0.0", nil, nil,
		map[string][]byte{"manifests/cm.yaml": []byte("apiVersion: v1\n")},
	)
	cat := newTestCatalog(map[string]*Addon{a.Name: a})
	sec := &fakeResolver{resolved: map[string]string{}}
	kub := newFakeKubectl()
	kub.errs["apply"] = errors.New("kubectl: connection refused")
	reg := newFakeRegistry()
	reg.clusters["alpha"] = registry.Cluster{
		Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}

	inst := installerForTest(t, cat, sec, kub, reg)

	err := inst.Install(context.Background(), "test-addon", "alpha")
	if err == nil {
		t.Fatal("expected kubectl error")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error must wrap underlying cause; got %v", err)
	}

	// The deployments row must NOT be written when kubectl fails.
	if _, ok := reg.deployments[depKey("alpha", "test-addon")]; ok {
		t.Error("deployments row must not exist after kubectl failure")
	}

	// A failure history row MUST be appended for audit.
	if len(reg.history) != 1 {
		t.Fatalf("history rows: want 1 (failed), got %d", len(reg.history))
	}
	if reg.history[0].Status != registry.StatusFailed {
		t.Errorf("history Status: want failed, got %q", reg.history[0].Status)
	}
	if !strings.Contains(reg.history[0].Error, "connection refused") {
		t.Errorf("history Error must capture cause; got %q", reg.history[0].Error)
	}
}

func TestInstall_TempFile_RemovedAfterKubectlFailure(t *testing.T) {
	a := mkAddon("test-addon", "v1.0.0", nil, nil,
		map[string][]byte{"manifests/cm.yaml": []byte("apiVersion: v1\n")},
	)
	cat := newTestCatalog(map[string]*Addon{a.Name: a})
	sec := &fakeResolver{resolved: map[string]string{}}

	var capturedPath string
	kub := newFakeKubectl()
	kub.errs["apply"] = errors.New("boom")
	// Wrap Run to capture the temp file path before the installer's defer
	// tears it down.
	wrapper := &captureRunner{
		inner: kub,
		onRun: func(args []string) {
			if capturedPath == "" {
				capturedPath = findFile(args)
			}
		},
	}

	reg := newFakeRegistry()
	reg.clusters["alpha"] = registry.Cluster{
		Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}

	inst := installerForTest(t, cat, sec, kub, reg)
	inst.Kubectl = wrapper

	_ = inst.Install(context.Background(), "test-addon", "alpha")
	if capturedPath == "" {
		t.Fatal("kubectl was never invoked; cannot verify cleanup")
	}
	if _, err := os.Stat(capturedPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp manifest %q must be removed even on kubectl error; stat err=%v", capturedPath, err)
	}
}

// captureRunner observes args before delegating, so tests can capture the
// temp manifest path while the file still exists.
type captureRunner struct {
	inner *fakeKubectl
	onRun func(args []string)
}

func (r *captureRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if r.onRun != nil {
		r.onRun(args)
	}
	return r.inner.Run(ctx, name, args...)
}

func TestInstall_UnknownAddon_ReturnsErrNotFound(t *testing.T) {
	cat := newTestCatalog(map[string]*Addon{})
	sec := &fakeResolver{}
	kub := newFakeKubectl()
	reg := newFakeRegistry()
	reg.clusters["alpha"] = registry.Cluster{
		Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}

	inst := installerForTest(t, cat, sec, kub, reg)

	err := inst.Install(context.Background(), "ghost", "alpha")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want errors.Is ErrNotFound, got %v", err)
	}
	if len(kub.runs) != 0 {
		t.Errorf("kubectl must not be invoked for unknown addon")
	}
}

func TestInstall_UnknownCluster_ErrorsBeforeKubectl(t *testing.T) {
	a := mkAddon("test-addon", "v1.0.0", nil, nil,
		map[string][]byte{"manifests/cm.yaml": []byte("apiVersion: v1\n")},
	)
	cat := newTestCatalog(map[string]*Addon{a.Name: a})
	sec := &fakeResolver{resolved: map[string]string{}}
	kub := newFakeKubectl()
	reg := newFakeRegistry() // no clusters

	inst := installerForTest(t, cat, sec, kub, reg)

	err := inst.Install(context.Background(), "test-addon", "missing")
	if err == nil {
		t.Fatal("expected error for missing cluster")
	}
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("error must wrap registry.ErrNotFound; got %v", err)
	}
	if len(kub.runs) != 0 {
		t.Errorf("kubectl must not be invoked when cluster is unknown")
	}
}

func TestInstall_MultipleManifests_RenderedAsYAMLStream(t *testing.T) {
	a := mkAddon("multi", "v0.1.0",
		[]Secret{{Key: "TOKEN", Required: true}},
		nil,
		map[string][]byte{
			"manifests/01-ns.yaml":  []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: multi\n"),
			"manifests/02-cm.yaml":  []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: multi\ndata:\n  token: ${TOKEN}\n"),
			"manifests/.gitkeep":    []byte("ignored"),
			"manifests/sub/03.yaml": []byte("apiVersion: v1\nkind: Service\nmetadata:\n  name: multi\n"),
		},
	)
	cat := newTestCatalog(map[string]*Addon{a.Name: a})
	sec := &fakeResolver{resolved: map[string]string{"TOKEN": "abc"}}
	kub := newReadingKubectl()
	reg := newFakeRegistry()
	reg.clusters["alpha"] = registry.Cluster{
		Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}

	inst := installerForTest(t, cat, sec, kub.fakeKubectl, reg)
	inst.Kubectl = kub

	if err := inst.Install(context.Background(), "multi", "alpha"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	out := string(kub.captured)
	if !strings.Contains(out, "kind: Namespace") || !strings.Contains(out, "kind: ConfigMap") || !strings.Contains(out, "kind: Service") {
		t.Errorf("rendered stream missing one of the manifests:\n%s", out)
	}
	if !strings.Contains(out, "---") {
		t.Errorf("rendered stream missing YAML separators:\n%s", out)
	}
	if strings.Contains(out, "ignored") {
		t.Errorf(".gitkeep must be skipped from the rendered stream:\n%s", out)
	}
	if !strings.Contains(out, "token: abc") {
		t.Errorf("placeholder substitution failed:\n%s", out)
	}

	// File-name ordering: 01-ns must precede 02-cm in the rendered stream.
	if i, j := strings.Index(out, "Namespace"), strings.Index(out, "ConfigMap"); i < 0 || j < 0 || i >= j {
		t.Errorf("expected lexicographic file ordering (Namespace before ConfigMap):\n%s", out)
	}
}

func TestInstall_HelmStrategy_NotSupported(t *testing.T) {
	a := mkAddon("gha-runner-scale-set", "v0.1.0", nil, nil,
		map[string][]byte{"manifests/helmchart.yaml": []byte("name: foo\n")},
	)
	a.Strategy = StrategyHelmChart
	cat := newTestCatalog(map[string]*Addon{a.Name: a})
	sec := &fakeResolver{resolved: map[string]string{}}
	kub := newFakeKubectl()
	reg := newFakeRegistry()
	reg.clusters["alpha"] = registry.Cluster{
		Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}

	inst := installerForTest(t, cat, sec, kub, reg)
	err := inst.Install(context.Background(), a.Name, "alpha")
	if err == nil {
		t.Fatal("expected error for helmchart strategy")
	}
	if !strings.Contains(err.Error(), "not yet supported") {
		t.Errorf("error must explain helm is unsupported; got %v", err)
	}
	if len(kub.runs) != 0 {
		t.Errorf("kubectl must not be invoked for helm strategy")
	}
}

func TestUninstall_RemovesRegistryRow_AppendsHistory(t *testing.T) {
	a := mkAddon("test-addon", "v1.0.0", nil, nil,
		map[string][]byte{"manifests/cm.yaml": []byte("apiVersion: v1\nkind: ConfigMap\n")},
	)
	cat := newTestCatalog(map[string]*Addon{a.Name: a})
	sec := &fakeResolver{resolved: map[string]string{}}
	kub := newFakeKubectl()
	reg := newFakeRegistry()
	reg.clusters["alpha"] = registry.Cluster{
		Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}
	reg.deployments[depKey("alpha", "test-addon")] = registry.Deployment{
		ClusterName: "alpha", Service: "test-addon", Version: "v1.0.0",
		Status: registry.StatusRolledOut, Kind: registry.KindAddon,
	}

	inst := installerForTest(t, cat, sec, kub, reg)
	if err := inst.Uninstall(context.Background(), "test-addon", "alpha"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	// Exactly one kubectl call (delete --ignore-not-found).
	if len(kub.runs) != 1 {
		t.Fatalf("kubectl call count: want 1, got %d (%+v)", len(kub.runs), kub.runs)
	}
	args := kub.runs[0].args
	hasDelete := false
	hasIgnoreNotFound := false
	for _, a := range args {
		if a == "delete" {
			hasDelete = true
		}
		if a == "--ignore-not-found" {
			hasIgnoreNotFound = true
		}
	}
	if !hasDelete {
		t.Errorf("kubectl args must contain delete; got %v", args)
	}
	if !hasIgnoreNotFound {
		t.Errorf("kubectl args must contain --ignore-not-found; got %v", args)
	}

	// Registry: deployments row is gone, history has uninstalled row.
	if _, ok := reg.deployments[depKey("alpha", "test-addon")]; ok {
		t.Errorf("deployments row should be removed")
	}
	if len(reg.history) != 1 {
		t.Fatalf("history rows: want 1, got %d", len(reg.history))
	}
	if reg.history[0].Status != registry.StatusUninstalled {
		t.Errorf("history Status: want uninstalled, got %q", reg.history[0].Status)
	}
	if reg.history[0].Kind != registry.KindAddon {
		t.Errorf("history Kind: want addon, got %q", reg.history[0].Kind)
	}
	if reg.history[0].Version != "v1.0.0" {
		t.Errorf("history Version must come from registry row, got %q", reg.history[0].Version)
	}
}

func TestUninstall_NotInstalled_ReturnsDescriptiveError(t *testing.T) {
	a := mkAddon("test-addon", "v1.0.0", nil, nil,
		map[string][]byte{"manifests/cm.yaml": []byte("apiVersion: v1\n")},
	)
	cat := newTestCatalog(map[string]*Addon{a.Name: a})
	sec := &fakeResolver{resolved: map[string]string{}}
	kub := newFakeKubectl()
	reg := newFakeRegistry()
	reg.clusters["alpha"] = registry.Cluster{
		Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}

	inst := installerForTest(t, cat, sec, kub, reg)
	err := inst.Uninstall(context.Background(), "test-addon", "alpha")
	if err == nil {
		t.Fatal("want error for uninstall of un-installed addon")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("error must say 'not installed'; got %v", err)
	}
	if len(kub.runs) != 0 {
		t.Errorf("kubectl must not be invoked when addon is not installed")
	}
	if len(reg.history) != 0 {
		t.Errorf("history must not be written when addon is not installed")
	}
}

func TestUninstall_IsIdempotent(t *testing.T) {
	a := mkAddon("test-addon", "v1.0.0", nil, nil,
		map[string][]byte{"manifests/cm.yaml": []byte("apiVersion: v1\n")},
	)
	cat := newTestCatalog(map[string]*Addon{a.Name: a})
	sec := &fakeResolver{resolved: map[string]string{}}
	kub := newFakeKubectl()
	reg := newFakeRegistry()
	reg.clusters["alpha"] = registry.Cluster{
		Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}
	reg.deployments[depKey("alpha", "test-addon")] = registry.Deployment{
		ClusterName: "alpha", Service: "test-addon", Version: "v1.0.0",
		Status: registry.StatusRolledOut, Kind: registry.KindAddon,
	}

	inst := installerForTest(t, cat, sec, kub, reg)

	// First uninstall succeeds.
	if err := inst.Uninstall(context.Background(), "test-addon", "alpha"); err != nil {
		t.Fatalf("first Uninstall: %v", err)
	}
	// Second uninstall should now fail with "not installed", not crash or
	// double-record. Since the first removed the row, this is the
	// expected idempotency contract: the operator can detect already-gone
	// addons by the not-installed sentinel.
	err := inst.Uninstall(context.Background(), "test-addon", "alpha")
	if err == nil {
		t.Fatal("second Uninstall should report not-installed")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("error must say 'not installed'; got %v", err)
	}
}

func TestUninstall_VersionDriftLogsWarning(t *testing.T) {
	// Catalog has v0.2.0; registry has v0.1.0.
	a := mkAddon("test-addon", "v0.2.0", nil, nil,
		map[string][]byte{"manifests/cm.yaml": []byte("apiVersion: v1\n")},
	)
	cat := newTestCatalog(map[string]*Addon{a.Name: a})
	sec := &fakeResolver{resolved: map[string]string{}}
	kub := newFakeKubectl()
	reg := newFakeRegistry()
	reg.clusters["alpha"] = registry.Cluster{
		Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}
	reg.deployments[depKey("alpha", "test-addon")] = registry.Deployment{
		ClusterName: "alpha", Service: "test-addon", Version: "v0.1.0",
		Status: registry.StatusRolledOut, Kind: registry.KindAddon,
	}

	inst := installerForTest(t, cat, sec, kub, reg)

	stderr := captureStderr(t, func() {
		if err := inst.Uninstall(context.Background(), "test-addon", "alpha"); err != nil {
			t.Fatalf("Uninstall: %v", err)
		}
	})
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "v0.1.0") {
		t.Errorf("expected version-drift warning on stderr; got %q", stderr)
	}

	// History entry should record the *registry* version (v0.1.0), not the
	// catalog version (v0.2.0): the audit trail is "we removed what was
	// installed", not "we removed what we currently know about".
	if len(reg.history) != 1 || reg.history[0].Version != "v0.1.0" {
		t.Errorf("history Version: want v0.1.0 (from registry); got %+v", reg.history)
	}
}

func TestUpgrade_UpdatesVersion(t *testing.T) {
	// Pre-existing v1.0.0 deployment; catalog now has v2.0.0.
	a := mkAddon("test-addon", "v2.0.0",
		[]Secret{{Key: "TOKEN", Required: true}},
		nil,
		map[string][]byte{"manifests/cm.yaml": []byte("apiVersion: v1\nkind: ConfigMap\ndata:\n  token: ${TOKEN}\n")},
	)
	cat := newTestCatalog(map[string]*Addon{a.Name: a})
	sec := &fakeResolver{resolved: map[string]string{"TOKEN": "fresh"}}
	kub := newReadingKubectl()
	reg := newFakeRegistry()
	reg.clusters["alpha"] = registry.Cluster{
		Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}
	reg.deployments[depKey("alpha", "test-addon")] = registry.Deployment{
		ClusterName: "alpha", Service: "test-addon", Version: "v1.0.0",
		Status: registry.StatusRolledOut, Kind: registry.KindAddon,
		DeployedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	inst := installerForTest(t, cat, sec, kub.fakeKubectl, reg)
	inst.Kubectl = kub

	if err := inst.Upgrade(context.Background(), "test-addon", "alpha"); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}

	d := reg.deployments[depKey("alpha", "test-addon")]
	if d.Version != "v2.0.0" {
		t.Errorf("Version: want v2.0.0, got %q", d.Version)
	}
	if !d.DeployedAt.After(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("DeployedAt should advance on upgrade; got %v", d.DeployedAt)
	}
	if len(reg.history) != 1 {
		t.Errorf("history rows: want 1 rolled_out, got %d", len(reg.history))
	}
	if reg.history[0].Status != registry.StatusRolledOut || reg.history[0].Version != "v2.0.0" {
		t.Errorf("history must record new version rolled_out; got %+v", reg.history[0])
	}
}

func TestSubstitutePlaceholders_ExactMatchOnly(t *testing.T) {
	resolved := map[string]string{
		"TOKEN":   "abc",
		"NUMBER1": "1",
	}
	cases := []struct {
		in, want string
	}{
		{"prefix ${TOKEN} suffix", "prefix abc suffix"},
		{"${NUMBER1}", "1"},
		// Lowercase keys must NOT match.
		{"${token}", "${token}"},
		// Empty body must NOT match.
		{"${}", "${}"},
		// Hyphens must NOT match.
		{"${not-a-secret}", "${not-a-secret}"},
		// Missing keys must remain.
		{"${UNKNOWN}", "${UNKNOWN}"},
		// Multiple substitutions on one line.
		{"a=${TOKEN}, b=${NUMBER1}, c=${TOKEN}", "a=abc, b=1, c=abc"},
		// Unrelated $... constructs are untouched.
		{"$TOKEN ${TOKEN}", "$TOKEN abc"},
		// Helm-style {{ .Values.foo }} unaffected.
		{"{{ .Values.foo }}", "{{ .Values.foo }}"},
	}
	for _, tc := range cases {
		got := string(substitutePlaceholders([]byte(tc.in), resolved))
		if got != tc.want {
			t.Errorf("substitutePlaceholders(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderManifests_DeterministicOrder(t *testing.T) {
	// Identical addons rendered twice must yield byte-identical output.
	a := mkAddon("multi", "v0.1.0", nil, nil, map[string][]byte{
		"manifests/zz.yaml": []byte("apiVersion: v1\nkind: B\n"),
		"manifests/aa.yaml": []byte("apiVersion: v1\nkind: A\n"),
		"manifests/mm.yaml": []byte("apiVersion: v1\nkind: M\n"),
	})
	out1, err := renderManifests(a, nil)
	if err != nil {
		t.Fatalf("render1: %v", err)
	}
	out2, err := renderManifests(a, nil)
	if err != nil {
		t.Fatalf("render2: %v", err)
	}
	if string(out1) != string(out2) {
		t.Fatalf("renderManifests is not deterministic")
	}
	// Lexicographic order: kind A before kind M before kind B.
	idxA := strings.Index(string(out1), "kind: A")
	idxM := strings.Index(string(out1), "kind: M")
	idxB := strings.Index(string(out1), "kind: B")
	if idxA >= idxM || idxM >= idxB {
		t.Errorf("expected lexicographic order A<M<B; got positions %d, %d, %d", idxA, idxM, idxB)
	}
}

// captureStderr redirects os.Stderr for the duration of fn.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })

	done := make(chan string)
	go func() {
		var b strings.Builder
		buf := make([]byte, 1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()

	fn()
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// Compile-time guarantee that fakeRegistry satisfies registry.Registry. If
// the interface evolves, this line breaks the build before tests run.
var _ registry.Registry = (*fakeRegistry)(nil)

// Sanity: the embedded gha-runner-scale-set addon's manifest file must be
// referenceable through filepath without any platform-specific separator
// surprises. (We don't actually rely on filepath in the installer, but this
// catches an accidental Windows-style path slipping into the loader.)
func TestEmbeddedAddon_HelmchartIsHelmStrategy(t *testing.T) {
	cat := DefaultCatalog()
	a, err := cat.Get("gha-runner-scale-set")
	if err != nil {
		t.Fatalf("DefaultCatalog.Get: %v", err)
	}
	if a.Strategy != StrategyHelmChart {
		t.Fatalf("stub addon strategy: want helmchart, got %q", a.Strategy)
	}
	// Confirm the helm strategy is rejected by the installer end-to-end with
	// a real (embedded) catalog rather than only the synthetic test fixture.
	sec := &fakeResolver{resolved: map[string]string{
		"GH_APP_ID":              "1",
		"GH_APP_INSTALLATION_ID": "2",
		"GH_APP_PRIVATE_KEY":     "key",
	}}
	kub := newFakeKubectl()
	reg := newFakeRegistry()
	reg.clusters["alpha"] = registry.Cluster{
		Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}
	inst := &Installer{
		Catalog: cat, Secrets: sec, Kubectl: kub, Registry: reg,
		Now: func() time.Time { return time.Unix(0, 0).UTC() },
	}
	err = inst.Install(context.Background(), "gha-runner-scale-set", "alpha")
	if err == nil {
		t.Fatal("expected helm-strategy not-supported error")
	}
	if !strings.Contains(err.Error(), "not yet supported") {
		t.Errorf("error must explain helm is unsupported; got %v", err)
	}
}
