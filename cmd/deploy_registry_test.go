package cmd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/secrets"
)

// deployFakeRegistry records what the deploy command writes. Only the methods
// needed by recordDeploySuccess / recordDeployFailure are non-trivial; the
// rest panic so accidental usage shows up immediately.
type deployFakeRegistry struct {
	mu sync.Mutex

	deployments []registry.Deployment
	history     []registry.DeploymentHistoryEntry
	closed      bool

	upsertDeployErr error
	appendHistErr   error
	closeErr        error
}

func (f *deployFakeRegistry) UpsertDeployment(_ context.Context, d registry.Deployment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertDeployErr != nil {
		return f.upsertDeployErr
	}
	f.deployments = append(f.deployments, d)
	return nil
}

func (f *deployFakeRegistry) AppendHistory(_ context.Context, e registry.DeploymentHistoryEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendHistErr != nil {
		return f.appendHistErr
	}
	f.history = append(f.history, e)
	return nil
}

func (f *deployFakeRegistry) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return f.closeErr
}

// Unused interface methods for the deploy fake.
func (f *deployFakeRegistry) UpsertCluster(context.Context, registry.Cluster) error {
	panic("not used")
}
func (f *deployFakeRegistry) GetCluster(context.Context, string) (registry.Cluster, error) {
	panic("not used")
}
func (f *deployFakeRegistry) ListClusters(context.Context) ([]registry.Cluster, error) {
	panic("not used")
}
func (f *deployFakeRegistry) DeleteCluster(context.Context, string) error { panic("not used") }
func (f *deployFakeRegistry) UpsertNode(context.Context, registry.Node) error {
	panic("not used")
}
func (f *deployFakeRegistry) RemoveNode(context.Context, string, string) error {
	panic("not used")
}
func (f *deployFakeRegistry) ListNodes(context.Context, string) ([]registry.Node, error) {
	panic("not used")
}
func (f *deployFakeRegistry) GetDeployment(context.Context, string, string) (registry.Deployment, error) {
	panic("not used")
}
func (f *deployFakeRegistry) ListDeployments(context.Context, string) ([]registry.Deployment, error) {
	panic("not used")
}
func (f *deployFakeRegistry) ListHistory(context.Context, registry.HistoryFilter) ([]registry.DeploymentHistoryEntry, error) {
	panic("not used")
}
func (f *deployFakeRegistry) MarkSynced(context.Context, string, time.Time) error {
	panic("not used")
}

// minimalDeployDeps wires up the dependency injection for a deploy run with a
// fake registry plus the standard noop manifest fetcher / resolver / runner.
// The runner implements secrets.CommandRunner.
type noopRunner struct{}

func (noopRunner) Run(context.Context, string, ...string) ([]byte, error) { return nil, nil }

type noopResolver struct{}

func (noopResolver) Resolve(context.Context, string, string, string, string) (map[string]string, error) {
	return map[string]string{}, nil
}

func depsWithFake(fake registry.Registry) DeployDeps {
	return DeployDeps{
		FetchManifest: func(context.Context, string, string, string, string) ([]byte, error) {
			return []byte("apiVersion: v1\n"), nil
		},
		SecretsResolver: noopResolver{},
		Runner:          noopRunner{},
		OpenRegistry: func(context.Context) (registry.Registry, error) {
			return fake, nil
		},
	}
}

