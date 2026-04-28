package cmd_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/cmd"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/registry/sqlite"
)

// seedRunnerTestDB opens a fresh sqlite registry at dbPath, inserts a cluster
// row, and optionally seeds the gha-runner-scale-set addon deployment row.
// It returns the dbPath so callers can build an OpenRegistry factory from it.
func seedRunnerTestDB(t *testing.T, dbPath string, seedAddon bool) {
	t.Helper()
	reg, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatalf("open seed registry: %v", err)
	}
	defer func() { _ = reg.Close() }()

	if err := reg.UpsertCluster(context.Background(), registry.Cluster{
		Name:           "alpha",
		Provider:       "hetzner",
		Region:         "nbg1",
		Env:            "prod",
		KubeconfigPath: "/tmp/alpha.yaml",
	}); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}

	if seedAddon {
		if err := reg.UpsertDeployment(context.Background(), registry.Deployment{
			ClusterName: "alpha",
			Service:     "gha-runner-scale-set",
			Version:     "v0.9.3",
			Status:      registry.StatusRolledOut,
			Kind:        registry.KindAddon,
		}); err != nil {
			t.Fatalf("seed addon deployment: %v", err)
		}
	}
}

func runnerDeps(t *testing.T, dbPath string, runner *fakeKubectlRunner) cmd.RunnerCmdDeps {
	t.Helper()
	return cmd.RunnerCmdDeps{
		OpenRegistry: func(_ context.Context) (registry.Registry, error) { return sqlite.New(dbPath) },
		Runner:       runner,
	}
}

// TestRunnerAdd_AddonNotInstalled verifies that runner add fails with a helpful
// message when the gha-runner-scale-set addon is not present.
func TestRunnerAdd_AddonNotInstalled(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "r.db")
	seedRunnerTestDB(t, dbPath, false)

	runner := &fakeKubectlRunner{}
	var buf bytes.Buffer
	err := cmd.RunRunnerAdd(context.Background(), "my-runner", "FoundryFabric/clusterbox", "alpha", 0, 4, &buf, runnerDeps(t, dbPath, runner))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "gha-runner-scale-set") {
		t.Errorf("error should mention gha-runner-scale-set, got: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("kubectl should not be called, got %d calls", len(runner.calls))
	}
}

// TestRunnerAdd_HappyPath verifies that runner add calls kubectl apply and
// writes a registry row with Kind=runner-scale-set.
func TestRunnerAdd_HappyPath(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "r.db")
	seedRunnerTestDB(t, dbPath, true)

	runner := &fakeKubectlRunner{}
	var buf bytes.Buffer
	err := cmd.RunRunnerAdd(context.Background(), "my-runner", "https://github.com/FoundryFabric/clusterbox", "alpha", 0, 4, &buf, runnerDeps(t, dbPath, runner))
	if err != nil {
		t.Fatalf("RunRunnerAdd: %v", err)
	}

	var applied bool
	for _, c := range runner.calls {
		for _, a := range c.args {
			if a == "apply" {
				applied = true
			}
		}
	}
	if !applied {
		t.Errorf("expected kubectl apply call, got %+v", runner.calls)
	}

	reg, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatalf("reopen registry: %v", err)
	}
	defer func() { _ = reg.Close() }()

	d, err := reg.GetDeployment(context.Background(), "alpha", "my-runner")
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if d.Kind != registry.KindRunnerScaleSet {
		t.Errorf("Kind: want runner-scale-set, got %q", d.Kind)
	}
	if d.Status != registry.StatusRolledOut {
		t.Errorf("Status: want rolled_out, got %q", d.Status)
	}

	out := buf.String()
	if !strings.Contains(out, "my-runner") {
		t.Errorf("expected name in output, got %q", out)
	}
}

// TestRunnerAdd_NormalisesRepo verifies that a bare org/repo slug is expanded
// to a full https://github.com/ URL in the manifest applied via kubectl.
func TestRunnerAdd_NormalisesRepo(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "r.db")
	seedRunnerTestDB(t, dbPath, true)

	capturing := &captureFileRunner{inner: &fakeKubectlRunner{}}

	var buf bytes.Buffer
	err := cmd.RunRunnerAdd(context.Background(), "ci-runner", "FoundryFabric/clusterbox", "alpha", 0, 4, &buf, cmd.RunnerCmdDeps{
		OpenRegistry: func(_ context.Context) (registry.Registry, error) { return sqlite.New(dbPath) },
		Runner:       capturing,
	})
	if err != nil {
		t.Fatalf("RunRunnerAdd: %v", err)
	}

	// The Version stored in the registry must be the full URL.
	reg, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatalf("reopen registry: %v", err)
	}
	defer func() { _ = reg.Close() }()

	d, err := reg.GetDeployment(context.Background(), "alpha", "ci-runner")
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	want := "https://github.com/FoundryFabric/clusterbox"
	if d.Version != want {
		t.Errorf("Version: want %q, got %q", want, d.Version)
	}

	// The YAML that was applied must also contain the full URL.
	applied := capturing.manifestContent
	if !strings.Contains(applied, want) {
		t.Errorf("applied manifest should contain %q, got:\n%s", want, applied)
	}
}

