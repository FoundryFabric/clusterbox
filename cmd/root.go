package cmd

import (
	"fmt"
	"os"

	// Register the SQLite registry backend so registry.NewRegistry can find it.
	_ "github.com/foundryfabric/clusterbox/internal/registry/sqlite"
	"github.com/spf13/cobra"
)

var version = "dev"

// Version returns the clusterbox CLI version string, set at build time via
// `-ldflags "-X github.com/foundryfabric/clusterbox/cmd.version=$VERSION"`.
// It is exported so the agentbundle package can assert (in a unit test) that
// the embedded clusterboxnode binaries were built with the same version
// stamp — catching Makefile drift loudly.
func Version() string {
	return version
}

// globalContextOverride holds the value of the --context persistent flag.
// Commands that resolve infra credentials read this to select the active
// context rather than CurrentContext in config.yaml.
var globalContextOverride string

var rootCmd = &cobra.Command{
	Use:     "clusterbox",
	Short:   "Cluster provisioner: Pulumi + k3sup + Jsonnet + kubectl",
	Long:    `clusterbox provisions and manages k3s clusters on Hetzner using Pulumi, k3sup, Jsonnet, and kubectl.`,
	Version: version,
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&globalContextOverride, "context", "", "Override the active context (default: current_context in config)")

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
	rootCmd.AddCommand(addonCmd)
	rootCmd.AddCommand(loginCmd)
	rootCmd.AddCommand(useContextCmd)
}
