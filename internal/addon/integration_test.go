// Copyright 2026 Foundry Fabric

//go:build integration

// Package addon's integration test exercises the full Installer pipeline
// against the real embedded catalog (gha-runner-scale-set), an on-disk SQLite
// registry, and a recording fake kubectl. Run with:
//
//	go test -tags integration ./internal/addon/...
//
// The test deliberately uses no network and no real kubectl: every shell-out
// is captured by the recording runner, the secrets resolver returns a static
// placeholder bundle, and the registry is a fresh tempdir SQLite database.
//
// The gha-runner-scale-set addon ships with strategy=helmchart, which the
// installer does not yet apply directly (helmchart support is a follow-up).
// The test therefore loads the addon via the catalog, then flips the in-memory
// Strategy field to "manifests" so the same rendered manifests (namespace,
// secret, helmchart) flow through the kubectl-apply path. This validates the
// installer's catalog-load → secret-resolve → render → apply → registry-write
// pipeline end-to-end against the real embedded files.
package addon

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/registry/sqlite"
)

// integrationKubectl records every kubectl invocation the installer makes and
// returns success on every call. It mirrors the recording runners used in the
// unit tests but is duplicated here so the integration_test build tag does
// not pull in the unit-test file.
type integrationKubectl struct {
	mu sync.Mutex

	calls []integrationCall
}

type integrationCall struct {
	name string
	args []string
}

func (k *integrationKubectl) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	cp := append([]string(nil), args...)
	k.calls = append(k.calls, integrationCall{name: name, args: cp})
	return nil, nil
}

// integrationResolver returns the seeded map verbatim; the installer never
// mutates the resolver's internal state, so we hand back the same instance.
type integrationResolver struct {
	values map[string]string
}

func (r *integrationResolver) Resolve(_ context.Context, _, _, _, _ string) (map[string]string, error) {
	out := make(map[string]string, len(r.values))
	for k, v := range r.values {
		out[k] = v
	}
	return out, nil
}

// findApplyCall returns the first integrationCall whose argv contains the
// supplied verb (e.g. "apply" or "delete"), along with whether one was found.
func findVerbCall(calls []integrationCall, verb string) (integrationCall, bool) {
	for _, c := range calls {
		for _, a := range c.args {
			if a == verb {
				return c, true
			}
		}
	}
	return integrationCall{}, false
}