// captureFileRunner wraps fakeKubectlRunner and, when kubectl apply -f <path>
// is seen, reads the file content before the temp file is removed.
type captureFileRunner struct {
	inner           *fakeKubectlRunner
	manifestContent string
}

func (c *captureFileRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	// Look for apply -f <path> to capture the manifest.
	for i, a := range args {
		if a == "-f" && i+1 < len(args) {
			data, _ := readFileOnce(args[i+1])
			c.manifestContent = string(data)
		}
	}
	return c.inner.Run(ctx, name, args...)
}

func readFileOnce(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 8192)
	n, _ := f.Read(buf)
	return buf[:n], nil
}

// TestRunnerList_Empty verifies that list prints a "no runner" message when no
// scale sets are registered.
func TestRunnerList_Empty(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "r.db")
	seedRunnerTestDB(t, dbPath, true)

	var buf bytes.Buffer
	err := cmd.RunRunnerList(context.Background(), "alpha", &buf, cmd.RunnerCmdDeps{
		OpenRegistry: func(_ context.Context) (registry.Registry, error) { return sqlite.New(dbPath) },
	})
	if err != nil {
		t.Fatalf("RunRunnerList: %v", err)
	}
	if !strings.Contains(buf.String(), "no runner") {
		t.Errorf("expected 'no runner' in output, got %q", buf.String())
	}
}

// TestRunnerList_ShowsScaleSets verifies that a seeded scale-set row appears
// in runner list output.
func TestRunnerList_ShowsScaleSets(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "r.db")
	seedRunnerTestDB(t, dbPath, true)

	// Seed a runner scale set row directly.
	{
		reg, err := sqlite.New(dbPath)
		if err != nil {
			t.Fatalf("open registry for seed: %v", err)
		}
		if err := reg.UpsertDeployment(context.Background(), registry.Deployment{
			ClusterName: "alpha",
			Service:     "my-runner",
			Version:     "https://github.com/FoundryFabric/clusterbox",
			Status:      registry.StatusRolledOut,
			Kind:        registry.KindRunnerScaleSet,
		}); err != nil {
			t.Fatalf("seed runner: %v", err)
		}
		_ = reg.Close()
	}

	var buf bytes.Buffer
	err := cmd.RunRunnerList(context.Background(), "alpha", &buf, cmd.RunnerCmdDeps{
		OpenRegistry: func(_ context.Context) (registry.Registry, error) { return sqlite.New(dbPath) },
	})
	if err != nil {
		t.Fatalf("RunRunnerList: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "my-runner") {
		t.Errorf("expected my-runner in output, got %q", out)
	}
	if !strings.Contains(out, "https://github.com/FoundryFabric/clusterbox") {
		t.Errorf("expected repo URL in output, got %q", out)
	}
}

// TestRunnerRemove_AddonNotInstalled verifies that remove fails with a helpful
// message when the addon is absent.
func TestRunnerRemove_AddonNotInstalled(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "r.db")
	seedRunnerTestDB(t, dbPath, false)

	runner := &fakeKubectlRunner{}
	var buf bytes.Buffer
	err := cmd.RunRunnerRemove(context.Background(), "my-runner", "alpha", &buf, runnerDeps(t, dbPath, runner))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "gha-runner-scale-set") {
		t.Errorf("error should mention gha-runner-scale-set, got: %v", err)
	}
}

// TestRunnerRemove_HappyPath verifies that remove calls kubectl delete, then
// removes the registry row.
func TestRunnerRemove_HappyPath(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "r.db")
	seedRunnerTestDB(t, dbPath, true)

	// Seed a runner scale set row.
	{
		reg, err := sqlite.New(dbPath)
		if err != nil {
			t.Fatalf("open registry: %v", err)
		}
		if err := reg.UpsertDeployment(context.Background(), registry.Deployment{
			ClusterName: "alpha",
			Service:     "my-runner",
			Version:     "https://github.com/FoundryFabric/clusterbox",
			Status:      registry.StatusRolledOut,
			Kind:        registry.KindRunnerScaleSet,
		}); err != nil {
			t.Fatalf("seed runner: %v", err)
		}
		_ = reg.Close()
	}

	runner := &fakeKubectlRunner{}
	var buf bytes.Buffer
	err := cmd.RunRunnerRemove(context.Background(), "my-runner", "alpha", &buf, runnerDeps(t, dbPath, runner))
	if err != nil {
		t.Fatalf("RunRunnerRemove: %v", err)
	}

	var deleted bool
	for _, c := range runner.calls {
		for _, a := range c.args {
			if a == "delete" {
				deleted = true
			}
		}
	}
	if !deleted {
		t.Errorf("expected kubectl delete call, got %+v", runner.calls)
	}

	reg, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatalf("reopen registry: %v", err)
	}
	defer func() { _ = reg.Close() }()

	if _, err := reg.GetDeployment(context.Background(), "alpha", "my-runner"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("expected ErrNotFound after remove, got %v", err)
	}
}
