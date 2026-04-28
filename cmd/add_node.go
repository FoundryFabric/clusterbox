package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/provision/hetzner"
	"github.com/foundryfabric/clusterbox/internal/provision/qemu"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/tailscale"
	hcloudsdk "github.com/hetznercloud/hcloud-go/v2/hcloud"
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

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("add-node: resolve home dir: %w", err)
	}
	kubeconfigPath := filepath.Join(home, ".kube", clusterName+".yaml")

	// Generate unique names for this batch using Unix timestamp + index.
	batchTS := time.Now().Unix()
	nodeNames := make([]string, count)
	for i := range nodeNames {
		nodeNames[i] = fmt.Sprintf("%s-node-%d-%d", clusterName, batchTS, i)
	}

	if count == 1 {
		_, _ = fmt.Fprintf(os.Stderr, "Adding node %q to cluster %q...\n", nodeNames[0], clusterName)
	} else {
		_, _ = fmt.Fprintf(os.Stderr, "Adding %d nodes to cluster %q in parallel...\n", count, clusterName)
		for _, nn := range nodeNames {
			_, _ = fmt.Fprintf(os.Stderr, "  %s\n", nn)
		}
	}

	type nodeResult struct {
		nodeName string
		err      error
	}
	ch := make(chan nodeResult, count)

	for _, nn := range nodeNames {
		nn := nn
		go func() {
			ch <- nodeResult{
				nodeName: nn,
				err: addOneNode(ctx, nn, clusterName, hetznerToken,
					tsClientID, tsClientSecret, kubeconfigPath,
					addNodeF.region, addNodeF.k3sVersion, addNodeF.tailscaleTag),
			}
		}()
	}

	var failed []string
	for range nodeNames {
		r := <-ch
		if r.err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "[%s] FAILED: %v\n", r.nodeName, r.err)
			failed = append(failed, r.nodeName)
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("add-node: %d of %d node(s) failed: %s",
			len(failed), count, strings.Join(failed, ", "))
	}

	// One reconcile pass after all nodes are up.
	runReconcileHook(ctx, ReconcileDeps{}, clusterName, hetznerToken)

	if count == 1 {
		_, _ = fmt.Fprintf(os.Stderr, "Node %q successfully added to cluster %q.\n", nodeNames[0], clusterName)
	} else {
		_, _ = fmt.Fprintf(os.Stderr, "All %d nodes successfully added to cluster %q.\n", count, clusterName)
	}
	return nil
}

// addOneNode provisions and joins a single worker node. It is called
// concurrently by runAddNode when --count > 1. All log lines are prefixed
// with [nodeName] so interleaved output remains readable.
func addOneNode(ctx context.Context, nodeName, clusterName, hetznerToken, tsClientID, tsClientSecret, kubeconfigPath, region, k3sVersion, tailscaleTag string) error {
	logf := func(msg string) {
		_, _ = fmt.Fprintf(os.Stderr, "[%s] %s\n", nodeName, msg)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("add-node: resolve home dir: %w", err)
	}

	// Step 1: Tailscale ephemeral auth key.
	logf("[1/4] Generating Tailscale auth key...")
	tsAuthKey, err := tailscale.GenerateAuthKey(ctx, tsClientID, tsClientSecret, []string{tailscaleTag})
	if err != nil {
		return fmt.Errorf("[1/4] tailscale key: %w", err)
	}

	// Step 2: Provision VM via hcloud API.
	logf("[2/4] Provisioning VM via hcloud API...")
	cfg := provision.ClusterConfig{
		ClusterName:           nodeName,
		ClusterLabel:          clusterName,
		SnapshotName:          hetzner.SnapshotName,
		Location:              region,
		DNSDomain:             clusterName + ".foundryfabric.dev",
		TailscaleClientID:     tsClientID,
		TailscaleClientSecret: tsClientSecret,
		ResourceRole:          "worker",
	}
	sshPubKeyBytes, err := os.ReadFile(filepath.Join(home, ".ssh", "id_ed25519.pub"))
	if err != nil {
		return fmt.Errorf("[2/4] read ssh pub key: %w", err)
	}
	workerSpec := &config.Spec{Hostname: nodeName}
	specYAML, err := yaml.Marshal(workerSpec)
	if err != nil {
		return fmt.Errorf("[2/4] marshal spec: %w", err)
	}
	configB64 := base64.StdEncoding.EncodeToString(specYAML)
	userData, err := hetzner.RenderCloudInit(strings.TrimSpace(string(sshPubKeyBytes)), configB64, tsAuthKey, nodeName)
	if err != nil {
		return fmt.Errorf("[2/4] render cloud-init: %w", err)
	}
	hcloudClient := hcloudsdk.NewClient(hcloudsdk.WithToken(hetznerToken))
	if _, err := hetzner.CreateClusterResources(ctx, hcloudClient, cfg, userData, nil); err != nil {
		return fmt.Errorf("[2/4] provision: %w", err)
	}

	// Step 3: k3sup join.
	logf("[3/4] Joining cluster via k3sup...")
	joinCfg := bootstrap.JoinConfig{
		NodeIP:         nodeName,
		ServerIP:       clusterName,
		K3sVersion:     k3sVersion,
		KubeconfigPath: kubeconfigPath,
		SSHKeyPath:     filepath.Join(home, ".ssh", "id_ed25519"),
	}
	if err := bootstrap.Join(ctx, joinCfg); err != nil {
		return fmt.Errorf("[3/4] join: %w", err)
	}

	logf("[4/4] Node joined and Ready.")
	recordNodeInRegistry(ctx, AddNodeDeps{}, clusterName, nodeName)
	return nil
}

// runAddQEMUNodes provisions count worker VMs for a QEMU cluster and joins
// them in parallel. Each worker is added by calling Provider.AddNode.
func runAddQEMUNodes(ctx context.Context, clusterName string, count int) error {
	home, _ := os.UserHomeDir()
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
	var failed []string
	for range count {
		r := <-ch
		if r.err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "[%s] FAILED: %v\n", clusterName, r.err)
			failed = append(failed, r.err.Error())
		} else {
			_, _ = fmt.Fprintf(os.Stderr, "Node %q added to cluster %q\n", r.name, clusterName)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("add-node: %d of %d failed", len(failed), count)
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
