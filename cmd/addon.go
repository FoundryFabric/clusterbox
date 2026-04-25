package cmd

import (
	"github.com/spf13/cobra"
)

// addonCmd is the parent command for addon-related subcommands. It has no
// runnable behavior of its own; subcommands (list, install, uninstall, ...)
// hang off it.
var addonCmd = &cobra.Command{
	Use:   "addon",
	Short: "Manage cluster addons",
	Long: `Addon groups subcommands that operate on the clusterbox addon catalog
and on addons installed into a cluster. Use "clusterbox addon list" to see the
catalog of known addons or those installed on a specific cluster.`,
}

func init() {
	addonCmd.AddCommand(addonListCmd)
	addonCmd.AddCommand(addonInstallCmd)
	addonCmd.AddCommand(addonUninstallCmd)
	addonCmd.AddCommand(addonUpgradeCmd)
}
