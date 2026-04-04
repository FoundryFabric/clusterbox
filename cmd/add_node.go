package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var addNodeCmd = &cobra.Command{
	Use:   "add-node",
	Short: "Add a node to an existing cluster",
	Long:  `Provision a new Hetzner VM and join it to an existing k3s cluster via k3sup.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("clusterbox add-node: not yet implemented")
		return nil
	},
}
