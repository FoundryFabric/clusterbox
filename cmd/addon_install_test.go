package cmd_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/foundryfabric/clusterbox/cmd"
	"github.com/foundryfabric/clusterbox/internal/addon"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/registry/sqlite"
	"github.com/foundryfabric/clusterbox/internal/secrets"
)

// fakeInstaller satisfies the cmd-internal addonInstaller interface (which is
// not exported, but the cobra wrappers accept it via cmd.AddonCmdDeps's
// exported field). It records every call and optionally returns canned
// errors.
type fakeInstaller struct {
	mu sync.Mutex

	installCalls   []installCall
	uninstallCalls []installCall
	upgradeCalls   []installCall

	installErr   error
	uninstallErr error
	upgradeErr   error
}

type installCall struct {
	addon, cluster, mode string
}

func (f *fakeInstaller) Install(_ context.Context, addonName, cluster, mode string) error {
	f.mu.Lock()
	f.installCalls = append(f.installCalls, installCall{addonName, cluster, mode})
	f.mu.Unlock()
	return f.installErr
}

func (f *fakeInstaller) Uninstall(_ context.Context, addonName, cluster string) error {
	f.mu.Lock()
	f.uninstallCalls = append(f.uninstallCalls, installCall{addonName, cluster, ""})
	f.mu.Unlock()
	return f.uninstallErr
}

func (f *fakeInstaller) Upgrade(_ context.Context, addonName, cluster, mode string) error {
	f.mu.Lock()
	f.upgradeCalls = append(f.upgradeCalls, installCall{addonName, cluster, mode})
	f.mu.Unlock()
	return f.upgradeErr
}

// fakeKubectlRunner implements secrets.CommandRunner. It records every call
// and short-circuits a canned error for invocations whose args contain
// errOnVerb (e.g. "apply"). errOnVerb=="" means every call succeeds.
type fakeKubectlRunner struct {
	mu sync.Mutex

	calls     []kubectlCall
	errOnVerb string
	err       error
}

type kubectlCall struct {
	name string
	args []string
}

func (k *fakeKubectlRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	k.mu.Lock()
	cp := append([]string(nil), args...)
	k.calls = append(k.calls, kubectlCall{name: name, args: cp})
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

// staticResolver returns a fixed map for every Resolve call.
type staticResolver struct {
	values map[string]string
}

func (r *staticResolver) Resolve(_ context.Context, _, _, _, _ string) (map[string]string, error) {
	out := make(map[string]string, len(r.values))
	for k, v := range r.values {
		out[k] = v
	}
	return out, nil
}

// resolverFactory returns a NewResolver factory that always yields the
// supplied resolver and a nil closer.
func resolverFactory(r secrets.Resolver) func(ctx context.Context) (secrets.Resolver, io.Closer, error) {
	return func(_ context.Context) (secrets.Resolver, io.Closer, error) {
		return r, nil, nil
	}
}

// addonTestEnv bundles the moving parts an end-to-end addon test needs.
// dbPath is on disk so the production cleanup can Close the registry handle
// without losing data; the test re-opens it through reopenRegistry to inspect
// post-run state.
type addonTestEnv struct {
	t *testing.T

	dbPath  string
	catalog *addon.Catalog
	runner  *fakeKubectlRunner
	deps    cmd.AddonCmdDeps
}

// newAddonTestEnv builds a hermetic environment with:
//   - a custom in-memory addon catalog containing a single "demo" addon
//     using the manifests strategy and one required secret;
//   - a fresh sqlite registry on disk seeded with a cluster row;
//   - a fakeKubectlRunner wired into cmd.AddonCmdDeps;
//   - a static secrets resolver that satisfies the addon's required keys.
//
// The returned env exposes deps suitable for passing into RunAddonInstall /
// RunAddonUninstall / RunAddonUpgrade, plus a reopenRegistry helper for
// post-run assertions.
func newAddonTestEnv(t *testing.T) *addonTestEnv {
	t.Helper()

	cat := buildDemoCatalog(t)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "registry.db")

	// Seed the registry with the target cluster row before handing the path
	// to the deps factory: the installer expects GetCluster to succeed.
	{
		reg, err := sqlite.New(dbPath)
		if err != nil {
			t.Fatalf("open seed registry: %v", err)
		}
		if err := reg.UpsertCluster(context.Background(), registry.Cluster{
			Name: "alpha", Provider: "hetzner", Region: "nbg1", Env: "prod",
			KubeconfigPath: "/tmp/alpha.yaml",
		}); err != nil {
			t.Fatalf("seed cluster: %v", err)
		}
		if err := reg.Close(); err != nil {
			t.Fatalf("close seed registry: %v", err)
		}
	}

	runner := &fakeKubectlRunner{}
	deps := cmd.AddonCmdDeps{
		Catalog:      cat,
		OpenRegistry: func(_ context.Context) (registry.Registry, error) { return sqlite.New(dbPath) },
		NewResolver:  resolverFactory(&staticResolver{values: map[string]string{"API_TOKEN": "s3cret"}}),
		Runner:       runner,
	}
	return &addonTestEnv{t: t, dbPath: dbPath, catalog: cat, runner: runner, deps: deps}
}

// reopenRegistry returns a fresh sqlite handle on the same on-disk DB and
// registers cleanup. Tests use this to read post-run state without colliding
// with the wrapper's own Close.
func (e *addonTestEnv) reopenRegistry() registry.Registry {
	e.t.Helper()
	reg, err := sqlite.New(e.dbPath)
	if err != nil {
		e.t.Fatalf("reopen registry: %v", err)
	}
	e.t.Cleanup(func() { _ = reg.Close() })
	return reg
}

