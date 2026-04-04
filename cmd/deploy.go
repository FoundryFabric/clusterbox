package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var deployCluster string

var deployCmd = &cobra.Command{
	Use:   "deploy <service> <version>",
	Short: "Deploy a service to a cluster",
	Long:  `Deploy a service at a given version to the specified cluster using Jsonnet manifests and kubectl.`,
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		service := args[0]
		ver := args[1]
		fmt.Printf("clusterbox deploy: service=%s version=%s cluster=%s (not yet implemented)\n", service, ver, deployCluster)
		return nil
	},
}

func init() {
	deployCmd.Flags().StringVar(&deployCluster, "cluster", "", "Target cluster name (required)")
	_ = deployCmd.MarkFlagRequired("cluster")
}
