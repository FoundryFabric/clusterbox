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

type runnerUpdateFlags struct {
	cluster string
	min     int
	max     int
}

var runnerUpdateF runnerUpdateFlags

var runnerUpdateCmd = &cobra.Command{
	Use:   "update <name>",
	Short: "Update settings on an existing GitHub Actions runner scale set",
	Long: `Update patches an existing AutoscalingRunnerSet's minRunners and/or
maxRunners without removing and re-adding it. At least one of --min or --max
must be provided.`,
	Example: `  # Cap to 1 concurrent runner on a resource-constrained single-node cluster
  clusterbox runner update server-runners --max 1

  # Keep a warm runner alive at all times
  clusterbox runner update server-runners --min 1

  # Both
  clusterbox runner update server-runners --min 1 --max 4`,
	Args: cobra.ExactArgs(1),
	RunE: runRunnerUpdate,
}

func init() {
	runnerUpdateCmd.Flags().StringVar(&runnerUpdateF.cluster, "cluster", "", "Target cluster name (default: active context cluster)")
	runnerUpdateCmd.Flags().IntVar(&runnerUpdateF.min, "min", -1, "Minimum number of runners (-1 = no change)")
	runnerUpdateCmd.Flags().IntVar(&runnerUpdateF.max, "max", -1, "Maximum number of runners (-1 = no change)")
}

func runRunnerUpdate(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return RunRunnerUpdate(ctx, args[0], runnerUpdateF.cluster, runnerUpdateF.min, runnerUpdateF.max, cmd.OutOrStdout(), RunnerCmdDeps{})
}

// RunRunnerUpdate patches an existing AutoscalingRunnerSet's min/max runner
// counts. Pass -1 for min or max to leave that field unchanged.
// It is exported so tests can drive it with injected deps and captured output.
func RunRunnerUpdate(ctx context.Context, name, cluster string, min, max int, out io.Writer, deps RunnerCmdDeps) error {
	if min < 0 && max < 0 {
		return fmt.Errorf("runner update: at least one of --min or --max must be set")
	}
	if min >= 0 && max >= 0 && min > max {
		return fmt.Errorf("runner update: --min (%d) must not exceed --max (%d)", min, max)
	}

	var err error
	cluster, err = resolveCluster(cluster)
	if err != nil {
		return fmt.Errorf("runner update: %w", err)
	}

	openReg := deps.OpenRegistry
	if openReg == nil {
		openReg = registry.NewRegistry
	}
	reg, err := openReg(ctx)
	if err != nil {
		return fmt.Errorf("runner update: open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	if _, err := reg.GetDeployment(ctx, cluster, "gha-runner-scale-set"); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return fmt.Errorf("runner update: gha-runner-scale-set addon is not installed on cluster %q — run: clusterbox addon install gha-runner-scale-set", cluster)
		}
		return fmt.Errorf("runner update: check addon: %w", err)
	}

	if _, err := reg.GetDeployment(ctx, cluster, name); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return fmt.Errorf("runner update: runner scale set %q not found on cluster %q — run: clusterbox runner list", name, cluster)
		}
		return fmt.Errorf("runner update: check runner: %w", err)
	}

	cl, err := reg.GetCluster(ctx, cluster)
	if err != nil {
		return fmt.Errorf("runner update: get cluster %q: %w", cluster, err)
	}

	runner := deps.Runner
	if runner == nil {
		runner = bootstrap.ExecRunner{}
	}

	// Build a minimal JSON merge-patch containing only the fields being changed.
	patch := `{"spec":{`
	sep := ""
	if min >= 0 {
		patch += fmt.Sprintf(`%s"minRunners":%d`, sep, min)
		sep = ","
	}
	if max >= 0 {
		patch += fmt.Sprintf(`%s"maxRunners":%d`, sep, max)
	}
	patch += `}}`

	if _, err := runner.Run(ctx, "kubectl", "--kubeconfig", cl.KubeconfigPath,
		"patch", "autoscalingrunnersets", name,
		"-n", arcRunnerNamespace,
		"--type=merge",
		"-p", patch,
	); err != nil {
		return fmt.Errorf("runner update: kubectl patch: %w", err)
	}

	_, _ = fmt.Fprintf(out, "runner scale set %q updated on cluster %q\n", name, cluster)
	return nil
}
