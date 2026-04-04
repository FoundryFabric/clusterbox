package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var removeNodeCmd = &cobra.Command{
	Use:   "remove-node",
	Short: "Remove a node from a cluster",
	Long:  `Drain and delete a node from a k3s cluster, then destroy the underlying Hetzner VM via Pulumi.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("clusterbox remove-node: not yet implemented")
		return nil
	},
}
