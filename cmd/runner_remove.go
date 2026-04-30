package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/spf13/cobra"
)

type runnerRemoveFlags struct {
	cluster string
}

var runnerRemoveF runnerRemoveFlags

var runnerRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a GitHub Actions runner scale set from a cluster",
	Args:  cobra.ExactArgs(1),
	RunE:  runRunnerRemove,
}

func init() {
	runnerRemoveCmd.Flags().StringVar(&runnerRemoveF.cluster, "cluster", "", "Target cluster name (default: active context cluster)")
}

func runRunnerRemove(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return RunRunnerRemove(ctx, args[0], runnerRemoveF.cluster, cmd.OutOrStdout(), RunnerCmdDeps{})
}

// RunRunnerRemove deletes the AutoscalingRunnerSet from the cluster and removes
// its registry row. It is exported so tests can drive it with injected deps
// and captured output.
func RunRunnerRemove(ctx context.Context, name, cluster string, out io.Writer, deps RunnerCmdDeps) error {
	var err error
	cluster, err = resolveCluster(cluster)
	if err != nil {
		return fmt.Errorf("runner remove: %w", err)
	}

	openReg := deps.OpenRegistry
	if openReg == nil {
		openReg = registry.NewRegistry
	}
	reg, err := openReg(ctx)
	if err != nil {
		return fmt.Errorf("runner remove: open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	if _, err := reg.GetDeployment(ctx, cluster, "gha-runner-scale-set"); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return fmt.Errorf("runner remove: gha-runner-scale-set addon is not installed on cluster %q — run: clusterbox addon install gha-runner-scale-set", cluster)
		}
		return fmt.Errorf("runner remove: check addon: %w", err)
	}

	cl, err := reg.GetCluster(ctx, cluster)
	if err != nil {
		return fmt.Errorf("runner remove: get cluster %q: %w", cluster, err)
	}

	runner := deps.Runner
	if runner == nil {
		runner = bootstrap.ExecRunner{}
	}
	if _, err := runner.Run(ctx, "kubectl", "--kubeconfig", cl.KubeconfigPath, "delete", "autoscalingrunnersets", name, "-n", arcRunnerNamespace, "--ignore-not-found"); err != nil {
		return fmt.Errorf("runner remove: kubectl delete: %w", err)
	}

	if err := reg.DeleteDeployment(ctx, cluster, name); err != nil {
		return fmt.Errorf("runner remove: delete registry row: %w", err)
	}

	_, _ = fmt.Fprintf(out, "runner scale set %q removed from cluster %q\n", name, cluster)
	return nil
}
