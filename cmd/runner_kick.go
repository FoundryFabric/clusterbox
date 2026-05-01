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

type runnerKickFlags struct {
	cluster string
}

var runnerKickF runnerKickFlags

var runnerKickCmd = &cobra.Command{
	Use:   "kick",
	Short: "Restart the ARC controller to recover offline runners",
	Long: `Kick performs a rolling restart of the ARC controller deployment in
arc-systems, then waits for it to become ready. Use this when self-hosted
runners show as offline in GitHub Actions.`,
	Example: `  clusterbox runner kick
  clusterbox runner kick --cluster production`,
	RunE: runRunnerKick,
}

func init() {
	runnerKickCmd.Flags().StringVar(&runnerKickF.cluster, "cluster", "", "Target cluster name (default: active context cluster)")
}

func runRunnerKick(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return RunRunnerKick(ctx, runnerKickF.cluster, cmd.OutOrStdout(), RunnerCmdDeps{})
}

// RunRunnerKick performs a rolling restart of the ARC controller deployment
// and waits for it to become healthy. It is exported so tests can drive it
// with injected deps and captured output.
func RunRunnerKick(ctx context.Context, cluster string, out io.Writer, deps RunnerCmdDeps) error {
	var err error
	cluster, err = resolveCluster(cluster)
	if err != nil {
		return fmt.Errorf("runner kick: %w", err)
	}

	openReg := deps.OpenRegistry
	if openReg == nil {
		openReg = registry.NewRegistry
	}
	reg, err := openReg(ctx)
	if err != nil {
		return fmt.Errorf("runner kick: open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	if _, err := reg.GetDeployment(ctx, cluster, "gha-runner-scale-set"); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return fmt.Errorf("runner kick: gha-runner-scale-set addon is not installed on cluster %q — run: clusterbox addon install gha-runner-scale-set", cluster)
		}
		return fmt.Errorf("runner kick: check addon: %w", err)
	}

	cl, err := reg.GetCluster(ctx, cluster)
	if err != nil {
		return fmt.Errorf("runner kick: get cluster %q: %w", cluster, err)
	}

	runner := deps.Runner
	if runner == nil {
		runner = bootstrap.ExecRunner{}
	}

	_, _ = fmt.Fprintf(out, "restarting ARC controller on cluster %q...\n", cluster)

	if _, err := runner.Run(ctx, "kubectl", "--kubeconfig", cl.KubeconfigPath,
		"rollout", "restart", "deployment", "arc", "-n", "arc-systems"); err != nil {
		return fmt.Errorf("runner kick: rollout restart: %w", err)
	}

	if _, err := runner.Run(ctx, "kubectl", "--kubeconfig", cl.KubeconfigPath,
		"rollout", "status", "deployment", "arc", "-n", "arc-systems", "--timeout=90s"); err != nil {
		return fmt.Errorf("runner kick: rollout status: %w", err)
	}

	_, _ = fmt.Fprintf(out, "ARC controller ready — runners will reconnect shortly\n")
	return nil
}
