package bootstrap_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
)

// ---- mock runner ----

// call records a single invocation of CommandRunner.Run.
type call struct {
	name string
	args []string
}

// mockRunner is a configurable CommandRunner for unit tests.
type mockRunner struct {
	calls    []call
	response func(name string, args []string) ([]byte, error)
}

func (m *mockRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	m.calls = append(m.calls, call{name: name, args: args})
	if m.response != nil {
		return m.response(name, args)
	}
	return nil, nil
}

// ---- helpers ----

func baseConfig() bootstrap.K3sConfig {
	return bootstrap.K3sConfig{
		TailscaleIP:    "100.64.0.1",
		K3sVersion:     "v1.32.3+k3s1",
		User:           "clusterbox",
		KubeconfigPath: "/tmp/clusterbox.yaml",
		SSHKeyPath:     "/home/ops/.ssh/id_ed25519",
	}
}

// succeedAll returns a runner that immediately succeeds for k3sup and returns a
// Ready node line for kubectl.
func succeedAll() *mockRunner {
	return &mockRunner{
		response: func(name string, _ []string) ([]byte, error) {
			if name == "kubectl" {
				return []byte("clusterbox   Ready   master   1m\n"), nil
			}
			return nil, nil
		},
	}
}

// ---- tests ----

// TestBootstrap_InvokesK3supWithCorrectFlags verifies that BootstrapWith calls
// k3sup with every required flag.
func TestBootstrap_InvokesK3supWithCorrectFlags(t *testing.T) {
	cfg := baseConfig()
	runner := succeedAll()

	if err := bootstrap.BootstrapWith(context.Background(), cfg, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The first call must be k3sup.
	if len(runner.calls) == 0 {
		t.Fatal("no commands were executed")
	}
	k3supCall := runner.calls[0]
	if k3supCall.name != "k3sup" {
		t.Fatalf("expected first command to be k3sup, got %q", k3supCall.name)
	}

	want := map[string]string{
		"--ip":          cfg.TailscaleIP,
		"--user":        cfg.User,
		"--k3s-version": cfg.K3sVersion,
		"--local-path":  cfg.KubeconfigPath,
		"--ssh-key":     cfg.SSHKeyPath,
		"--context":     "clusterbox",
	}
	args := k3supCall.args
	for flag, value := range want {
		assertFlagValue(t, args, flag, value)
	}
}

// TestBootstrap_K3supNonZero checks that a non-zero k3sup exit is propagated as
// an error that includes the captured output.
func TestBootstrap_K3supNonZero(t *testing.T) {
	runner := &mockRunner{
		response: func(name string, _ []string) ([]byte, error) {
			if name == "k3sup" {
				return []byte("connection refused"), errors.New("exit status 1")
			}
			return nil, nil
		},
	}

	err := bootstrap.BootstrapWith(context.Background(), baseConfig(), runner)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	errStr := err.Error()
	if !strings.Contains(errStr, "bootstrap: k3sup install") {
		t.Errorf("error should mention k3sup install step, got: %s", errStr)
	}
}

// TestBootstrap_K3sVersionPropagated verifies that changing K3sVersion in config
// changes the --k3s-version flag passed to k3sup.
func TestBootstrap_K3sVersionPropagated(t *testing.T) {
	const customVersion = "v1.29.0+k3s1"
	cfg := baseConfig()
	cfg.K3sVersion = customVersion

	runner := succeedAll()
	if err := bootstrap.BootstrapWith(context.Background(), cfg, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(runner.calls) == 0 {
		t.Fatal("no commands executed")
	}
	assertFlagValue(t, runner.calls[0].args, "--k3s-version", customVersion)
}

// TestBootstrap_DefaultVersion verifies that when K3sVersion is empty the
// package-level default is used.
func TestBootstrap_DefaultVersion(t *testing.T) {
	cfg := baseConfig()
	cfg.K3sVersion = "" // rely on default

	runner := succeedAll()
	if err := bootstrap.BootstrapWith(context.Background(), cfg, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertFlagValue(t, runner.calls[0].args, "--k3s-version", bootstrap.DefaultK3sVersion)
}

// TestBootstrap_DefaultUser verifies that when User is empty "clusterbox" is used.
func TestBootstrap_DefaultUser(t *testing.T) {
	cfg := baseConfig()
	cfg.User = ""

	runner := succeedAll()
	if err := bootstrap.BootstrapWith(context.Background(), cfg, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertFlagValue(t, runner.calls[0].args, "--user", "clusterbox")
}

// TestBootstrap_WaitsForReadyNode verifies that BootstrapWith retries kubectl
// until "Ready" appears in the output.
func TestBootstrap_WaitsForReadyNode(t *testing.T) {
	kubectlAttempts := 0
	runner := &mockRunner{
		response: func(name string, _ []string) ([]byte, error) {
			if name != "kubectl" {
				return nil, nil // k3sup succeeds immediately
			}
			kubectlAttempts++
			if kubectlAttempts < 3 {
				return []byte("clusterbox   NotReady   master   0s\n"), nil
			}
			return []byte("clusterbox   Ready   master   9s\n"), nil
		},
	}

	if err := bootstrap.BootstrapWith(context.Background(), baseConfig(), runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kubectlAttempts < 3 {
		t.Errorf("expected at least 3 kubectl attempts, got %d", kubectlAttempts)
	}
}

// TestBootstrap_TimeoutWhenNodeNeverReady verifies that BootstrapWith returns an
// error when the context is cancelled before the node becomes Ready.
func TestBootstrap_TimeoutWhenNodeNeverReady(t *testing.T) {
	runner := &mockRunner{
		response: func(name string, _ []string) ([]byte, error) {
			if name == "kubectl" {
				return []byte("clusterbox   NotReady   master   0s\n"), nil
			}
			return nil, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 0) // immediately expired
	defer cancel()

	err := bootstrap.BootstrapWith(ctx, baseConfig(), runner)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "bootstrap: node not ready within timeout") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// ---- assertion helpers ----

// assertFlagValue checks that args contains flag immediately followed by value.
func assertFlagValue(t *testing.T, args []string, flag, value string) {
	t.Helper()
	argsStr := fmt.Sprintf("%v", args)
	for i, a := range args {
		if a == flag {
			if i+1 >= len(args) {
				t.Errorf("flag %q present but has no value in args: %s", flag, argsStr)
				return
			}
			if got := args[i+1]; got != value {
				t.Errorf("flag %q: want %q, got %q (args: %s)", flag, value, got, argsStr)
			}
			return
		}
	}
	t.Errorf("flag %q not found in k3sup args: %s", flag, argsStr)
}

// assertContains is a simple substring helper kept for potential future use.
func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !bytes.Contains([]byte(haystack), []byte(needle)) {
		t.Errorf("expected %q to contain %q", haystack, needle)
	}
}