// TestRunDeploy_Success_RecordsDeploymentAndHistory verifies that a successful
// deploy upserts the deployments row AND appends a rolled_out history entry
// with a non-empty rollout duration and an empty error.
func TestRunDeploy_Success_RecordsDeploymentAndHistory(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("USER", "alice")

	fake := &deployFakeRegistry{}

	err := RunDeploy(context.Background(), "myservice", "v1.2.3", "hetzner-ash", "prod", depsWithFake(fake))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !fake.closed {
		t.Errorf("registry was not closed")
	}

	if got := len(fake.deployments); got != 1 {
		t.Fatalf("expected 1 deployment row, got %d", got)
	}
	d := fake.deployments[0]
	if d.ClusterName != "hetzner-ash" || d.Service != "myservice" || d.Version != "v1.2.3" {
		t.Errorf("deployment row mismatch: %+v", d)
	}
	if d.DeployedBy != "alice" {
		t.Errorf("DeployedBy: want alice, got %q", d.DeployedBy)
	}
	if d.Status != registry.StatusRolledOut {
		t.Errorf("Status: want rolled_out, got %q", d.Status)
	}
	if d.DeployedAt.IsZero() || d.DeployedAt.Location() != time.UTC {
		t.Errorf("DeployedAt must be set and UTC, got %v (loc=%v)", d.DeployedAt, d.DeployedAt.Location())
	}

	if got := len(fake.history); got != 1 {
		t.Fatalf("expected 1 history row, got %d", got)
	}
	h := fake.history[0]
	if h.Status != registry.StatusRolledOut {
		t.Errorf("history Status: want rolled_out, got %q", h.Status)
	}
	if h.Error != "" {
		t.Errorf("history Error must be empty on success, got %q", h.Error)
	}
	if h.AttemptedAt.IsZero() || h.AttemptedAt.Location() != time.UTC {
		t.Errorf("AttemptedAt must be set and UTC, got %v (loc=%v)", h.AttemptedAt, h.AttemptedAt.Location())
	}
	if h.RolloutDurationMs < 0 {
		t.Errorf("RolloutDurationMs must be non-negative, got %d", h.RolloutDurationMs)
	}
}

// TestRunDeploy_Failure_AppendsFailedHistoryOnly verifies that when a deploy
// step fails, only a failed history row is written — the deployments table
// must remain untouched so it still reflects the last good rollout.
func TestRunDeploy_Failure_AppendsFailedHistoryOnly(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")

	fake := &deployFakeRegistry{}
	deps := depsWithFake(fake)
	deps.FetchManifest = func(context.Context, string, string, string, string) ([]byte, error) {
		return nil, errors.New("manifest 404")
	}

	err := RunDeploy(context.Background(), "myservice", "v9.9.9", "hetzner-ash", "prod", deps)
	if err == nil {
		t.Fatal("expected error from failing FetchManifest, got nil")
	}
	if !strings.Contains(err.Error(), "manifest 404") {
		t.Errorf("error must wrap underlying cause, got %v", err)
	}

	if got := len(fake.deployments); got != 0 {
		t.Errorf("deployments table must NOT be updated on failure, got %d rows", got)
	}

	if got := len(fake.history); got != 1 {
		t.Fatalf("expected 1 history row, got %d", got)
	}
	h := fake.history[0]
	if h.Status != registry.StatusFailed {
		t.Errorf("history Status: want failed, got %q", h.Status)
	}
	if !strings.Contains(h.Error, "manifest 404") {
		t.Errorf("history Error must contain underlying cause, got %q", h.Error)
	}
	if h.ClusterName != "hetzner-ash" || h.Service != "myservice" || h.Version != "v9.9.9" {
		t.Errorf("history identifiers mismatch: %+v", h)
	}
	if h.RolloutDurationMs < 0 {
		t.Errorf("RolloutDurationMs must be non-negative, got %d", h.RolloutDurationMs)
	}
}

