// Command clusterboxnode is the on-host agent run by clusterbox to install
// and uninstall the standard node stack (harden, Tailscale, k3s).
//
// It is linux-only by design — the binary is intended to be cross-compiled
// from CI for linux/amd64 and linux/arm64 and shipped to bare-metal nodes.
// Files in this package therefore use no platform-specific APIs and carry
// no build tags.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "clusterboxnode",
	Short: "On-host agent for clusterbox node provisioning",
	Long: `clusterboxnode runs on a target node and applies (or removes) the
standard clusterbox node stack: host hardening, Tailscale enrolment, and
k3s install. It reads a YAML config and walks each section in order,
emitting structured JSON to stdout.`,
	Version:       version,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func main() {
	rootCmd.AddCommand(installCmd)
	rootCmd.AddCommand(uninstallCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
