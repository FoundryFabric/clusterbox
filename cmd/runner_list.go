package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/spf13/cobra"
)

type runnerListFlags struct {
	cluster string
}

var runnerListF runnerListFlags

var runnerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List GitHub Actions runner scale sets on a cluster",
	RunE:  runRunnerList,
}

func init() {
	runnerListCmd.Flags().StringVar(&runnerListF.cluster, "cluster", "", "Target cluster name (default: active context cluster)")
}

func runRunnerList(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return RunRunnerList(ctx, runnerListF.cluster, cmd.OutOrStdout(), RunnerCmdDeps{})
}

// RunRunnerList lists all runner scale sets registered on the cluster. It is
// exported so tests can drive it with injected deps and captured output.
func RunRunnerList(ctx context.Context, cluster string, out io.Writer, deps RunnerCmdDeps) error {
	var err error
	cluster, err = resolveCluster(cluster)
	if err != nil {
		return fmt.Errorf("runner list: %w", err)
	}

	openReg := deps.OpenRegistry
	if openReg == nil {
		openReg = registry.NewRegistry
	}
	reg, err := openReg(ctx)
	if err != nil {
		return fmt.Errorf("runner list: open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	if _, err := reg.GetDeployment(ctx, cluster, "gha-runner-scale-set"); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return fmt.Errorf("runner list: gha-runner-scale-set addon is not installed on cluster %q — run: clusterbox addon install gha-runner-scale-set", cluster)
		}
		return fmt.Errorf("runner list: check addon: %w", err)
	}

	deployments, err := reg.ListDeployments(ctx, cluster)
	if err != nil {
		return fmt.Errorf("runner list: list deployments: %w", err)
	}

	var rows []registry.Deployment
	for _, d := range deployments {
		if d.Kind == registry.KindRunnerScaleSet {
			rows = append(rows, d)
		}
	}

	if len(rows) == 0 {
		_, _ = fmt.Fprintf(out, "no runner scale sets on cluster %q\n", cluster)
		return nil
	}

	for _, r := range rows {
		_, _ = fmt.Fprintf(out, "%-30s  %-50s  %s\n", r.Service, r.Version, r.Status)
	}
	return nil
}
