package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Provision a new cluster",
	Long:  `Provision a new k3s cluster on Hetzner using Pulumi and bootstrap it with k3sup.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("clusterbox up: not yet implemented")
		return nil
	},
}
