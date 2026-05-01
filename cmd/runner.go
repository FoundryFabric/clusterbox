package cmd

import (
	"context"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/spf13/cobra"
)

// RunnerCmdDeps groups the injectable dependencies used by runner subcommands.
// nil fields fall back to production defaults.
type RunnerCmdDeps struct {
	OpenRegistry func(ctx context.Context) (registry.Registry, error)
	Runner       bootstrap.CommandRunner
}

var runnerCmd = &cobra.Command{
	Use:   "runner",
	Short: "Manage GitHub Actions runner scale sets",
	Long:  `Runner groups subcommands for managing ARC AutoscalingRunnerSets on a cluster. Requires the gha-runner-scale-set addon to be installed first.`,
}

func init() {
	runnerCmd.AddCommand(runnerAddCmd)
	runnerCmd.AddCommand(runnerKickCmd)
	runnerCmd.AddCommand(runnerListCmd)
	runnerCmd.AddCommand(runnerRemoveCmd)
	runnerCmd.AddCommand(runnerUpdateCmd)
}
