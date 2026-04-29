package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/provision/hetzner"
	"github.com/foundryfabric/clusterbox/internal/provision/qemu"
	"github.com/foundryfabric/clusterbox/internal/registry"
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
	Short: "Add one or more nodes to an existing cluster",
	Long:  `Provision Hetzner VMs and join them to an existing k3s cluster via k3sup. Use --count to add multiple nodes in parallel.`,
}

// addNodeFlags holds all CLI flags for the add-node command.
type addNodeFlags struct {
	cluster      string
	provider     string
	region       string
	k3sVersion   string
	count        int
	tailscaleTag string
}

var addNodeF addNodeFlags

func init() {
	addNodeCmd.Flags().StringVar(&addNodeF.cluster, "cluster", "", "Cluster name to add the node to (required)")
	addNodeCmd.Flags().StringVar(&addNodeF.provider, "provider", hetzner.Name, "Infrastructure provider")
	addNodeCmd.Flags().StringVar(&addNodeF.region, "region", "ash", "Region / datacenter location")
	addNodeCmd.Flags().StringVar(&addNodeF.k3sVersion, "k3s-version", bootstrap.DefaultK3sVersion, "k3s version to install")
	addNodeCmd.Flags().IntVar(&addNodeF.count, "count", 1, "Number of nodes to add in parallel")
	addNodeCmd.Flags().StringVar(&addNodeF.tailscaleTag, "tailscale-tag", "tag:server", "ACL tag assigned to Tailscale devices (must exist in your tailnet ACL)")
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
	count := addNodeF.count
	if count < 1 {
		count = 1
	}

	// QEMU provider does not need cloud tokens — short-circuit early.
	if addNodeF.provider == qemu.Name {
		return runAddQEMUNodes(ctx, clusterName, count)
	}

	// Resolve infra tokens once; shared across all goroutines (read-only).
	hetznerToken, err := resolveToken("hetzner", "HETZNER_API_TOKEN")
	if err != nil {
		return fmt.Errorf("add-node: %w", err)
	}
	tsClientID, err := resolveToken("tailscale_client_id", "TAILSCALE_OAUTH_CLIENT_ID")
	if err != nil {
		return fmt.Errorf("add-node: %w", err)
	}
	tsClientSecret, err := resolveToken("tailscale_client_secret", "TAILSCALE_OAUTH_CLIENT_SECRET")
	if err != nil {
		return fmt.Errorf("add-node: %w", err)
	}

	prov, err := resolveProvider(addNodeF.provider, providerOptions{
		HetznerToken:          hetznerToken,
		TailscaleClientID:     tsClientID,
		TailscaleClientSecret: tsClientSecret,
		K3sVersion:            addNodeF.k3sVersion,
		HetznerRegion:         addNodeF.region,
		HetznerTailscaleTag:   addNodeF.tailscaleTag,
	}, nil)
	if err != nil {
		return fmt.Errorf("add-node: %w", err)
	}

	if err := addProviderWorkers(ctx, prov, clusterName, count); err != nil {
		return fmt.Errorf("add-node: %w", err)
	}

	// One reconcile pass after all nodes are up.
	runReconcileHook(ctx, ReconcileDeps{}, clusterName, hetznerToken)
	return nil
}

// addProviderWorkers adds count worker nodes to clusterName using the given
// provider. It is the common implementation for both add-node and up --nodes N.
func addProviderWorkers(ctx context.Context, prov provision.Provider, clusterName string, count int) error {
	type result struct {
		name string
		err  error
	}
	ch := make(chan result, count)
	for i := 0; i < count; i++ {
		go func() {
			name, err := prov.AddNode(ctx, clusterName)
			ch <- result{name, err}
		}()
	}

	var failedNames []string
	for range count {
		r := <-ch
		if r.err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "FAILED adding node to %q: %v\n", clusterName, r.err)
			failedNames = append(failedNames, clusterName)
		} else {
			_, _ = fmt.Fprintf(os.Stderr, "Node %q added to cluster %q\n", r.name, clusterName)
			recordNodeInRegistry(ctx, AddNodeDeps{}, clusterName, r.name)
		}
	}
	if len(failedNames) > 0 {
		return fmt.Errorf("%d of %d worker(s) failed to join %q", len(failedNames), count, clusterName)
	}
	return nil
}

// runAddQEMUNodes provisions count worker VMs for a QEMU cluster and joins
// them in parallel. Each worker is added by calling Provider.AddNode.
func runAddQEMUNodes(ctx context.Context, clusterName string, count int) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("add-node (qemu): resolve home dir: %w", err)
	}
	sshKeyPath := filepath.Join(home, ".ssh", "id_ed25519")

	p := qemu.New(qemu.Deps{SSHKeyPath: sshKeyPath})

	type result struct {
		name string
		err  error
	}
	ch := make(chan result, count)
	for i := 0; i < count; i++ {
		go func() {
			name, err := p.AddNode(ctx, clusterName)
			ch <- result{name, err}
		}()
	}
	var failedNames []string
	for range count {
		r := <-ch
		if r.err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "[%s] FAILED: %v\n", clusterName, r.err)
			failedNames = append(failedNames, clusterName)
		} else {
			_, _ = fmt.Fprintf(os.Stderr, "Node %q added to cluster %q\n", r.name, clusterName)
			recordNodeInRegistry(ctx, AddNodeDeps{}, clusterName, r.name)
		}
	}
	if len(failedNames) > 0 {
		return fmt.Errorf("add-node: %d of %d worker(s) failed to join %q", len(failedNames), count, clusterName)
	}
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
		_, _ = fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", err)
		return
	}
	defer func() {
		if cerr := reg.Close(); cerr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", cerr)
		}
	}()

	if err := reg.UpsertNode(ctx, registry.Node{
		ClusterName: clusterName,
		Hostname:    hostname,
		Role:        "worker",
		JoinedAt:    time.Now().UTC(),
	}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", err)
	}
}
