package cmd_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/cmd"
)

// TestRemoveNode_StepOrder verifies the remove-node sequence:
// kubectl drain → kubectl delete node, for a single node.
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

	err := cmd.RunRemoveNodeWith(context.Background(), "prod", []string{"worker-1"}, runner)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(order) < 2 {
		t.Fatalf("expected at least 2 kubectl steps, got %d: %v", len(order), order)
	}
	if order[0] != "drain" {
		t.Errorf("first step should be drain, got %q", order[0])
	}
	if order[1] != "delete" {
		t.Errorf("second step should be delete, got %q", order[1])
	}
}

// TestRemoveNode_DrainFailureStopsEarly verifies that a kubectl drain failure
// aborts the sequence before kubectl delete is called.
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

	err := cmd.RunRemoveNodeWith(context.Background(), "prod", []string{"worker-1"}, runner)
	if err == nil {
		t.Fatal("expected error from drain failure, got nil")
	}
	if !strings.Contains(err.Error(), "1/2") {
		t.Errorf("error should reference step 1/2, got: %v", err)
	}
	for _, s := range order {
		if s == "delete" {
			t.Error("kubectl delete should not have been called after drain failed")
		}
	}
}

// TestRemoveNode_DeleteFailureStopsEarly verifies that a kubectl delete failure
// is reported correctly.
func TestRemoveNode_DeleteFailureStopsEarly(t *testing.T) {
	runner := &mockCommandRunner{
		response: func(name string, args []string) ([]byte, error) {
			if name == "kubectl" && containsArg(args, "drain") {
				return nil, nil
			}
			if name == "kubectl" && containsArg(args, "delete") {
				return nil, errors.New("exit status 1")
			}
			return nil, nil
		},
	}

	err := cmd.RunRemoveNodeWith(context.Background(), "prod", []string{"worker-1"}, runner)
	if err == nil {
		t.Fatal("expected error from delete failure, got nil")
	}
	if !strings.Contains(err.Error(), "2/2") {
		t.Errorf("error should reference step 2/2, got: %v", err)
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

	_ = cmd.RunRemoveNodeWith(context.Background(), "mycluster", []string{"node-1"}, runner)

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

// TestRemoveNode_MultipleNodesParallel verifies that specifying multiple nodes
// runs drain+delete for each, and all succeed.
func TestRemoveNode_MultipleNodesParallel(t *testing.T) {
	var drained, deleted []string

	runner := &mockCommandRunner{
		response: func(name string, args []string) ([]byte, error) {
			if name != "kubectl" {
				return nil, nil
			}
			// Extract the node name (last positional arg after the subcommand).
			node := args[len(args)-1]
			if containsArg(args, "drain") {
				drained = append(drained, node)
			}
			if containsArg(args, "delete") {
				deleted = append(deleted, node)
			}
			return nil, nil
		},
	}

	nodes := []string{"worker-1", "worker-2", "worker-3"}
	err := cmd.RunRemoveNodeWith(context.Background(), "prod", nodes, runner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(drained) != 3 {
		t.Errorf("expected 3 drain calls, got %d: %v", len(drained), drained)
	}
	if len(deleted) != 3 {
		t.Errorf("expected 3 delete calls, got %d: %v", len(deleted), deleted)
	}
}
