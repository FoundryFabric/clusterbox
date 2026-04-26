package main

import (
	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/node/install"
	"github.com/spf13/cobra"
)

var uninstallConfigPath string

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the node configuration (harden, tailscale, k3s)",
	Long: `uninstall reads the YAML config at --config and walks each section
in reverse order, recording per-section errors onto the result without
stopping the walk. The final JSON document includes every section so the
caller can see exactly what was torn down and what failed.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		spec, err := config.Load(uninstallConfigPath)
		if err != nil {
			return err
		}
		w := &install.Walker{
			Out:      cmd.OutOrStdout(),
			Sections: install.DefaultUninstallSections(),
		}
		return w.Uninstall(spec)
	},
}

func init() {
	uninstallCmd.Flags().StringVar(&uninstallConfigPath, "config", "", "Path to the YAML node configuration (required)")
	_ = uninstallCmd.MarkFlagRequired("config")
}
