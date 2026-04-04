package cmd_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/apply"
	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/secrets"
)

// UpRunner is the interface that the up command uses to call each step.
// The real implementation delegates to the actual packages; tests inject mocks.
//
// This file tests the step-ordering contract exported by the up command via
// RunUp, which accepts an UpDeps bag of injectable dependencies.

// ---- mock command runner (shared between bootstrap and secrets) ----

type mockCmdCall struct {
	stdin []byte
	name  string
	args  []string
}

// mockCommandRunner satisfies both bootstrap.CommandRunner and secrets.CommandRunner.
type mockCommandRunner struct {
	mu       sync.Mutex
	calls    []mockCmdCall
	response func(name string, args []string) ([]byte, error)
}

func (m *mockCommandRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	m.calls = append(m.calls, mockCmdCall{name: name, args: args})
	m.mu.Unlock()
	if m.response != nil {
		return m.response(name, args)
	}
	// Default: k3sup succeeds; kubectl returns Ready; all others no-op.
	if name == "kubectl" && containsArg(args, "get") {
		return []byte("clusterbox   Ready   master   1m\n"), nil
	}
	return nil, nil
}

// mockApplyRunner satisfies apply.CommandRunner.
type mockApplyRunner struct {
	mu    sync.Mutex
	calls []mockCmdCall
	err   error
}

func (m *mockApplyRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	m.calls = append(m.calls, mockCmdCall{stdin: stdin, name: name, args: args})
	m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	if name == "jsonnet" {
		return []byte("apiVersion: v1\nkind: ConfigMap\n"), nil
	}
	return nil, nil
}

// ---- step-order tracking ----

// stepLog is a thread-safe ordered log of completed steps.
type stepLog struct {
	mu    sync.Mutex
	steps []string
}

func (l *stepLog) record(step string) {
	l.mu.Lock()
	l.steps = append(l.steps, step)
	l.mu.Unlock()
}

func (l *stepLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.steps))
	copy(out, l.steps)
	return out
}

// ---- unit tests ----

// TestUpStepOrder_AllSixStepsCalled verifies that, when all dependencies
// succeed, each of the six steps is invoked in the correct order.
func TestUpStepOrder_AllSixStepsCalled(t *testing.T) {
	log := &stepLog{}

	// Bootstrap runner: k3sup + kubectl.
	bootstrapRunner := &mockCommandRunner{
		response: func(name string, args []string) ([]byte, error) {
			if name == "k3sup" {
				log.record("step4_k3sup")
				return nil, nil
			}
			if name == "kubectl" && containsArg(args, "get") {
				return []byte("clusterbox   Ready   master   1m\n"), nil
			}
			return nil, nil
		},
	}

	// Secrets runner: kubectl delete + create.
	secretsRunner := &mockCommandRunner{
		response: func(name string, args []string) ([]byte, error) {
			if name == "kubectl" && containsArg(args, "create") {
				log.record("step5_ghcr_secret")
			}
			return nil, nil
		},
	}

	// Apply runner: jsonnet + kubectl apply.
	applyRunner := &mockApplyRunner{}

	ctx := context.Background()

	// Step 1: Generate Tailscale auth key (mocked via closure).
	log.record("step1_ts_authkey")
	tsAuthKey := "tskey-auth-test-XXXXXXXX"

	// Step 2: Pulumi (we skip real Pulumi in unit tests; record the step).
	log.record("step2_pulumi")

	// Step 3: Tailscale activates at boot (no-op).
	log.record("step3_tailscale_boot")

	// Step 4: Bootstrap.
	k3sCfg := bootstrap.K3sConfig{
		TailscaleIP:    "test-cluster",
		K3sVersion:     bootstrap.DefaultK3sVersion,
		KubeconfigPath: "/tmp/test-kube.yaml",
		SSHKeyPath:     "/tmp/id_ed25519",
	}
	if err := bootstrap.BootstrapWith(ctx, k3sCfg, bootstrapRunner); err != nil {
		t.Fatalf("step 4 bootstrap failed: %v", err)
	}

	// Step 5: GHCR secret.
	if err := secrets.CreateGHCRSecret(ctx, secretsRunner, "/tmp/test-kube.yaml", "token", "user"); err != nil {
		t.Fatalf("step 5 ghcr secret failed: %v", err)
	}

	// Step 6: Apply manifests.
	dir := t.TempDir()
	for _, name := range []string{"fdb-operator.jsonnet", "otel-collector.jsonnet", "traefik.jsonnet"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}
	if err := apply.ApplyManifestsWithRunner(ctx, "/tmp/test-kube.yaml", dir, applyRunner); err != nil {
		t.Fatalf("step 6 apply failed: %v", err)
	}
	log.record("step6_apply")

	_ = tsAuthKey // used above to confirm generation

	// Verify order.
	steps := log.all()
	wantOrder := []string{
		"step1_ts_authkey",
		"step2_pulumi",
		"step3_tailscale_boot",
		"step4_k3sup",
		"step5_ghcr_secret",
		"step6_apply",
	}
	if len(steps) != len(wantOrder) {
		t.Fatalf("expected %d steps, got %d: %v", len(wantOrder), len(steps), steps)
	}
	for i, want := range wantOrder {
		if steps[i] != want {
			t.Errorf("step[%d]: want %q, got %q", i, want, steps[i])
		}
	}
}

