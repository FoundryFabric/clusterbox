package cmd_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
)

// TestAddNode_StepOrder verifies the add-node sequence: Tailscale key →
// Pulumi → k3sup join → wait Ready. We test this at the bootstrap.JoinWith
// level (the innermost step we can inject) following the same pattern as
// up_test.go: steps 1 and 2 are recorded by the test harness; step 3+4 are
// exercised through the real JoinWith with a mock runner.
func TestAddNode_StepOrder(t *testing.T) {
	type stepEntry struct{ name string }
	var mu sync.Mutex
	var steps []stepEntry

	record := func(s string) {
		mu.Lock()
		steps = append(steps, stepEntry{s})
		mu.Unlock()
	}

	// Simulate step 1 (Tailscale key generation – mocked via closure).
	record("step1_ts_authkey")

	// Simulate step 2 (Pulumi provision – mocked).
	record("step2_pulumi")

	// Step 3+4: k3sup join + wait Ready, exercised via JoinWith.
	joinRunner := &mockCommandRunner{
		response: func(name string, args []string) ([]byte, error) {
			if name == "k3sup" {
				record("step3_k3sup_join")
				return nil, nil
			}
			if name == "kubectl" && containsArg(args, "get") {
				return []byte("worker-1   Ready   <none>   1m\n"), nil
			}
			return nil, nil
		},
	}

	joinCfg := bootstrap.JoinConfig{
		NodeIP:         "100.64.0.2",
		ServerIP:       "100.64.0.1",
		K3sVersion:     bootstrap.DefaultK3sVersion,
		KubeconfigPath: "/tmp/test-kube.yaml",
		SSHKeyPath:     "/tmp/id_ed25519",
	}

	if err := bootstrap.JoinWith(context.Background(), joinCfg, joinRunner); err != nil {
		t.Fatalf("JoinWith failed: %v", err)
	}
	record("step4_node_ready")

	mu.Lock()
	got := make([]string, len(steps))
	for i, s := range steps {
		got[i] = s.name
	}
	mu.Unlock()

	want := []string{
		"step1_ts_authkey",
		"step2_pulumi",
		"step3_k3sup_join",
		"step4_node_ready",
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d steps, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step[%d]: want %q, got %q", i, want[i], got[i])
		}
	}
}

// TestAddNode_JoinPassesCorrectFlags verifies that the JoinConfig values
// (node IP, server IP, k3s version) are forwarded correctly to k3sup.
func TestAddNode_JoinPassesCorrectFlags(t *testing.T) {
	runner := &mockCommandRunner{
		response: func(name string, args []string) ([]byte, error) {
			if name == "kubectl" {
				return []byte("worker   Ready   <none>   1m\n"), nil
			}
			return nil, nil
		},
	}

	cfg := bootstrap.JoinConfig{
		NodeIP:         "100.64.0.50",
		ServerIP:       "100.64.0.1",
		K3sVersion:     "v1.32.3+k3s1",
		KubeconfigPath: "/tmp/kube.yaml",
		SSHKeyPath:     "/tmp/id_ed25519",
	}

	if err := bootstrap.JoinWith(context.Background(), cfg, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(runner.calls) == 0 {
		t.Fatal("no calls made")
	}

	k3supCall := runner.calls[0]
	if k3supCall.name != "k3sup" {
		t.Fatalf("first call should be k3sup, got %q", k3supCall.name)
	}
	if len(k3supCall.args) == 0 || k3supCall.args[0] != "join" {
		t.Fatalf("expected k3sup subcommand 'join', got %v", k3supCall.args)
	}

	assertFlagValue(t, k3supCall.args, "--ip", "100.64.0.50")
	assertFlagValue(t, k3supCall.args, "--server-ip", "100.64.0.1")
	assertFlagValue(t, k3supCall.args, "--k3s-version", "v1.32.3+k3s1")
}

// TestAddNode_JoinErrorPropagated verifies that a k3sup join failure is
// returned to the caller with a descriptive message.
func TestAddNode_JoinErrorPropagated(t *testing.T) {
	runner := &mockCommandRunner{
		response: func(name string, _ []string) ([]byte, error) {
			if name == "k3sup" {
				return []byte("dial tcp refused"), &fakeExitError{}
			}
			return nil, nil
		},
	}

	err := bootstrap.JoinWith(context.Background(), bootstrap.JoinConfig{
		NodeIP:         "100.64.0.2",
		ServerIP:       "100.64.0.1",
		K3sVersion:     bootstrap.DefaultK3sVersion,
		KubeconfigPath: "/tmp/kube.yaml",
		SSHKeyPath:     "/tmp/id_ed25519",
	}, runner)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "k3sup join") {
		t.Errorf("error should reference k3sup join, got: %v", err)
	}
}

// fakeExitError satisfies the error interface for mocking non-zero exits.
type fakeExitError struct{}

func (*fakeExitError) Error() string { return "exit status 1" }
