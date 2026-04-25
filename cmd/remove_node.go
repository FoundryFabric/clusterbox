package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/spf13/cobra"
)

// RemoveNodeDeps groups injectable dependencies for the remove-node command.
// Tests replace individual fields; nil fields fall back to production defaults.
type RemoveNodeDeps struct {
	// OpenRegistry opens the local registry. Defaults to registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)
}

var removeNodeCmd = &cobra.Command{
	Use:   "remove-node",
	Short: "Remove a node from a cluster",
	Long:  `Drain and delete a node from a k3s cluster, then destroy the underlying Hetzner VM via Pulumi.`,
}

// removeNodeFlags holds all CLI flags for the remove-node command.
type removeNodeFlags struct {
	cluster string
	node    string
}

var removeNodeF removeNodeFlags

func init() {
	removeNodeCmd.Flags().StringVar(&removeNodeF.cluster, "cluster", "", "Cluster name (required)")
	removeNodeCmd.Flags().StringVar(&removeNodeF.node, "node", "", "Node name to remove (required)")
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
	return RunRemoveNodeWith(ctx, removeNodeF.cluster, removeNodeF.node, bootstrap.ExecRunner{})
}

// RunRemoveNodeWith is the injectable variant used by tests. It defaults to
// the real registry; use RunRemoveNodeWithDeps to inject a fake.
func RunRemoveNodeWith(ctx context.Context, clusterName, nodeName string, runner bootstrap.CommandRunner) error {
	return RunRemoveNodeWithDeps(ctx, clusterName, nodeName, runner, RemoveNodeDeps{})
}

// RunRemoveNodeWithDeps is the fully-injectable variant of remove-node.
// Tests use it to substitute the registry without touching the filesystem.
func RunRemoveNodeWithDeps(ctx context.Context, clusterName, nodeName string, runner bootstrap.CommandRunner, deps RemoveNodeDeps) error {
	hetznerToken := os.Getenv("HETZNER_API_TOKEN")
	pulumiToken := os.Getenv("PULUMI_ACCESS_TOKEN")

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("remove-node: resolve home dir: %w", err)
	}
	kubeconfigPath := filepath.Join(home, ".kube", clusterName+".yaml")

	// -------------------------------------------------------------------------
	// Step 1: kubectl drain
	// -------------------------------------------------------------------------
	fmt.Fprintln(os.Stderr, "[1/3] Draining node...")
	if _, err := runner.Run(ctx, "kubectl",
		"--kubeconfig", kubeconfigPath,
		"drain", nodeName,
		"--ignore-daemonsets",
		"--delete-emptydir-data",
		"--timeout=60s",
	); err != nil {
		return fmt.Errorf("[1/3] kubectl drain failed: %w", err)
	}

	// -------------------------------------------------------------------------
	// Step 2: kubectl delete node
	// -------------------------------------------------------------------------
	fmt.Fprintln(os.Stderr, "[2/3] Deleting node from cluster...")
	if _, err := runner.Run(ctx, "kubectl",
		"--kubeconfig", kubeconfigPath,
		"delete", "node", nodeName,
	); err != nil {
		return fmt.Errorf("[2/3] kubectl delete node failed: %w", err)
	}

	// -------------------------------------------------------------------------
	// Step 3: Pulumi destroy node VM/volume resources
	// -------------------------------------------------------------------------
	fmt.Fprintln(os.Stderr, "[3/3] Destroying node VM resources via Pulumi...")
	if err := destroyNodePulumiStack(ctx, clusterName, nodeName, hetznerToken, pulumiToken); err != nil {
		return fmt.Errorf("[3/3] failed: %w", err)
	}

	// -------------------------------------------------------------------------
	// Best-effort: drop the node row from the local registry. Failures here
	// must not fail the command — the tear-down already succeeded.
	// -------------------------------------------------------------------------
	removeNodeFromRegistry(ctx, deps, clusterName, nodeName)

	fmt.Fprintf(os.Stderr, "Node %q successfully removed from cluster %q.\n", nodeName, clusterName)
	return nil
}

// removeNodeFromRegistry deletes the node row from the local registry on a
// best-effort basis. It is called only after a successful Pulumi destroy.
// Errors are logged to stderr; the function never returns an error so that
// registry failures cannot break a successful remove-node.
//
// The cluster row itself is left untouched.
func removeNodeFromRegistry(ctx context.Context, deps RemoveNodeDeps, clusterName, hostname string) {
	open := deps.OpenRegistry
	if open == nil {
		open = registry.NewRegistry
	}

	reg, err := open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", err)
		return
	}
	defer func() {
		if cerr := reg.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", cerr)
		}
	}()

	if err := reg.RemoveNode(ctx, clusterName, hostname); err != nil {
		fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", err)
	}
}

// destroyNodePulumiStack destroys the Pulumi stack that was created for the
// given node, tearing down its VM and volume resources on Hetzner.
func destroyNodePulumiStack(ctx context.Context, clusterName, nodeName, hetznerToken, pulumiToken string) error {
	// The inline program is required by the Automation API even for destroy; it
	// is used only to resolve stack metadata — all resources will be deleted.
	program := func(pCtx *pulumi.Context) error {
		return provision.ProvisionStackWithUserData(pCtx, provision.ClusterConfig{
			ClusterName:  nodeName,
			SnapshotName: "clusterbox-base-v0.1.0",
			Location:     "ash",
			DNSDomain:    clusterName + ".foundryfabric.dev",
		}, "#cloud-config\nruncmd: []")
	}

	if pulumiToken != "" {
		_ = os.Setenv("PULUMI_ACCESS_TOKEN", pulumiToken)
	}

	// The stack name matches what add-node created: nodeName under the clusterName project.
	s, err := auto.UpsertStackInlineSource(ctx, nodeName, clusterName, program)
	if err != nil {
		return fmt.Errorf("pulumi: upsert stack: %w", err)
	}

	if err := s.SetConfig(ctx, "hcloud:token", auto.ConfigValue{Value: hetznerToken, Secret: true}); err != nil {
		return fmt.Errorf("pulumi: set hcloud token: %w", err)
	}

	if _, err = s.Destroy(ctx, optdestroy.ProgressStreams(os.Stderr)); err != nil {
		return fmt.Errorf("pulumi: destroy: %w", err)
	}
	return nil
}