// buildDemoCatalog synthesises an addon.Catalog containing a single addon
// named "demo" with the manifests strategy and one required secret. It uses
// fstest.MapFS so the test does not depend on the embedded addons/ tree.
func buildDemoCatalog(t *testing.T) *addon.Catalog {
	t.Helper()
	mfs := fstest.MapFS{
		"addons/demo/addon.yaml": &fstest.MapFile{
			Data: []byte(`name: demo
version: v1.0.0
description: demo addon for cmd-layer tests
strategy: manifests
secrets:
  - key: API_TOKEN
    description: token
    required: true
`),
		},
		"addons/demo/manifests/cm.yaml": &fstest.MapFile{
			Data: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: demo\ndata:\n  token: ${API_TOKEN}\n"),
		},
	}
	return addon.NewCatalogFromFS(fs.FS(mfs), "addons")
}

// --- tests that drive the cobra wrappers via a fake installer ------------

// TestRunAddonInstall_HappyPath_FakeInstaller verifies that RunAddonInstall
// delegates to the injected installer and prints a one-line confirmation
// containing both the addon name and the cluster name.
func TestRunAddonInstall_HappyPath_FakeInstaller(t *testing.T) {
	fi := &fakeInstaller{}
	var buf bytes.Buffer
	deps := cmd.AddonCmdDeps{Installer: fi}

	if err := cmd.RunAddonInstall(context.Background(), "demo", "alpha", "", &buf, deps); err != nil {
		t.Fatalf("RunAddonInstall: %v", err)
	}
	if got := len(fi.installCalls); got != 1 {
		t.Fatalf("install calls: want 1, got %d", got)
	}
	if fi.installCalls[0] != (installCall{"demo", "alpha", ""}) {
		t.Errorf("install call: got %+v", fi.installCalls[0])
	}
	out := buf.String()
	if !strings.Contains(out, "demo") || !strings.Contains(out, "alpha") {
		t.Errorf("expected addon and cluster in output, got %q", out)
	}
	if !strings.Contains(out, "installed") {
		t.Errorf("expected confirmation verb in output, got %q", out)
	}
}

// TestRunAddonInstall_PropagatesInstallerError verifies that installer
// failures surface verbatim in the error chain.
func TestRunAddonInstall_PropagatesInstallerError(t *testing.T) {
	want := errors.New("kubectl apply boom")
	fi := &fakeInstaller{installErr: want}
	var buf bytes.Buffer
	deps := cmd.AddonCmdDeps{Installer: fi}

	err := cmd.RunAddonInstall(context.Background(), "demo", "alpha", "", &buf, deps)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, want) {
		t.Errorf("error chain: want wraps %v, got %v", want, err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no success output on failure, got %q", buf.String())
	}
}

// TestRunAddonInstall_ExplicitClusterUsed verifies that an explicit --cluster
// value is passed through to the installer unchanged.
func TestRunAddonInstall_ExplicitClusterUsed(t *testing.T) {
	fi := &fakeInstaller{}
	var buf bytes.Buffer
	deps := cmd.AddonCmdDeps{Installer: fi}

	err := cmd.RunAddonInstall(context.Background(), "demo", "mycluster", "", &buf, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fi.installCalls) != 1 || fi.installCalls[0].cluster != "mycluster" {
		t.Errorf("expected installer called with cluster %q, got %+v", "mycluster", fi.installCalls)
	}
}

// --- end-to-end tests that drive the real Installer through the wrappers --

// TestRunAddonInstall_EndToEnd_RegistryRowWritten exercises the full pipeline
// against a custom catalog, an on-disk sqlite registry, and a stub kubectl
// runner. It asserts that on success a deployments row exists with the
// catalog version and that kubectl apply was invoked.
func TestRunAddonInstall_EndToEnd_RegistryRowWritten(t *testing.T) {
	env := newAddonTestEnv(t)

	var buf bytes.Buffer
	if err := cmd.RunAddonInstall(context.Background(), "demo", "alpha", "", &buf, env.deps); err != nil {
		t.Fatalf("RunAddonInstall: %v", err)
	}
	if !strings.Contains(buf.String(), "v1.0.0") {
		t.Errorf("expected version in success output, got %q", buf.String())
	}

	reg := env.reopenRegistry()
	d, err := reg.GetDeployment(context.Background(), "alpha", "demo")
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
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

	var applied bool
	for _, c := range env.runner.calls {
		for _, a := range c.args {
			if a == "apply" {
				applied = true
			}
		}
	}
	if !applied {
		t.Errorf("expected at least one kubectl apply call, got %+v", env.runner.calls)
	}
}

// TestRunAddonInstall_EndToEnd_KubectlFailure_NoRegistryRow verifies that a
// kubectl apply failure leaves the deployments row absent and the original
// error reaches the caller through the error chain.
func TestRunAddonInstall_EndToEnd_KubectlFailure_NoRegistryRow(t *testing.T) {
	env := newAddonTestEnv(t)
	wantErr := errors.New("simulated kubectl apply failure")
	env.runner.errOnVerb = "apply"
	env.runner.err = wantErr

	var buf bytes.Buffer
	err := cmd.RunAddonInstall(context.Background(), "demo", "alpha", "", &buf, env.deps)
	if err == nil {
		t.Fatal("expected error from kubectl failure, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error chain: want wraps %v, got %v", wantErr, err)
	}

	reg := env.reopenRegistry()
	if _, err := reg.GetDeployment(context.Background(), "alpha", "demo"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("deployments row should be absent on failure; GetDeployment err=%v", err)
	}
}
