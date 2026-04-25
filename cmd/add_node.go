package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/tailscale"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/spf13/cobra"
)

// AddNodeDeps groups injectable dependencies for the add-node command. Tests
// replace individual fields; nil fields fall back to production defaults.
type AddNodeDeps struct {
	// OpenRegistry opens the local registry. Defaults to registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)
}

var addNodeCmd = &cobra.Command{
	Use:   "add-node",
	Short: "Add a node to an existing cluster",
	Long:  `Provision a new Hetzner VM and join it to an existing k3s cluster via k3sup.`,
}

// addNodeFlags holds all CLI flags for the add-node command.
type addNodeFlags struct {
	cluster    string
	provider   string
	region     string
	k3sVersion string
}

var addNodeF addNodeFlags

func init() {
	addNodeCmd.Flags().StringVar(&addNodeF.cluster, "cluster", "", "Cluster name to add the node to (required)")
	addNodeCmd.Flags().StringVar(&addNodeF.provider, "provider", "hetzner", "Infrastructure provider")
	addNodeCmd.Flags().StringVar(&addNodeF.region, "region", "ash", "Region / datacenter location")
	addNodeCmd.Flags().StringVar(&addNodeF.k3sVersion, "k3s-version", bootstrap.DefaultK3sVersion, "k3s version to install")
	_ = addNodeCmd.MarkFlagRequired("cluster")
	addNodeCmd.RunE = runAddNode
}

// runAddNode is the cobra RunE handler for `clusterbox add-node`.
func runAddNode(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	clusterName := addNodeF.cluster

	// Read required env vars.
	hetznerToken := os.Getenv("HETZNER_API_TOKEN")
	tsClientID := os.Getenv("TAILSCALE_OAUTH_CLIENT_ID")
	tsClientSecret := os.Getenv("TAILSCALE_OAUTH_CLIENT_SECRET")
	pulumiToken := os.Getenv("PULUMI_ACCESS_TOKEN")

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("add-node: resolve home dir: %w", err)
	}
	kubeconfigPath := filepath.Join(home, ".kube", clusterName+".yaml")

	// -------------------------------------------------------------------------
	// Step 1: Generate Tailscale ephemeral auth key
	// -------------------------------------------------------------------------
	fmt.Fprintln(os.Stderr, "[1/4] Generating Tailscale auth key...")
	tsAuthKey, err := tailscale.GenerateAuthKey(ctx, tsClientID, tsClientSecret)
	if err != nil {
		return fmt.Errorf("[1/4] failed: %w", err)
	}

	// -------------------------------------------------------------------------
	// Step 2: Pulumi — provision new VM using existing stack
	// -------------------------------------------------------------------------
	fmt.Fprintln(os.Stderr, "[2/4] Running Pulumi to provision new node VM...")
	// Derive a node-specific resource name: <cluster>-node-<timestamp-like suffix>
	// We reuse the existing stack by appending to the cluster's Pulumi stack.
	nodeName := clusterName + "-node"
	cfg := provision.ClusterConfig{
		ClusterName:           nodeName,
		SnapshotName:          "clusterbox-base-v0.1.0",
		Location:              addNodeF.region,
		DNSDomain:             clusterName + ".foundryfabric.dev",
		TailscaleClientID:     tsClientID,
		TailscaleClientSecret: tsClientSecret,
	}
	if err := runAddNodePulumiStack(ctx, clusterName, nodeName, hetznerToken, pulumiToken, tsAuthKey, cfg); err != nil {
		return fmt.Errorf("[2/4] failed: %w", err)
	}

	// -------------------------------------------------------------------------
	// Step 3: k3sup join — join new node to existing cluster
	// -------------------------------------------------------------------------
	fmt.Fprintln(os.Stderr, "[3/4] Joining new node to cluster via k3sup...")
	joinCfg := bootstrap.JoinConfig{
		NodeIP:         nodeName,    // Tailscale resolves the hostname
		ServerIP:       clusterName, // Control-plane Tailscale hostname
		K3sVersion:     addNodeF.k3sVersion,
		KubeconfigPath: kubeconfigPath,
		SSHKeyPath:     filepath.Join(home, ".ssh", "id_ed25519"),
	}
	if err := bootstrap.Join(ctx, joinCfg); err != nil {
		return fmt.Errorf("[3/4] failed: %w", err)
	}

	// -------------------------------------------------------------------------
	// Step 4: Wait for node Ready (handled inside Join / JoinWith)
	// -------------------------------------------------------------------------
	fmt.Fprintln(os.Stderr, "[4/4] Node joined and Ready.")

	// -------------------------------------------------------------------------
	// Best-effort: record the new node in the local registry. Failures here
	// must not fail the command — the registry is a local cache, not the
	// source of truth.
	// -------------------------------------------------------------------------
	recordNodeInRegistry(ctx, AddNodeDeps{}, clusterName, nodeName)

	fmt.Fprintf(os.Stderr, "Node %q successfully added to cluster %q.\n", nodeName, clusterName)
	return nil
}

// recordNodeInRegistry writes a worker-node row to the local registry on a
// best-effort basis. It is called only after a successful k3sup join. Errors
// are logged to stderr; the function never returns an error so that registry
// failures cannot break a successful add-node.
//
// The cluster row itself is left untouched: add-node does not modify the
// cluster's CreatedAt, KubeconfigPath, or any other column.
func recordNodeInRegistry(ctx context.Context, deps AddNodeDeps, clusterName, hostname string) {
	open := deps.OpenRegistry
	if open == nil {
		open = registry.NewRegistry
	}

	reg, err := open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", err)
		return
	}
	defer func() {
		if cerr := reg.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", cerr)
		}
	}()

	if err := reg.UpsertNode(ctx, registry.Node{
		ClusterName: clusterName,
		Hostname:    hostname,
		Role:        "worker",
		JoinedAt:    time.Now().UTC(),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", err)
	}
}

// runAddNodePulumiStack creates or updates a Pulumi stack for a new node VM
// within an existing cluster. The stack name is scoped to the cluster.
func runAddNodePulumiStack(ctx context.Context, clusterName, nodeName, hetznerToken, pulumiToken, tsAuthKey string, cfg provision.ClusterConfig) error {
	program := func(pCtx *pulumi.Context) error {
		userData, err := provision.RenderCloudInit(nodeName, tsAuthKey)
		if err != nil {
			return err
		}
		return provision.ProvisionStackWithUserData(pCtx, cfg, userData)
	}

	if pulumiToken != "" {
		_ = os.Setenv("PULUMI_ACCESS_TOKEN", pulumiToken)
	}

	// Use the cluster name as the Pulumi project, node name as the stack.
	s, err := auto.UpsertStackInlineSource(ctx, nodeName, clusterName, program)
	if err != nil {
		return fmt.Errorf("pulumi: upsert stack: %w", err)
	}

	if err := s.SetConfig(ctx, "hcloud:token", auto.ConfigValue{Value: hetznerToken, Secret: true}); err != nil {
		return fmt.Errorf("pulumi: set hcloud token: %w", err)
	}

	if _, err = s.Up(ctx, optup.ProgressStreams(os.Stderr)); err != nil {
		return fmt.Errorf("pulumi: up: %w", err)
	}
	return nil
}