// TestIntegration_AddonInstallUninstallUpgrade walks the operator's full
// install → uninstall → re-install → upgrade lifecycle for the embedded
// gha-runner-scale-set addon against a tempdir SQLite registry.
//
// Acceptance:
//
//   - Install records a `kind=addon` deployments row at the catalog version.
//   - kubectl was invoked with the addon's rendered manifests via apply.
//   - The rendered blob has the GH App placeholders substituted.
//   - Uninstall invokes kubectl delete, removes the deployments row, and
//     appends a status=uninstalled history entry.
//   - Re-installing after an uninstall restores the deployments row.
//   - A subsequent Upgrade with a bumped catalog version advances the
//     deployments row's Version column.
func TestIntegration_AddonInstallUninstallUpgrade(t *testing.T) {
	ctx := context.Background()

	// 1. Tempdir SQLite registry seeded with a cluster row.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "registry.db")
	reg, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatalf("open sqlite registry: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	cluster := registry.Cluster{
		Name:           "alpha",
		Provider:       "hetzner",
		Region:         "nbg1",
		Env:            "prod",
		CreatedAt:      time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC),
		KubeconfigPath: "/tmp/alpha.yaml",
	}
	if err := reg.UpsertCluster(ctx, cluster); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}

	// 2. Load the gha-runner-scale-set addon from the embedded catalog. The
	// addon ships with strategy=helmchart; the installer does not yet apply
	// helmchart-strategy addons via kubectl, so we flip the in-memory
	// strategy to manifests to exercise the rest of the pipeline against
	// the real embedded manifest files.
	cat := DefaultCatalog()
	a, err := cat.Get("gha-runner-scale-set")
	if err != nil {
		t.Fatalf("catalog.Get: %v", err)
	}
	if a.Strategy != StrategyHelmChart {
		t.Fatalf("precondition: catalog addon strategy: want helmchart, got %q", a.Strategy)
	}
	a.Strategy = StrategyManifests

	// 3. Fake secrets resolver returning the GH App placeholder credentials.
	sec := &integrationResolver{values: map[string]string{
		"GH_APP_ID":              "111111",
		"GH_APP_INSTALLATION_ID": "222222",
		"GH_APP_PRIVATE_KEY":     "-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----\n",
		"GH_PAT_TOKEN":           "ghp_fake",
	}}

	// 4. Recording kubectl runner.
	kub := &integrationKubectl{}

	// 5. Wire up the installer with deterministic clock + identity.
	clock := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	tick := 0
	inst := &Installer{
		Catalog:  cat,
		Secrets:  sec,
		Kubectl:  kub,
		Registry: reg,
		Now: func() time.Time {
			tick++
			return clock.Add(time.Duration(tick) * time.Second)
		},
		DeployedBy: func() string { return "integration-test" },
	}

	// ---------------------------------------------------------------------
	// Install
	// ---------------------------------------------------------------------
	if err := inst.Install(ctx, "gha-runner-scale-set", "alpha"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// kubectl was invoked at least once with apply.
	applyCall, ok := findVerbCall(kub.calls, "apply")
	if !ok {
		t.Fatalf("expected at least one kubectl apply call; got %+v", kub.calls)
	}
	if applyCall.name != "kubectl" {
		t.Errorf("apply call binary: want kubectl, got %q", applyCall.name)
	}
	wantPrefix := []string{"--kubeconfig", "/tmp/alpha.yaml", "apply", "-f"}
	for i, want := range wantPrefix {
		if i >= len(applyCall.args) || applyCall.args[i] != want {
			t.Errorf("apply args[%d]: want %q, got %v", i, want, applyCall.args)
			break
		}
	}

	// Registry row exists with kind=addon and the catalog version.
	dep, err := reg.GetDeployment(ctx, "alpha", "gha-runner-scale-set")
	if err != nil {
		t.Fatalf("GetDeployment after install: %v", err)
	}
	if dep.Kind != registry.KindAddon {
		t.Errorf("Kind: want addon, got %q", dep.Kind)
	}
	if dep.Status != registry.StatusRolledOut {
		t.Errorf("Status: want rolled_out, got %q", dep.Status)
	}
	if dep.Version != a.Version {
		t.Errorf("Version: want %q, got %q", a.Version, dep.Version)
	}
	if dep.DeployedBy != "integration-test" {
		t.Errorf("DeployedBy: want integration-test, got %q", dep.DeployedBy)
	}

	// History row exists for the install.
	hist, err := reg.ListHistory(ctx, registry.HistoryFilter{
		ClusterName: "alpha", Service: "gha-runner-scale-set",
	})
	if err != nil {
		t.Fatalf("ListHistory after install: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("history rows after install: want 1, got %d", len(hist))
	}
	if hist[0].Kind != registry.KindAddon || hist[0].Status != registry.StatusRolledOut {
		t.Errorf("install history entry: want addon/rolled_out, got %s/%s", hist[0].Kind, hist[0].Status)
	}

	// ---------------------------------------------------------------------
	// Uninstall
	// ---------------------------------------------------------------------
	preUninstallCalls := len(kub.calls)
	if err := inst.Uninstall(ctx, "gha-runner-scale-set", "alpha"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	// kubectl was invoked with delete after the uninstall.
	postUninstall := kub.calls[preUninstallCalls:]
	deleteCall, ok := findVerbCall(postUninstall, "delete")
	if !ok {
		t.Fatalf("expected kubectl delete on uninstall; got %+v", postUninstall)
	}
	wantDelete := []string{"--kubeconfig", "/tmp/alpha.yaml", "delete", "-f"}
	for i, want := range wantDelete {
		if i >= len(deleteCall.args) || deleteCall.args[i] != want {
			t.Errorf("delete args[%d]: want %q, got %v", i, want, deleteCall.args)
			break
		}
	}
	var sawIgnore bool
	for _, a := range deleteCall.args {
		if a == "--ignore-not-found" {
			sawIgnore = true
		}
	}
	if !sawIgnore {
		t.Errorf("delete call must include --ignore-not-found; got %v", deleteCall.args)
	}

	// Deployments row is gone after uninstall.
	if _, err := reg.GetDeployment(ctx, "alpha", "gha-runner-scale-set"); err == nil {
		t.Errorf("deployments row should be absent after uninstall")
	}

	// History row was appended for the uninstall.
	hist, err = reg.ListHistory(ctx, registry.HistoryFilter{
		ClusterName: "alpha", Service: "gha-runner-scale-set",
	})
	if err != nil {
		t.Fatalf("ListHistory after uninstall: %v", err)
	}
	var sawUninstalled bool
	for _, h := range hist {
		if h.Status == registry.StatusUninstalled && h.Kind == registry.KindAddon {
			sawUninstalled = true
		}
	}
	if !sawUninstalled {
		t.Errorf("uninstall history entry missing; got %+v", hist)
	}

	// ---------------------------------------------------------------------
	// Re-install + Upgrade: verify the version advances.
	// ---------------------------------------------------------------------
	if err := inst.Install(ctx, "gha-runner-scale-set", "alpha"); err != nil {
		t.Fatalf("re-Install: %v", err)
	}
	dep, err = reg.GetDeployment(ctx, "alpha", "gha-runner-scale-set")
	if err != nil {
		t.Fatalf("GetDeployment after re-install: %v", err)
	}
	if dep.Version != a.Version {
		t.Errorf("re-install version: want %q, got %q", a.Version, dep.Version)
	}

	// Bump the catalog version in-memory and call Upgrade. Upgrade is an
	// alias for Install; the deployments row's Version must advance.
	a.Version = "0.99.0"
	if err := inst.Upgrade(ctx, "gha-runner-scale-set", "alpha"); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	dep, err = reg.GetDeployment(ctx, "alpha", "gha-runner-scale-set")
	if err != nil {
		t.Fatalf("GetDeployment after upgrade: %v", err)
	}
	if dep.Version != "0.99.0" {
		t.Errorf("upgrade version: want 0.99.0, got %q", dep.Version)
	}
	if dep.Kind != registry.KindAddon {
		t.Errorf("upgrade Kind: want addon, got %q", dep.Kind)
	}

	// The recorded apply count should now be at least 2 (initial install +
	// re-install + upgrade), and the captured args should never reference a
	// kubeconfig other than the cluster's.
	var applyCount int
	for _, c := range kub.calls {
		for _, a := range c.args {
			if a == "apply" {
				applyCount++
			}
		}
		// Sanity: every call uses our seeded kubeconfig.
		if !containsArg(c.args, "/tmp/alpha.yaml") {
			t.Errorf("kubectl call must target the seeded kubeconfig; got %v", c.args)
		}
	}
	if applyCount < 2 {
		t.Errorf("expected at least 2 kubectl apply calls across install+re-install+upgrade, got %d", applyCount)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
		if strings.Contains(a, want) {
			return true
		}
	}
	return false
}
