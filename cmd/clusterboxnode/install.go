package main

import (
	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/node/install"
	"github.com/spf13/cobra"
)

var installConfigPath string

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Apply the node configuration (harden, tailscale, k3s)",
	Long: `install reads the YAML config at --config and walks each enabled
section in order. On success it emits a JSON object describing the result of
every section. If any section fails the walk stops and an error-shape JSON
document is emitted on stdout before the process exits non-zero.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		spec, err := config.Load(installConfigPath)
		if err != nil {
			return err
		}
		w := &install.Walker{
			Out:      cmd.OutOrStdout(),
			Sections: install.DefaultInstallSections(),
		}
		return w.Install(spec)
	},
}

func init() {
	installCmd.Flags().StringVar(&installConfigPath, "config", "", "Path to the YAML node configuration (required)")
	_ = installCmd.MarkFlagRequired("config")
}
