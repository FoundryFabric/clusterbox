package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// addonUpgradeFlags holds CLI flags for `clusterbox addon upgrade`.
type addonUpgradeFlags struct {
	cluster string
}

var addonUpgradeF addonUpgradeFlags

var addonUpgradeCmd = &cobra.Command{
	Use:   "upgrade <name>",
	Short: "Upgrade an addon on a cluster to the catalog version",
	Long: `Upgrade re-applies the named addon's manifests against the target cluster
using the addon version currently in this binary's catalog. Because kubectl
apply is idempotent, upgrade is non-destructive — there is no confirmation
prompt.

The deployments row is updated to the new version on success; failures are
returned verbatim and leave the registry row unchanged.`,
	Args: cobra.ExactArgs(1),
	RunE: runAddonUpgrade,
}

func init() {
	addonUpgradeCmd.Flags().StringVar(&addonUpgradeF.cluster, "cluster", "", "Target cluster name (required)")
	_ = addonUpgradeCmd.MarkFlagRequired("cluster")
}

// runAddonUpgrade is the cobra RunE handler for `clusterbox addon upgrade`.
func runAddonUpgrade(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return RunAddonUpgrade(ctx, args[0], addonUpgradeF.cluster, cmd.OutOrStdout(), AddonCmdDeps{})
}

// RunAddonUpgrade executes the upgrade pipeline against the supplied (or
// default) Installer. It is exported so tests can drive it with an injected
// addonInstaller and a captured stdout writer.
//
// On success it prints a one-line confirmation including the (now-current)
// catalog version and the target cluster name. Failures are returned verbatim
// so cobra surfaces them on stderr.
func RunAddonUpgrade(ctx context.Context, addonName, clusterName string, out io.Writer, deps AddonCmdDeps) error {
	if clusterName == "" {
		return fmt.Errorf("addon upgrade: --cluster is required")
	}

	inst, version, cleanup, err := buildInstaller(ctx, addonName, deps)
	if err != nil {
		return fmt.Errorf("addon upgrade: %w", err)
	}
	defer cleanup()

	if err := inst.Upgrade(ctx, addonName, clusterName); err != nil {
		return err
	}
	fmt.Fprintf(out, "addon %q upgraded to %s on cluster %q\n", addonName, version, clusterName)
	return nil
}
