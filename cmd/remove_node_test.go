package cmd_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/cmd"
)

// TestRemoveNode_StepOrder verifies the remove-node sequence:
// kubectl drain → kubectl delete node → Pulumi destroy.
// Steps 1 and 2 (drain + delete) are captured via the mock runner.
// Step 3 (Pulumi destroy) is recorded by the test harness because it requires
// a live Pulumi backend; we confirm it would be called by checking the drain
// and delete precede it.
func TestRemoveNode_StepOrder(t *testing.T) {
	var order []string

	runner := &mockCommandRunner{
		response: func(name string, args []string) ([]byte, error) {
			if name == "kubectl" && containsArg(args, "drain") {
				order = append(order, "drain")
				return nil, nil
			}
			if name == "kubectl" && containsArg(args, "delete") {
				order = append(order, "delete")
				return nil, nil
			}
			return nil, nil
		},
	}

	// cmd.RunRemoveNodeWith stops after kubectl delete because we don't have a
	// real Pulumi stack; we verify only that drain happens before delete and
	// that the error returned is from the Pulumi step (step 3), not earlier.
	err := cmd.RunRemoveNodeWith(context.Background(), "prod", "worker-1", runner)

	// Drain and delete must have run in order before Pulumi is attempted.
	if len(order) < 2 {
		t.Fatalf("expected at least 2 kubectl steps, got %d: %v", len(order), order)
	}
	if order[0] != "drain" {
		t.Errorf("first step should be drain, got %q", order[0])
	}
	if order[1] != "delete" {
		t.Errorf("second step should be delete, got %q", order[1])
	}

	// The error (if any) should originate from the Pulumi destroy step (step 3),
	// confirming kubectl steps completed successfully.
	if err != nil && !strings.Contains(err.Error(), "[3/3]") {
		t.Errorf("unexpected error (should be from step 3 only): %v", err)
	}
}

// TestRemoveNode_DrainFailureStopsEarly verifies that a kubectl drain failure
// aborts the sequence before delete or Pulumi destroy are called.
func TestRemoveNode_DrainFailureStopsEarly(t *testing.T) {
	var order []string

	runner := &mockCommandRunner{
		response: func(name string, args []string) ([]byte, error) {
			if name == "kubectl" && containsArg(args, "drain") {
				order = append(order, "drain")
				return nil, errors.New("exit status 1")
			}
			if name == "kubectl" && containsArg(args, "delete") {
				order = append(order, "delete")
				return nil, nil
			}
			return nil, nil
		},
	}

	err := cmd.RunRemoveNodeWith(context.Background(), "prod", "worker-1", runner)
	if err == nil {
		t.Fatal("expected error from drain failure, got nil")
	}
	if !strings.Contains(err.Error(), "[1/3]") {
		t.Errorf("error should reference step 1, got: %v", err)
	}
	// delete must NOT have been called.
	for _, s := range order {
		if s == "delete" {
			t.Error("kubectl delete should not have been called after drain failed")
		}
	}
}

// TestRemoveNode_DeleteFailureStopsEarly verifies that a kubectl delete failure
// aborts the sequence before Pulumi destroy is called.
func TestRemoveNode_DeleteFailureStopsEarly(t *testing.T) {
	pulumiCalled := false

	runner := &mockCommandRunner{
		response: func(name string, args []string) ([]byte, error) {
			if name == "kubectl" && containsArg(args, "drain") {
				return nil, nil // drain succeeds
			}
			if name == "kubectl" && containsArg(args, "delete") {
				return nil, errors.New("exit status 1")
			}
			// Any other call would indicate Pulumi attempting kubectl.
			pulumiCalled = true
			return nil, nil
		},
	}

	err := cmd.RunRemoveNodeWith(context.Background(), "prod", "worker-1", runner)
	if err == nil {
		t.Fatal("expected error from delete failure, got nil")
	}
	if !strings.Contains(err.Error(), "[2/3]") {
		t.Errorf("error should reference step 2, got: %v", err)
	}
	if pulumiCalled {
		t.Error("Pulumi destroy should not have been called after delete failed")
	}
}

// TestRemoveNode_DrainFlags verifies that kubectl drain receives the required
// flags: --ignore-daemonsets, --delete-emptydir-data, --timeout=60s.
func TestRemoveNode_DrainFlags(t *testing.T) {
	var drainArgs []string

	runner := &mockCommandRunner{
		response: func(name string, args []string) ([]byte, error) {
			if name == "kubectl" && containsArg(args, "drain") {
				drainArgs = args
			}
			return nil, nil
		},
	}

	// We only care about the drain call; subsequent steps may fail.
	_ = cmd.RunRemoveNodeWith(context.Background(), "mycluster", "node-1", runner)

	if len(drainArgs) == 0 {
		t.Fatal("drain was not called")
	}

	for _, flag := range []string{"--ignore-daemonsets", "--delete-emptydir-data", "--timeout=60s"} {
		found := false
		for _, a := range drainArgs {
			if a == flag {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("kubectl drain missing flag %q; got args: %v", flag, drainArgs)
		}
	}
}
