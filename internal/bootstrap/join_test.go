package bootstrap_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
)

// baseJoinConfig returns a fully-populated JoinConfig suitable for unit tests.
func baseJoinConfig() bootstrap.JoinConfig {
	return bootstrap.JoinConfig{
		NodeIP:         "100.64.0.2",
		ServerIP:       "100.64.0.1",
		K3sVersion:     "v1.32.3+k3s1",
		User:           "clusterbox",
		KubeconfigPath: "/tmp/clusterbox.yaml",
		SSHKeyPath:     "/home/ops/.ssh/id_ed25519",
	}
}

// succeedAllJoin returns a runner that immediately succeeds for k3sup and
// returns a Ready node line for kubectl.
func succeedAllJoin() *mockRunner {
	return &mockRunner{
		response: func(name string, _ []string) ([]byte, error) {
			if name == "kubectl" {
				return []byte("worker-1   Ready   <none>   1m\n"), nil
			}
			return nil, nil
		},
	}
}

// TestJoin_InvokesK3supJoinWithCorrectFlags verifies that JoinWith calls
// k3sup join with all required flags and values.
func TestJoin_InvokesK3supJoinWithCorrectFlags(t *testing.T) {
	cfg := baseJoinConfig()
	runner := succeedAllJoin()

	if err := bootstrap.JoinWith(context.Background(), cfg, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(runner.calls) == 0 {
		t.Fatal("no commands were executed")
	}

	joinCall := runner.calls[0]
	if joinCall.name != "k3sup" {
		t.Fatalf("expected first command to be k3sup, got %q", joinCall.name)
	}

	// Verify the sub-command is "join".
	if len(joinCall.args) == 0 || joinCall.args[0] != "join" {
		t.Fatalf("expected first arg to be 'join', got %v", joinCall.args)
	}

	want := map[string]string{
		"--ip":          cfg.NodeIP,
		"--server-ip":   cfg.ServerIP,
		"--user":        cfg.User,
		"--k3s-version": cfg.K3sVersion,
		"--ssh-key":     cfg.SSHKeyPath,
	}
	for flag, value := range want {
		assertFlagValue(t, joinCall.args, flag, value)
	}
}

// TestJoin_ServerIPFlag verifies the --server-ip flag receives the control-plane IP.
func TestJoin_ServerIPFlag(t *testing.T) {
	cfg := baseJoinConfig()
	cfg.ServerIP = "100.64.1.100"

	runner := succeedAllJoin()
	if err := bootstrap.JoinWith(context.Background(), cfg, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertFlagValue(t, runner.calls[0].args, "--server-ip", "100.64.1.100")
}

// TestJoin_NodeIPFlag verifies the --ip flag receives the new node's IP.
func TestJoin_NodeIPFlag(t *testing.T) {
	cfg := baseJoinConfig()
	cfg.NodeIP = "100.64.0.99"

	runner := succeedAllJoin()
	if err := bootstrap.JoinWith(context.Background(), cfg, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertFlagValue(t, runner.calls[0].args, "--ip", "100.64.0.99")
}

// TestJoin_K3sVersionPropagated verifies that changing K3sVersion changes the
// --k3s-version flag passed to k3sup join.
func TestJoin_K3sVersionPropagated(t *testing.T) {
	const customVersion = "v1.29.0+k3s1"
	cfg := baseJoinConfig()
	cfg.K3sVersion = customVersion

	runner := succeedAllJoin()
	if err := bootstrap.JoinWith(context.Background(), cfg, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertFlagValue(t, runner.calls[0].args, "--k3s-version", customVersion)
}

// TestJoin_DefaultVersion verifies that when K3sVersion is empty the
// package-level default is used.
func TestJoin_DefaultVersion(t *testing.T) {
	cfg := baseJoinConfig()
	cfg.K3sVersion = ""

	runner := succeedAllJoin()
	if err := bootstrap.JoinWith(context.Background(), cfg, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertFlagValue(t, runner.calls[0].args, "--k3s-version", bootstrap.DefaultK3sVersion)
}

// TestJoin_DefaultUser verifies that when User is empty "clusterbox" is used.
func TestJoin_DefaultUser(t *testing.T) {
	cfg := baseJoinConfig()
	cfg.User = ""

	runner := succeedAllJoin()
	if err := bootstrap.JoinWith(context.Background(), cfg, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertFlagValue(t, runner.calls[0].args, "--user", "clusterbox")
}

// TestJoin_K3supNonZero checks that a non-zero k3sup exit is propagated as an
// error that names the join step.
func TestJoin_K3supNonZero(t *testing.T) {
	runner := &mockRunner{
		response: func(name string, _ []string) ([]byte, error) {
			if name == "k3sup" {
				return []byte("connection refused"), errors.New("exit status 1")
			}
			return nil, nil
		},
	}

	err := bootstrap.JoinWith(context.Background(), baseJoinConfig(), runner)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bootstrap: k3sup join") {
		t.Errorf("error should mention k3sup join step, got: %s", err.Error())
	}
}

// TestJoin_WaitsForReadyNode verifies that JoinWith retries kubectl until
// "Ready" appears in the output.
func TestJoin_WaitsForReadyNode(t *testing.T) {
	kubectlAttempts := 0
	runner := &mockRunner{
		response: func(name string, _ []string) ([]byte, error) {
			if name != "kubectl" {
				return nil, nil // k3sup succeeds immediately
			}
			kubectlAttempts++
			if kubectlAttempts < 3 {
				return []byte("worker-1   NotReady   <none>   0s\n"), nil
			}
			return []byte("worker-1   Ready   <none>   9s\n"), nil
		},
	}

	if err := bootstrap.JoinWith(context.Background(), baseJoinConfig(), runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kubectlAttempts < 3 {
		t.Errorf("expected at least 3 kubectl attempts, got %d", kubectlAttempts)
	}
}

// TestJoin_TimeoutWhenNodeNeverReady verifies that JoinWith returns an error
// when the context is cancelled before the joined node becomes Ready.
func TestJoin_TimeoutWhenNodeNeverReady(t *testing.T) {
	runner := &mockRunner{
		response: func(name string, _ []string) ([]byte, error) {
			if name == "kubectl" {
				return []byte("worker-1   NotReady   <none>   0s\n"), nil
			}
			return nil, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 0) // immediately expired
	defer cancel()

	err := bootstrap.JoinWith(ctx, baseJoinConfig(), runner)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "bootstrap: joined node not ready within timeout") {
		t.Errorf("unexpected error message: %v", err)
	}
}
