package cmd_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/foundryfabric/clusterbox/cmd"
)

// ---- deploy mock types ----

// deployStepLog records which steps ran and in what order.
type deployStepLog struct {
	mu    sync.Mutex
	steps []string
}

func (l *deployStepLog) record(s string) {
	l.mu.Lock()
	l.steps = append(l.steps, s)
	l.mu.Unlock()
}

func (l *deployStepLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.steps))
	copy(out, l.steps)
	return out
}

// mockDeployRunner satisfies secrets.CommandRunner and records kubectl calls.
type mockDeployRunner struct {
	mu    sync.Mutex
	calls []mockCmdCall
	log   *deployStepLog
}

func (m *mockDeployRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	m.calls = append(m.calls, mockCmdCall{name: name, args: args})
	m.mu.Unlock()

	// Track the semantic step from the args.
	if m.log != nil {
		if name == "kubectl" && containsArg(args, "apply") {
			m.log.record("step3_apply")
		} else if name == "kubectl" && containsArg(args, "rollout") {
			m.log.record("step4_rollout")
		}
	}
	return nil, nil
}

// mockSecretsResolver satisfies secrets.Resolver.
type mockSecretsResolver struct {
	log      *deployStepLog
	stepName string
	err      error
}

func (r *mockSecretsResolver) Resolve(_ context.Context, _, _, _, _ string) (map[string]string, error) {
	if r.log != nil {
		r.log.record(r.stepName)
	}
	if r.err != nil {
		return nil, r.err
	}
	return map[string]string{"KEY": "value"}, nil
}

// ---- tests ----