// TestRunDeploy_RegistryOpenFailure_PreservesDeployError verifies that a
// registry open error during a successful deploy logs a warning but does NOT
// turn a successful deploy into a failure.
func TestRunDeploy_RegistryOpenFailure_PreservesDeployError(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")

	deps := DeployDeps{
		FetchManifest: func(context.Context, string, string, string, string) ([]byte, error) {
			return []byte("apiVersion: v1\n"), nil
		},
		SecretsResolver: noopResolver{},
		Runner:          noopRunner{},
		OpenRegistry: func(context.Context) (registry.Registry, error) {
			return nil, errors.New("disk on fire")
		},
	}

	stderr := captureStderr(t, func() {
		if err := RunDeploy(context.Background(), "myservice", "v1.0.0", "hetzner-ash", "prod", deps); err != nil {
			t.Fatalf("registry open failure must NOT turn into deploy failure, got %v", err)
		}
	})

	if !strings.Contains(stderr, "warning: registry write failed") {
		t.Errorf("expected warning on stderr, got %q", stderr)
	}
	if !strings.Contains(stderr, "disk on fire") {
		t.Errorf("expected underlying error in warning, got %q", stderr)
	}
}

// TestRunDeploy_RegistryOpenFailure_OnDeployFailure_PreservesDeployError
// verifies that even if the registry can't be opened for the failure-history
// write, the deploy still returns the original underlying error (never the
// registry error).
func TestRunDeploy_RegistryOpenFailure_OnDeployFailure_PreservesDeployError(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")

	deployErr := errors.New("manifest 404")
	deps := DeployDeps{
		FetchManifest: func(context.Context, string, string, string, string) ([]byte, error) {
			return nil, deployErr
		},
		SecretsResolver: noopResolver{},
		Runner:          noopRunner{},
		OpenRegistry: func(context.Context) (registry.Registry, error) {
			return nil, errors.New("disk on fire")
		},
	}

	stderr := captureStderr(t, func() {
		err := RunDeploy(context.Background(), "myservice", "v1.0.0", "hetzner-ash", "prod", deps)
		if err == nil {
			t.Fatal("expected deploy error, got nil")
		}
		if !strings.Contains(err.Error(), "manifest 404") {
			t.Errorf("returned error must be the deploy error, got %v", err)
		}
		if strings.Contains(err.Error(), "disk on fire") {
			t.Errorf("registry error must not leak into the returned error, got %v", err)
		}
	})

	if !strings.Contains(stderr, "warning: registry write failed") {
		t.Errorf("expected warning on stderr, got %q", stderr)
	}
}

// TestRunDeploy_AppendHistoryError_LogsWarning verifies that a history append
// error after a successful deploy is logged but does not flip the result.
func TestRunDeploy_AppendHistoryError_LogsWarning(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")

	fake := &deployFakeRegistry{appendHistErr: errors.New("history disk full")}

	stderr := captureStderr(t, func() {
		if err := RunDeploy(context.Background(), "myservice", "v1.0.0", "hetzner-ash", "prod", depsWithFake(fake)); err != nil {
			t.Fatalf("history failure must not break a successful deploy, got %v", err)
		}
	})

	if !strings.Contains(stderr, "history disk full") {
		t.Errorf("expected history error on stderr, got %q", stderr)
	}
	// Deployment row should still have been written.
	if got := len(fake.deployments); got != 1 {
		t.Errorf("deployment row should still be written when history fails, got %d", got)
	}
}

// TestCurrentUser_FallbackOrder verifies USER → LOGNAME → "unknown".
func TestCurrentUser_FallbackOrder(t *testing.T) {
	t.Setenv("USER", "alice")
	t.Setenv("LOGNAME", "bob")
	if got := currentUser(); got != "alice" {
		t.Errorf("USER takes precedence: want alice, got %q", got)
	}

	t.Setenv("USER", "")
	if got := currentUser(); got != "bob" {
		t.Errorf("LOGNAME fallback: want bob, got %q", got)
	}

	t.Setenv("LOGNAME", "")
	if got := currentUser(); got != "unknown" {
		t.Errorf("final fallback: want unknown, got %q", got)
	}
}

// Ensure secrets.Resolver / secrets.CommandRunner remain satisfied at compile
// time (defends against accidental signature drift when DeployDeps changes).
var (
	_ secrets.Resolver      = noopResolver{}
	_ secrets.CommandRunner = noopRunner{}
)
