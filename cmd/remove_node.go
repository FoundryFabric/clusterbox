package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/spf13/cobra"
)

// RemoveNodeDeps groups injectable dependencies for the remove-node command.
// Tests replace individual fields; nil fields fall back to production defaults.
type RemoveNodeDeps struct {
	// OpenRegistry opens the local registry. Defaults to registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)
	// ResolveHetzner returns the Hetzner API token. Defaults to resolveToken.
	// Tests inject a no-op to skip the post-run reconcile hook.
	ResolveHetzner func() (string, error)
}

var removeNodeCmd = &cobra.Command{
	Use:   "remove-node",
	Short: "Remove one or more nodes from a cluster",
	Long:  `Drain and delete nodes from a k3s cluster, then destroy the underlying Hetzner VMs via the hcloud API. Specify --node multiple times to remove nodes in parallel.`,
}

// removeNodeFlags holds all CLI flags for the remove-node command.
type removeNodeFlags struct {
	cluster string
	nodes   []string
}

var removeNodeF removeNodeFlags

func init() {
	removeNodeCmd.Flags().StringVar(&removeNodeF.cluster, "cluster", "", "Cluster name (required)")
	removeNodeCmd.Flags().StringArrayVar(&removeNodeF.nodes, "node", nil, "Node name to remove; may be specified multiple times for parallel removal")
	_ = removeNodeCmd.MarkFlagRequired("cluster")
	_ = removeNodeCmd.MarkFlagRequired("node")
	removeNodeCmd.RunE = runRemoveNode
}

// runRemoveNode is the cobra RunE handler for `clusterbox remove-node`.
func runRemoveNode(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return RunRemoveNodeWith(ctx, removeNodeF.cluster, removeNodeF.nodes, bootstrap.ExecRunner{})
}

// RunRemoveNodeWith is the injectable variant used by tests. It defaults to
// the real registry; use RunRemoveNodeWithDeps to inject a fake.
func RunRemoveNodeWith(ctx context.Context, clusterName string, nodes []string, runner bootstrap.CommandRunner) error {
	return RunRemoveNodeWithDeps(ctx, clusterName, nodes, runner, RemoveNodeDeps{
		ResolveHetzner: func() (string, error) { return "", nil },
	})
}

// RunRemoveNodeWithDeps is the fully-injectable variant of remove-node.
// Tests use it to substitute the registry without touching the filesystem.
func RunRemoveNodeWithDeps(ctx context.Context, clusterName string, nodes []string, runner bootstrap.CommandRunner, deps RemoveNodeDeps) error {
	resolveHetzner := deps.ResolveHetzner
	if resolveHetzner == nil {
		resolveHetzner = func() (string, error) {
			return resolveToken("hetzner", "HETZNER_API_TOKEN")
		}
	}
	hetznerToken, err := resolveHetzner()
	if err != nil {
		return fmt.Errorf("remove-node: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("remove-node: resolve home dir: %w", err)
	}
	kubeconfigPath := filepath.Join(home, ".kube", clusterName+".yaml")

	if len(nodes) == 1 {
		_, _ = fmt.Fprintf(os.Stderr, "Removing node %q from cluster %q...\n", nodes[0], clusterName)
	} else {
		_, _ = fmt.Fprintf(os.Stderr, "Removing %d nodes from cluster %q in parallel...\n", len(nodes), clusterName)
		for _, nn := range nodes {
			_, _ = fmt.Fprintf(os.Stderr, "  %s\n", nn)
		}
	}

	type nodeResult struct {
		nodeName string
		err      error
	}
	ch := make(chan nodeResult, len(nodes))

	for _, nn := range nodes {
		go func() {
			ch <- nodeResult{
				nodeName: nn,
				err:      removeOneNode(ctx, clusterName, nn, kubeconfigPath, runner),
			}
		}()
	}

	type failedNode struct {
		name string
		err  error
	}
	var failures []failedNode
	for range nodes {
		r := <-ch
		if r.err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "[%s] FAILED: %v\n", r.nodeName, r.err)
			failures = append(failures, failedNode{name: r.nodeName, err: r.err})
		} else {
			removeNodeFromRegistry(ctx, deps, clusterName, r.nodeName)
		}
	}

	if len(failures) > 0 {
		msgs := make([]string, len(failures))
		for i, f := range failures {
			msgs[i] = fmt.Sprintf("%s: %v", f.name, f.err)
		}
		return fmt.Errorf("remove-node: %d of %d node(s) failed:\n  %s",
			len(failures), len(nodes), strings.Join(msgs, "\n  "))
	}

	// One reconcile sweep after all nodes are drained and deleted.
	_, _ = fmt.Fprintln(os.Stderr, "Sweeping node VM resources via hcloud API...")
	runReconcileHook(ctx, ReconcileDeps{}, clusterName, hetznerToken)

	if len(nodes) == 1 {
		_, _ = fmt.Fprintf(os.Stderr, "Node %q successfully removed from cluster %q.\n", nodes[0], clusterName)
	} else {
		_, _ = fmt.Fprintf(os.Stderr, "All %d nodes successfully removed from cluster %q.\n", len(nodes), clusterName)
	}
	return nil
}

// removeOneNode drains and deletes a single node from the cluster. It is
// called concurrently by RunRemoveNodeWithDeps when multiple nodes are given.
// All log lines are prefixed with [nodeName] so interleaved output is readable.
func removeOneNode(ctx context.Context, clusterName, nodeName, kubeconfigPath string, runner bootstrap.CommandRunner) error {
	logf := func(msg string) {
		_, _ = fmt.Fprintf(os.Stderr, "[%s] %s\n", nodeName, msg)
	}
	_ = clusterName // reserved for future use (e.g. node-specific sweeps)

	// Step 1: kubectl drain.
	logf("[1/2] Draining node...")
	if _, err := runner.Run(ctx, "kubectl",
		"--kubeconfig", kubeconfigPath,
		"drain", nodeName,
		"--ignore-daemonsets",
		"--delete-emptydir-data",
		"--timeout=60s",
	); err != nil {
		return fmt.Errorf("[1/2] kubectl drain failed: %w", err)
	}

	// Step 2: kubectl delete node.
	logf("[2/2] Deleting node from cluster...")
	if _, err := runner.Run(ctx, "kubectl",
		"--kubeconfig", kubeconfigPath,
		"delete", "node", nodeName,
	); err != nil {
		return fmt.Errorf("[2/2] kubectl delete node failed: %w", err)
	}

	logf("Drain and delete complete.")
	return nil
}

// removeNodeFromRegistry deletes the node row from the local registry on a
// best-effort basis. Errors are logged to stderr; the function never returns
// an error so that registry failures cannot break a successful remove-node.
//
// The cluster row itself is left untouched.
func removeNodeFromRegistry(ctx context.Context, deps RemoveNodeDeps, clusterName, hostname string) {
	open := deps.OpenRegistry
	if open == nil {
		open = registry.NewRegistry
	}

	reg, err := open(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", err)
		return
	}
	defer func() {
		if cerr := reg.Close(); cerr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", cerr)
		}
	}()

	if err := reg.RemoveNode(ctx, clusterName, hostname); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", err)
	}
}
