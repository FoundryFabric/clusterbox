package baremetal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/provision/baremetal"
	"github.com/foundryfabric/clusterbox/internal/provision/baremetal/provisiontest"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/secrets"
)

// memRegistry is an in-memory Registry double for capturing writes.
type memRegistry struct {
	mu       sync.Mutex
	clusters []registry.Cluster
	nodes    []registry.Node
	closed   bool
}

func (r *memRegistry) UpsertCluster(_ context.Context, c registry.Cluster) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clusters = append(r.clusters, c)
	return nil
}
func (r *memRegistry) UpsertNode(_ context.Context, n registry.Node) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes = append(r.nodes, n)
	return nil
}
func (r *memRegistry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

// Unused interface methods.
func (r *memRegistry) GetCluster(context.Context, string) (registry.Cluster, error) {
	panic("not used")
}
func (r *memRegistry) ListClusters(context.Context) ([]registry.Cluster, error) {
	panic("not used")
}
func (r *memRegistry) DeleteCluster(context.Context, string) error      { panic("not used") }
func (r *memRegistry) RemoveNode(context.Context, string, string) error { panic("not used") }
func (r *memRegistry) ListNodes(context.Context, string) ([]registry.Node, error) {
	panic("not used")
}
func (r *memRegistry) UpsertDeployment(context.Context, registry.Deployment) error {
	panic("not used")
}
func (r *memRegistry) GetDeployment(context.Context, string, string) (registry.Deployment, error) {
	panic("not used")
}
func (r *memRegistry) ListDeployments(context.Context, string) ([]registry.Deployment, error) {
	panic("not used")
}
func (r *memRegistry) DeleteDeployment(context.Context, string, string) error {
	panic("not used")
}
func (r *memRegistry) AppendHistory(context.Context, registry.DeploymentHistoryEntry) error {
	panic("not used")
}
func (r *memRegistry) ListHistory(context.Context, registry.HistoryFilter) ([]registry.DeploymentHistoryEntry, error) {
	panic("not used")
}
func (r *memRegistry) MarkSynced(context.Context, string, time.Time) error { panic("not used") }
func (r *memRegistry) RecordResource(context.Context, registry.HetznerResource) (int64, error) {
	panic("not used")
}
func (r *memRegistry) MarkResourceDestroyed(context.Context, int64, time.Time) error {
	panic("not used")
}
func (r *memRegistry) ListResources(context.Context, string, bool) ([]registry.HetznerResource, error) {
	panic("not used")
}
func (r *memRegistry) ListResourcesByType(context.Context, string, string) ([]registry.HetznerResource, error) {
	panic("not used")
}
func (r *memRegistry) MarkClusterDestroyed(context.Context, string, time.Time) error {
	panic("not used")
}

// installResponder wraps a baremetal.Transport, intercepts the install
// command (by " install --config " infix match) and returns a canned
// response. All other calls pass through to the inner Transport. This
// is how T7b's tests avoid having to know the content-hashed upload
// paths in advance.
type installResponder struct {
	inner *provisiontest.MockTransport

	resp provisiontest.MockRunResponse

	mu      sync.Mutex
	matched bool
	cmdSeen string
	envSeen map[string]string
}

func newResponder(stdout []byte, exit int) *installResponder {
	return &installResponder{
		inner: &provisiontest.MockTransport{
			RunResponses: map[string]provisiontest.MockRunResponse{
				"uname -m": {Stdout: []byte("x86_64\n")},
			},
		},
		resp: provisiontest.MockRunResponse{Stdout: stdout, ExitCode: exit},
	}
}


func (r *installResponder) Run(ctx context.Context, cmd string, envOverlay map[string]string) ([]byte, []byte, int, error) {
	if strings.Contains(cmd, " install --config ") {
		r.mu.Lock()
		r.matched = true
		r.cmdSeen = cmd
		if envOverlay != nil {
			r.envSeen = make(map[string]string, len(envOverlay))
			for k, v := range envOverlay {
				r.envSeen[k] = v
			}
		}
		resp := r.resp
		r.mu.Unlock()
		if resp.Err != nil {
			return resp.Stdout, resp.Stderr, -1, resp.Err
		}
		return resp.Stdout, resp.Stderr, resp.ExitCode, nil
	}
	return r.inner.Run(ctx, cmd, envOverlay)
}

func (r *installResponder) Upload(ctx context.Context, path string, data []byte) error {
	return r.inner.Upload(ctx, path, data)
}
func (r *installResponder) Remove(ctx context.Context, path string) error {
	return r.inner.Remove(ctx, path)
}
func (r *installResponder) Close() error { return r.inner.Close() }

func (r *installResponder) snapshot() (string, map[string]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	envCopy := make(map[string]string, len(r.envSeen))
	for k, v := range r.envSeen {
		envCopy[k] = v
	}
	return r.cmdSeen, envCopy, r.matched
}

// successInstallJSON returns the canned stdout the install command emits
// for a happy-path k3s server-init run.
func successInstallJSON(t *testing.T, k3sVersion, kubeconfig string) []byte {
	t.Helper()
	doc := map[string]interface{}{
		"sections": map[string]interface{}{
			"harden":    map[string]interface{}{"applied": false, "reason": "section not implemented yet"},
			"tailscale": map[string]interface{}{"applied": false, "reason": "section not implemented yet"},
			"k3s": map[string]interface{}{
				"applied":         true,
				"role":            "server-init",
				"k3s_version":     k3sVersion,
				"kubeconfig_yaml": kubeconfig,
				"node_token":      "K1example",
				"server_url":      "https://127.0.0.1:6443",
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(b, '\n')
}

// errorInstallJSON returns the canned error-shape doc.
func errorInstallJSON(t *testing.T, msg, section string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]interface{}{
		"error":           msg,
		"section":         section,
		"sections_so_far": map[string]interface{}{"harden": map[string]interface{}{"applied": true}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

const loopbackKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: default
  cluster:
    server: https://127.0.0.1:6443
    certificate-authority-data: aGVsbG8=
contexts:
- name: default
  context:
    cluster: default
    user: default
users:
- name: default
  user:
    token: t0ken
current-context: default
`

func dialFn(tr baremetal.Transport) func(context.Context, baremetal.DialConfig) (baremetal.Transport, error) {
	return func(context.Context, baremetal.DialConfig) (baremetal.Transport, error) { return tr, nil }
}

// TestProvision_HappyPath_DefaultSpec drives the full flow with the
// in-tree DefaultSpec.
func TestProvision_HappyPath_DefaultSpec(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	resp := newResponder(successInstallJSON(t, "v1.32.3+k3s1", loopbackKubeconfig), 0)
	reg := &memRegistry{}
	dest := filepath.Join(tmpHome, ".kube", "br-1.yaml")

	now := time.Date(2024, 7, 1, 12, 0, 0, 0, time.UTC)
	prov := baremetal.New(baremetal.Deps{
		Host:               "203.0.113.7",
		User:               "ops",
		SSHKeyPath:         "/dev/null",
		KubeconfigPath:     dest,
		AgentBundleForArch: func(string) ([]byte, error) { return []byte("\x7fELF-fake-agent"), nil },
		Dial:               dialFn(resp),
		OpenRegistry:       func(context.Context) (registry.Registry, error) { return reg, nil },
		Now:                func() time.Time { return now },
		Out:                io.Discard,
	})

	res, err := prov.Provision(context.Background(), provision.ClusterConfig{
		ClusterName: "br-1",
		Location:    "rack-a",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.KubeconfigPath != dest {
		t.Errorf("KubeconfigPath = %q, want %q", res.KubeconfigPath, dest)
	}
	if len(res.Nodes) != 1 || res.Nodes[0].Role != "control-plane" {
		t.Errorf("nodes = %+v", res.Nodes)
	}

	// Kubeconfig was written, mode 0600, server rewritten.
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat kubeconfig: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("kubeconfig mode = %v, want 0600", mode)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read kubeconfig: %v", err)
	}
	if !strings.Contains(string(got), "https://203.0.113.7:6443") {
		t.Errorf("kubeconfig server URL not rewritten:\n%s", got)
	}
	if strings.Contains(string(got), "127.0.0.1") {
		t.Errorf("kubeconfig still references loopback:\n%s", got)
	}

	// Registry recorded a control-plane node.
	if len(reg.clusters) != 1 || reg.clusters[0].Provider != baremetal.Name {
		t.Errorf("clusters = %+v", reg.clusters)
	}
	if len(reg.nodes) != 1 || reg.nodes[0].Role != "control-plane" {
		t.Errorf("nodes = %+v", reg.nodes)
	}

	// Both uploaded paths were Removed.
	if rm := resp.inner.Removed(); len(rm) != 2 {
		t.Errorf("expected 2 removals, got %v", rm)
	}

	// Install command targeted /tmp/clusterboxnode-<sha> and /tmp/clusterbox-node-<sha>.yaml.
	cmdSeen, _, matched := resp.snapshot()
	if !matched {
		t.Fatalf("install command not invoked")
	}
	if !strings.Contains(cmdSeen, "/tmp/clusterboxnode-") || !strings.Contains(cmdSeen, "/tmp/clusterbox-node-") {
		t.Errorf("install cmd missing expected paths: %q", cmdSeen)
	}
}

// TestProvision_RejectsZeroDeps verifies that constructing without
// Host/User/SSHKeyPath fails fast with a descriptive error.
func TestProvision_RejectsZeroDeps(t *testing.T) {
	prov := baremetal.New(baremetal.Deps{})
	_, err := prov.Provision(context.Background(), provision.ClusterConfig{ClusterName: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"Host", "User", "SSHKeyPath"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q, got %v", want, err)
		}
	}
}

// TestProvision_UnameUnsupportedArch verifies an unknown uname output
// surfaces a clear error and stops before any upload.
func TestProvision_UnameUnsupportedArch(t *testing.T) {
	mock := &provisiontest.MockTransport{
		RunResponses: map[string]provisiontest.MockRunResponse{
			"uname -m": {Stdout: []byte("riscv64\n")},
		},
	}
	prov := baremetal.New(baremetal.Deps{
		Host: "h", User: "u", SSHKeyPath: "/dev/null",
		Dial: dialFn(mock),
		Out:  io.Discard,
	})
	_, err := prov.Provision(context.Background(), provision.ClusterConfig{ClusterName: "c"})
	if err == nil || !strings.Contains(err.Error(), "unsupported arch") {
		t.Fatalf("expected unsupported-arch error, got %v", err)
	}
	if len(mock.Uploaded()) != 0 {
		t.Errorf("must not upload on arch failure, got %v", mock.Uploaded())
	}
}

// TestProvision_InstallErrorShape verifies the error envelope from
// clusterboxnode is surfaced with the failing section name.
func TestProvision_InstallErrorShape(t *testing.T) {
	resp := newResponder(errorInstallJSON(t, "k3s install failed: curl exit 22", "k3s"), 1)

	prov := baremetal.New(baremetal.Deps{
		Host: "h", User: "u", SSHKeyPath: "/dev/null",
		AgentBundleForArch: func(string) ([]byte, error) { return []byte("agent"), nil },
		Dial:               dialFn(resp),
		Out:                io.Discard,
		KubeconfigPath:     filepath.Join(t.TempDir(), "k.yaml"),
		OpenRegistry:       func(context.Context) (registry.Registry, error) { return &memRegistry{}, nil },
	})

	_, err := prov.Provision(context.Background(), provision.ClusterConfig{ClusterName: "c"})
	if err == nil {
		t.Fatal("expected install error")
	}
	if !strings.Contains(err.Error(), "k3s install failed: curl exit 22") {
		t.Errorf("error message lost: %v", err)
	}
	if !strings.Contains(err.Error(), "section k3s") {
		t.Errorf("error must mention failing section: %v", err)
	}
}

// TestProvision_SudoFailureWrapped verifies ErrSudoNotPasswordless is
// wrapped into a user-facing error that mentions the user.
func TestProvision_SudoFailureWrapped(t *testing.T) {
	mock := &provisiontest.MockTransport{
		RunResponses: map[string]provisiontest.MockRunResponse{
			"uname -m": {Err: baremetal.ErrSudoNotPasswordless},
		},
	}
	prov := baremetal.New(baremetal.Deps{
		Host: "h", User: "ops", SSHKeyPath: "/dev/null",
		Dial: dialFn(mock),
		Out:  io.Discard,
	})
	_, err := prov.Provision(context.Background(), provision.ClusterConfig{ClusterName: "c"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, baremetal.ErrSudoNotPasswordless) {
		t.Errorf("error must wrap ErrSudoNotPasswordless, got %v", err)
	}
	if !strings.Contains(err.Error(), "ops") {
		t.Errorf("error must mention the user: %v", err)
	}
}

// TestProvision_KubeconfigOverwriteWarning verifies that an existing
// kubeconfig at the destination is replaced and a warning is printed.
func TestProvision_KubeconfigOverwriteWarning(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "k.yaml")
	if err := os.WriteFile(dest, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var stderr bytes.Buffer
	resp := newResponder(successInstallJSON(t, "v1.32.3+k3s1", loopbackKubeconfig), 0)

	prov := baremetal.New(baremetal.Deps{
		Host: "h", User: "u", SSHKeyPath: "/dev/null",
		AgentBundleForArch: func(string) ([]byte, error) { return []byte("agent"), nil },
		Dial:               dialFn(resp),
		Out:                &stderr,
		KubeconfigPath:     dest,
		OpenRegistry:       func(context.Context) (registry.Registry, error) { return &memRegistry{}, nil },
	})

	if _, err := prov.Provision(context.Background(), provision.ClusterConfig{ClusterName: "c"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !strings.Contains(stderr.String(), "warning: overwriting existing kubeconfig") {
		t.Errorf("expected overwrite warning, got: %s", stderr.String())
	}
}

// TestProvision_SecretsResolverWired verifies that Tailscale.AuthKeyEnv
// triggers a secret lookup and the resolved value reaches the install
// envOverlay (and never appears in any captured Out output).
func TestProvision_SecretsResolverWired(t *testing.T) {
	const secretValue = "tskey-auth-NEVER-LOG-THIS"
	const envName = "TS_AUTHKEY"

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "node.yaml")
	cfgYAML := []byte(`hostname: cb
tailscale:
  enabled: true
  auth_key_env: ` + envName + `
k3s:
  enabled: true
  role: server-init
  version: v1.32.3+k3s1
`)
	if err := os.WriteFile(cfgPath, cfgYAML, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	resp := newResponder(successInstallJSON(t, "v1.32.3+k3s1", loopbackKubeconfig), 0)
	var out bytes.Buffer
	prov := baremetal.New(baremetal.Deps{
		Host: "h", User: "u", SSHKeyPath: "/dev/null",
		AgentBundleForArch: func(string) ([]byte, error) { return []byte("agent"), nil },
		Dial:               dialFn(resp),
		Out:                &out,
		KubeconfigPath:     filepath.Join(tmp, "k.yaml"),
		ConfigPath:         cfgPath,
		OpenRegistry:       func(context.Context) (registry.Registry, error) { return &memRegistry{}, nil },
		SecretsResolver: secretsResolverFunc(func(_ context.Context, _, _, _, _ string) (map[string]string, error) {
			return map[string]string{envName: secretValue}, nil
		}),
		SecretsApp: "cb", SecretsEnv: "test", SecretsRegion: "lab",
	})

	if _, err := prov.Provision(context.Background(), provision.ClusterConfig{ClusterName: "cb"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	_, env, matched := resp.snapshot()
	if !matched {
		t.Fatalf("install command not invoked")
	}
	if env[envName] != secretValue {
		t.Errorf("envOverlay[%s] = %q, want secret value", envName, env[envName])
	}
	if strings.Contains(out.String(), secretValue) {
		t.Errorf("Out leaked the secret value:\n%s", out.String())
	}
}

// TestProvision_NoJSONInOutput verifies the parser surfaces a clear
// error when the install binary emits non-JSON output.
func TestProvision_NoJSONInOutput(t *testing.T) {
	resp := newResponder([]byte("plain stdout, no JSON anywhere"), 0)
	prov := baremetal.New(baremetal.Deps{
		Host: "h", User: "u", SSHKeyPath: "/dev/null",
		AgentBundleForArch: func(string) ([]byte, error) { return []byte("agent"), nil },
		Dial:               dialFn(resp),
		Out:                io.Discard,
		KubeconfigPath:     filepath.Join(t.TempDir(), "k.yaml"),
		OpenRegistry:       func(context.Context) (registry.Registry, error) { return &memRegistry{}, nil },
	})
	_, err := prov.Provision(context.Background(), provision.ClusterConfig{ClusterName: "c"})
	if err == nil || !strings.Contains(err.Error(), "no JSON document") {
		t.Fatalf("want no-JSON error, got %v", err)
	}
}

// TestProvision_AgentBundleErrorShortCircuits verifies an
// AgentBundleForArch error stops the flow before any upload attempt.
func TestProvision_AgentBundleErrorShortCircuits(t *testing.T) {
	resp := newResponder(nil, 0)
	prov := baremetal.New(baremetal.Deps{
		Host: "h", User: "u", SSHKeyPath: "/dev/null",
		AgentBundleForArch: func(string) ([]byte, error) { return nil, errors.New("agent missing") },
		Dial:               dialFn(resp),
		Out:                io.Discard,
	})
	_, err := prov.Provision(context.Background(), provision.ClusterConfig{ClusterName: "c"})
	if err == nil || !strings.Contains(err.Error(), "agent missing") {
		t.Fatalf("want agent-bundle error, got %v", err)
	}
	if len(resp.inner.Uploaded()) != 0 {
		t.Errorf("must not upload when agent bundle fails: %v", resp.inner.Uploaded())
	}
}

// TestDefaultSpec_K3sEnabledServerInit asserts the in-tree default
// produces a server-init k3s control-plane spec.
func TestDefaultSpec_K3sEnabledServerInit(t *testing.T) {
	spec := baremetal.DefaultSpec("c", "control-plane")
	if spec.K3s == nil || !spec.K3s.Enabled {
		t.Fatal("k3s must be enabled by default")
	}
	if spec.K3s.Role != "server-init" {
		t.Errorf("role = %q, want server-init", spec.K3s.Role)
	}
	if spec.K3s.Version == "" {
		t.Error("version must be set")
	}
	if err := spec.Validate(); err != nil {
		t.Errorf("DefaultSpec must validate: %v", err)
	}
}

// TestResolveSecretsForSpec_NoTailscaleNoLookup verifies a Spec without
// Tailscale.AuthKeyEnv does not call the resolver.
func TestResolveSecretsForSpec_NoTailscaleNoLookup(t *testing.T) {
	called := false
	r := secretsResolverFunc(func(context.Context, string, string, string, string) (map[string]string, error) {
		called = true
		return nil, nil
	})
	spec := baremetal.DefaultSpec("c", "control-plane")
	env, err := baremetal.ResolveSecretsForSpec(context.Background(), spec, r, "a", "e", "p", "rg")
	if err != nil {
		t.Fatalf("ResolveSecretsForSpec: %v", err)
	}
	if env != nil {
		t.Errorf("env must be nil, got %v", env)
	}
	if called {
		t.Error("resolver must not be called when no env keys referenced")
	}
}

// TestResolveSecretsForSpec_MissingResolver verifies an env-key-bearing
// Spec without a resolver fails fast.
func TestResolveSecretsForSpec_MissingResolver(t *testing.T) {
	spec := &config.Spec{
		Tailscale: &config.TailscaleSpec{Enabled: true, AuthKeyEnv: "TS_KEY"},
	}
	_, err := baremetal.ResolveSecretsForSpec(context.Background(), spec, nil, "", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "no secrets resolver") {
		t.Fatalf("want missing-resolver error, got %v", err)
	}
}

// TestResolveSecretsForSpec_KeyNotFound verifies a resolver that
// doesn't return the requested key produces an error that names the
// key but never the value.
func TestResolveSecretsForSpec_KeyNotFound(t *testing.T) {
	spec := &config.Spec{
		Tailscale: &config.TailscaleSpec{Enabled: true, AuthKeyEnv: "TS_KEY"},
	}
	r := secretsResolverFunc(func(context.Context, string, string, string, string) (map[string]string, error) {
		return map[string]string{"OTHER": "x"}, nil
	})
	_, err := baremetal.ResolveSecretsForSpec(context.Background(), spec, r, "", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "TS_KEY") {
		t.Fatalf("want missing-key error mentioning key name, got %v", err)
	}
}

// TestProvider_Name returns the registry key.
func TestProvider_Name(t *testing.T) {
	p := baremetal.New(baremetal.Deps{Host: "h", User: "u", SSHKeyPath: "/dev/null"})
	if p.Name() != baremetal.Name {
		t.Errorf("Name = %q, want %q", p.Name(), baremetal.Name)
	}
}

// TestDestroyAndReconcile_NoOpForSingleHost verifies that the lifecycle
// methods are no-ops on a baremetal target (no provider-side state to
// touch) and never return errors.
func TestDestroyAndReconcile_NoOpForSingleHost(t *testing.T) {
	p := baremetal.New(baremetal.Deps{Host: "h", User: "u", SSHKeyPath: "/dev/null"})
	if err := p.Destroy(context.Background(), registry.Cluster{Name: "c"}); err != nil {
		t.Errorf("Destroy: %v", err)
	}
	summary, err := p.Reconcile(context.Background(), "c")
	if err != nil {
		t.Errorf("Reconcile: %v", err)
	}
	if summary.Added != 0 || summary.Existing != 0 || summary.MarkedDestroyed != 0 || len(summary.Unmanaged) != 0 {
		t.Errorf("Reconcile must return zero summary, got %+v", summary)
	}
}

// secretsResolverFunc is a test-only Resolver shim.
type secretsResolverFunc func(ctx context.Context, app, env, provider, region string) (map[string]string, error)

func (f secretsResolverFunc) Resolve(ctx context.Context, app, env, provider, region string) (map[string]string, error) {
	return f(ctx, app, env, provider, region)
}

var _ secrets.Resolver = secretsResolverFunc(nil)
