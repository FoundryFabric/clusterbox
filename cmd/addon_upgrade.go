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
	mode    string
}

var addonUpgradeF addonUpgradeFlags

var addonUpgradeCmd = &cobra.Command{
	Use:   "upgrade <name>",
	Short: "Upgrade an addon on a cluster to the catalog version",
	Long: `Upgrade re-applies the named addon's manifests against the target cluster
using the addon version currently in this binary's catalog. Because kubectl
apply is idempotent, upgrade is non-destructive — there is no confirmation
prompt.

For addons with multiple modes (e.g. telemetry), use --mode to select one.
If omitted, the addon's default mode is used.

The deployments row is updated to the new version on success; failures are
returned verbatim and leave the registry row unchanged.`,
	Args: cobra.ExactArgs(1),
	RunE: runAddonUpgrade,
}

func init() {
	addonUpgradeCmd.Flags().StringVar(&addonUpgradeF.cluster, "cluster", "", "Target cluster name (default: active context cluster)")
	addonUpgradeCmd.Flags().StringVar(&addonUpgradeF.mode, "mode", "", "Install mode for multi-mode addons (e.g. file, full)")
}

// runAddonUpgrade is the cobra RunE handler for `clusterbox addon upgrade`.
func runAddonUpgrade(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return RunAddonUpgrade(ctx, args[0], addonUpgradeF.cluster, addonUpgradeF.mode, cmd.OutOrStdout(), AddonCmdDeps{})
}

// RunAddonUpgrade executes the upgrade pipeline against the supplied (or
// default) Installer. It is exported so tests can drive it with an injected
// addonInstaller and a captured stdout writer.
//
// mode is passed through to the installer as the selected install mode for
// staged addons; an empty string uses the addon's default mode.
//
// On success it prints a one-line confirmation including the (now-current)
// catalog version and the target cluster name. Failures are returned verbatim
// so cobra surfaces them on stderr.
func RunAddonUpgrade(ctx context.Context, addonName, clusterName, mode string, out io.Writer, deps AddonCmdDeps) error {
	var err error
	clusterName, err = resolveCluster(clusterName)
	if err != nil {
		return fmt.Errorf("addon upgrade: %w", err)
	}

	inst, version, cleanup, err := buildInstaller(ctx, addonName, deps)
	if err != nil {
		return fmt.Errorf("addon upgrade: %w", err)
	}
	defer cleanup()

	if err := inst.Upgrade(ctx, addonName, clusterName, mode); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "addon %q upgraded to %s on cluster %q\n", addonName, version, clusterName)
	return nil
}
