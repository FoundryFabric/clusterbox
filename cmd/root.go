package cmd

import (
	"fmt"
	"os"

	// Register the SQLite registry backend so registry.NewRegistry can find it.
	_ "github.com/foundryfabric/clusterbox/internal/registry/sqlite"
	"github.com/spf13/cobra"
)

var version = "dev"

var rootCmd = &cobra.Command{
	Use:     "clusterbox",
	Short:   "Cluster provisioner: Pulumi + k3sup + Jsonnet + kubectl",
	Long:    `clusterbox provisions and manages k3s clusters on Hetzner using Pulumi, k3sup, Jsonnet, and kubectl.`,
	Version: version,
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(upCmd)
	rootCmd.AddCommand(deployCmd)
	rootCmd.AddCommand(addNodeCmd)
	rootCmd.AddCommand(removeNodeCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(dashboardCmd)
	rootCmd.AddCommand(historyCmd)
	rootCmd.AddCommand(syncCmd)
	rootCmd.AddCommand(diffCmd)
	rootCmd.AddCommand(destroyCmd)
}