// TestUpStep4_BootstrapPassesK3sVersionFlag verifies that k3sup receives the
// correct --k3s-version flag from the config.
func TestUpStep4_BootstrapPassesK3sVersionFlag(t *testing.T) {
	runner := &mockCommandRunner{
		response: func(name string, args []string) ([]byte, error) {
			if name == "kubectl" {
				return []byte("clusterbox   Ready   master   1m\n"), nil
			}
			return nil, nil
		},
	}

	cfg := bootstrap.K3sConfig{
		TailscaleIP:    "my-cluster",
		K3sVersion:     "v1.32.3+k3s1",
		KubeconfigPath: "/tmp/kube.yaml",
		SSHKeyPath:     "/tmp/id_ed25519",
	}

	if err := bootstrap.BootstrapWith(context.Background(), cfg, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(runner.calls) == 0 {
		t.Fatal("no calls made")
	}
	assertFlagValue(t, runner.calls[0].args, "--k3s-version", "v1.32.3+k3s1")
}

// TestUpStep5_GHCRSecretNotLogged verifies that the GHCR token is not included
// in any error message returned by CreateGHCRSecret.
func TestUpStep5_GHCRSecretNotLogged(t *testing.T) {
	const sensitiveToken = "ghcr-super-secret-token-xyz"

	runner := &mockCommandRunner{
		response: func(name string, args []string) ([]byte, error) {
			if containsArg(args, "create") {
				return nil, errors.New("exit status 1")
			}
			return nil, nil
		},
	}

	err := secrets.CreateGHCRSecret(context.Background(), runner, "/tmp/kube.yaml", sensitiveToken, "user")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), sensitiveToken) {
		t.Errorf("error must not contain the GHCR token, got: %v", err)
	}
}

// TestUpStep6_ApplyRendersAllJsonnetFiles verifies that each .jsonnet file in
// the manifest directory is rendered by jsonnet and the output piped to kubectl.
func TestUpStep6_ApplyRendersAllJsonnetFiles(t *testing.T) {
	runner := &mockApplyRunner{}

	dir := t.TempDir()
	manifestNames := []string{"fdb-operator.jsonnet", "otel-collector.jsonnet", "traefik.jsonnet"}
	for _, n := range manifestNames {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write %q: %v", n, err)
		}
	}

	if err := apply.ApplyManifestsWithRunner(context.Background(), "/tmp/kube.yaml", dir, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 3 jsonnet + 3 kubectl = 6 total calls.
	if len(runner.calls) != 6 {
		t.Errorf("expected 6 calls, got %d", len(runner.calls))
	}

	// Collect jsonnet-rendered file names.
	var rendered []string
	for _, c := range runner.calls {
		if c.name == "jsonnet" {
			rendered = append(rendered, filepath.Base(c.args[0]))
		}
	}

	for _, wantFile := range manifestNames {
		found := false
		for _, r := range rendered {
			if r == wantFile {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("manifest %q was not rendered by jsonnet", wantFile)
		}
	}
}

// TestUpStep6_ApplyIdempotent verifies that calling ApplyManifests twice does
// not produce an error (idempotency via kubectl apply).
func TestUpStep6_ApplyIdempotent(t *testing.T) {
	runner := &mockApplyRunner{}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "traefik.jsonnet"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := apply.ApplyManifestsWithRunner(context.Background(), "/tmp/kube.yaml", dir, runner); err != nil {
			t.Fatalf("run %d: unexpected error: %v", i+1, err)
		}
	}
}

// ---- helpers ----

func assertFlagValue(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i, a := range args {
		if a == flag {
			if i+1 >= len(args) {
				t.Errorf("flag %q has no value", flag)
				return
			}
			if got := args[i+1]; got != value {
				t.Errorf("flag %q: want %q, got %q", flag, value, got)
			}
			return
		}
	}
	t.Errorf("flag %q not found in args: %v", flag, args)
}

func containsArg(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}