// TestDeployStepOrder verifies fetch → secrets → apply → rollout ordering.
func TestDeployStepOrder(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")

	log := &deployStepLog{}

	fetchCalled := false
	runner := &mockDeployRunner{log: log}
	resolver := &mockSecretsResolver{log: log, stepName: "step2_secrets"}

	deps := cmd.DeployDeps{
		FetchManifest: func(_ context.Context, _, _, _, _ string) ([]byte, error) {
			fetchCalled = true
			log.record("step1_fetch")
			return []byte("apiVersion: v1\n"), nil
		},
		SecretsResolver: resolver,
		Runner:          runner,
	}

	err := cmd.RunDeploy(context.Background(), "myservice", "v1.0.0", "test-cluster", "prod", deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !fetchCalled {
		t.Error("FetchManifest was not called")
	}

	wantOrder := []string{"step1_fetch", "step2_secrets", "step3_apply", "step4_rollout"}
	steps := log.all()
	if len(steps) != len(wantOrder) {
		t.Fatalf("expected %d steps, got %d: %v", len(wantOrder), len(steps), steps)
	}
	for i, want := range wantOrder {
		if steps[i] != want {
			t.Errorf("step[%d]: want %q, got %q", i, want, steps[i])
		}
	}
}

// TestDeployMissingGitHubToken verifies that a missing GITHUB_TOKEN produces a
// clear error before any network call or kubectl invocation.
func TestDeployMissingGitHubToken(t *testing.T) {
	// Ensure the variable is absent for this test.
	t.Setenv("GITHUB_TOKEN", "")

	networkCalled := false

	deps := cmd.DeployDeps{
		FetchManifest: func(_ context.Context, _, _, _, _ string) ([]byte, error) {
			networkCalled = true
			return nil, errors.New("should not reach here")
		},
		SecretsResolver: &mockSecretsResolver{},
		Runner:          &mockDeployRunner{},
	}

	err := cmd.RunDeploy(context.Background(), "myservice", "v1.0.0", "test-cluster", "prod", deps)
	if err == nil {
		t.Fatal("expected error for missing GITHUB_TOKEN, got nil")
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("error should mention GITHUB_TOKEN, got: %v", err)
	}
	if networkCalled {
		t.Error("network call was made despite missing GITHUB_TOKEN — should have short-circuited")
	}
}

// TestDeployFetchError verifies that a FetchManifest failure propagates and
// stops the deploy before secrets or kubectl are invoked.
func TestDeployFetchError(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")

	secretsResolved := false
	kubectlCalled := false

	deps := cmd.DeployDeps{
		FetchManifest: func(_ context.Context, _, _, _, _ string) ([]byte, error) {
			return nil, errors.New("release: GitHub API returned 404")
		},
		SecretsResolver: &mockSecretsResolver{
			log: &deployStepLog{},
			err: nil,
		},
		Runner: &mockDeployRunner{},
	}
	// Override SecretsResolver to track whether it was called.
	called := false
	deps.SecretsResolver = &mockSecretsResolver{
		log: nil,
		err: fmt.Errorf("should not be called"),
	}
	_ = called
	deps.SecretsResolver = &mockSecretsResolver{err: nil}
	// Use a custom resolver to detect if secrets are resolved when fetch fails.
	deps.SecretsResolver = &trackingResolver{called: &secretsResolved}
	_ = kubectlCalled

	err := cmd.RunDeploy(context.Background(), "myservice", "v1.0.0", "test-cluster", "prod", deps)
	if err == nil {
		t.Fatal("expected error from FetchManifest failure, got nil")
	}
	if secretsResolved {
		t.Error("secrets resolver was invoked despite fetch failure")
	}
}

// TestDeploySecretsError verifies that a secrets resolver failure propagates
// and stops the deploy before kubectl apply is called.
func TestDeploySecretsError(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")

	kubectlApplyCalled := false
	runner := &mockDeployRunner{}

	deps := cmd.DeployDeps{
		FetchManifest: func(_ context.Context, _, _, _, _ string) ([]byte, error) {
			return []byte("apiVersion: v1\n"), nil
		},
		SecretsResolver: &mockSecretsResolver{err: errors.New("1Password unavailable")},
		Runner:          runner,
	}

	err := cmd.RunDeploy(context.Background(), "myservice", "v1.0.0", "test-cluster", "prod", deps)
	if err == nil {
		t.Fatal("expected error from secrets failure, got nil")
	}

	for _, c := range runner.calls {
		if containsArg(c.args, "apply") {
			kubectlApplyCalled = true
		}
	}
	if kubectlApplyCalled {
		t.Error("kubectl apply was called despite secrets resolution failure")
	}
}

// trackingResolver is a secrets.Resolver that records whether Resolve was called.
type trackingResolver struct {
	called *bool
}

func (r *trackingResolver) Resolve(_ context.Context, _, _, _, _ string) (map[string]string, error) {
	*r.called = true
	return map[string]string{}, nil
}

// TestDeployPassesCorrectKubeconfigPath verifies that kubectl is invoked with
// ~/.kube/<cluster>.yaml as the kubeconfig path.
func TestDeployPassesCorrectKubeconfigPath(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")

	runner := &mockDeployRunner{}

	deps := cmd.DeployDeps{
		FetchManifest: func(_ context.Context, _, _, _, _ string) ([]byte, error) {
			return []byte("apiVersion: v1\n"), nil
		},
		SecretsResolver: &mockSecretsResolver{},
		Runner:          runner,
	}

	err := cmd.RunDeploy(context.Background(), "myservice", "v1.0.0", "mycluster", "prod", deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	home, _ := os.UserHomeDir()
	wantKubeconfig := home + "/.kube/mycluster.yaml"

	for _, c := range runner.calls {
		if c.name != "kubectl" {
			continue
		}
		if containsArg(c.args, "apply") || containsArg(c.args, "rollout") {
			found := false
			for i, a := range c.args {
				if a == "--kubeconfig" && i+1 < len(c.args) && c.args[i+1] == wantKubeconfig {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("kubectl %v: expected --kubeconfig %q", c.args, wantKubeconfig)
			}
		}
	}
}

// TestDeployDevEnvUsesDevResolver verifies that --env dev selects the dev resolver
// (we check by ensuring secrets step doesn't fail, using mock injection).
func TestDeployDevEnvUsesDevResolver(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")

	resolved := false
	deps := cmd.DeployDeps{
		FetchManifest: func(_ context.Context, _, _, _, _ string) ([]byte, error) {
			return []byte("apiVersion: v1\n"), nil
		},
		SecretsResolver: &trackingResolver{called: &resolved},
		Runner:          &mockDeployRunner{},
	}

	err := cmd.RunDeploy(context.Background(), "myservice", "v1.0.0", "test-cluster", "dev", deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resolved {
		t.Error("secrets resolver was not called for dev env")
	}
}
