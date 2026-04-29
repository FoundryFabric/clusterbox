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

	"github.com/foundryfabric/clusterbox/internal/agentbundle"
	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/provision/hetzner"
	"github.com/foundryfabric/clusterbox/internal/provision/nodeinstall"
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

	if err := addHetznerWorkers(ctx, clusterName, count, hetznerToken,
		tsClientID, tsClientSecret, kubeconfigPath,
		addNodeF.region, addNodeF.k3sVersion, addNodeF.tailscaleTag); err != nil {
		return fmt.Errorf("add-node: %w", err)
	}

	// One reconcile pass after all nodes are up.
	runReconcileHook(ctx, ReconcileDeps{}, clusterName, hetznerToken)
	return nil
}

// addHetznerWorkers provisions `count` Hetzner worker nodes for clusterName in
// parallel and joins each to the k3s cluster. It is shared by add-node and
// up --nodes N>1 so multi-node creation is consistent across both entry points.
func addHetznerWorkers(ctx context.Context, clusterName string, count int,
	hetznerToken, tsClientID, tsClientSecret, kubeconfigPath,
	region, k3sVersion, tailscaleTag string,
) error {
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
					region, k3sVersion, tailscaleTag),
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
		return fmt.Errorf("%d of %d node(s) failed: %s",
			len(failed), count, strings.Join(failed, ", "))
	}

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
//
// Join flow:
//  1. Generate Tailscale ephemeral auth key (for the worker's tailnet join).
//  2. Look up control-plane private IP and the cluster network ID from hcloud.
//  3. Provision the worker VM attached to the cluster private network.
//  4. Wait for Tailscale SSH to become available on the worker.
//  5. Read the k3s node-token from the control plane via Tailscale SSH.
//  6. Run clusterboxnode with role=agent on the worker, binding k3s on the
//     private IP so all node-to-node and Flannel traffic stays off Tailscale.
func addOneNode(ctx context.Context, nodeName, clusterName, hetznerToken, tsClientID, tsClientSecret, kubeconfigPath, region, k3sVersion, tailscaleTag string) error {
	logf := func(msg string) {
		_, _ = fmt.Fprintf(os.Stderr, "[%s] %s\n", nodeName, msg)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("add-node: resolve home dir: %w", err)
	}
	sshKeyPath := filepath.Join(home, ".ssh", "id_ed25519")

	// Step 1: Tailscale ephemeral auth key.
	logf("[1/6] Generating Tailscale auth key...")
	tsAuthKey, err := tailscale.GenerateAuthKey(ctx, tsClientID, tsClientSecret, []string{tailscaleTag})
	if err != nil {
		return fmt.Errorf("[1/6] tailscale key: %w", err)
	}

	// Step 2: Look up the cluster's private network and the control-plane's
	// private IP. Both are required before we can provision the worker.
	logf("[2/6] Resolving cluster network and control-plane private IP...")
	hcloudClient := hcloudsdk.NewClient(hcloudsdk.WithToken(hetznerToken))
	cpServer, _, err := hcloudClient.Server.GetByName(ctx, clusterName)
	if err != nil {
		return fmt.Errorf("[2/6] lookup control-plane: %w", err)
	}
	if cpServer == nil {
		return fmt.Errorf("[2/6] control-plane server %q not found in hcloud", clusterName)
	}
	if len(cpServer.PrivateNet) == 0 {
		return fmt.Errorf("[2/6] control-plane %q has no private network attachment", clusterName)
	}
	cpPrivateIP := cpServer.PrivateNet[0].IP.String()
	networkID := cpServer.PrivateNet[0].Network.ID

	// Step 3: Provision the worker VM attached to the cluster private network.
	logf("[3/6] Provisioning worker VM...")
	sshPubKeyBytes, err := os.ReadFile(sshKeyPath + ".pub")
	if err != nil {
		return fmt.Errorf("[3/6] read ssh pub key: %w", err)
	}
	// Placeholder spec for cloud-init — the real k3s spec is uploaded in step 6.
	placeholderSpec := &config.Spec{Hostname: nodeName}
	placeholderYAML, err := yaml.Marshal(placeholderSpec)
	if err != nil {
		return fmt.Errorf("[3/6] marshal placeholder spec: %w", err)
	}
	configB64 := base64.StdEncoding.EncodeToString(placeholderYAML)
	userData, err := hetzner.RenderCloudInit(strings.TrimSpace(string(sshPubKeyBytes)), configB64, tsAuthKey, nodeName)
	if err != nil {
		return fmt.Errorf("[3/6] render cloud-init: %w", err)
	}
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
	// CreateClusterResources always creates the network for the cluster label;
	// since the network already exists for this cluster it will be reused.
	// We override the network ID via the existing server lookup above — but
	// CreateClusterResources still needs to call ensureClusterNetwork to attach
	// the server. The cluster label routes it to the correct existing network.
	_ = networkID // used implicitly via CreateClusterResources → ensureClusterNetwork
	createResult, err := hetzner.CreateClusterResources(ctx, hcloudClient, cfg, userData, nil)
	if err != nil {
		return fmt.Errorf("[3/6] provision: %w", err)
	}
	if createResult.PrivateIP == "" {
		return fmt.Errorf("[3/6] worker has no private network IP")
	}
	workerPrivateIP := createResult.PrivateIP

	// Step 4: Wait for Tailscale SSH on the worker.
	logf("[4/6] Waiting for Tailscale SSH (up to 10 min)...")
	workerSSH := nodeinstall.SSHConfig{
		Host:    nodeName,
		Port:    22,
		User:    "ubuntu",
		KeyPath: sshKeyPath,
	}
	if err := nodeinstall.WaitForSSH(ctx, workerSSH, 10*time.Minute, os.Stderr); err != nil {
		return fmt.Errorf("[4/6] ssh wait: %w", err)
	}

	// Step 5: Read the k3s node-token from the control plane via Tailscale SSH.
	// The control plane is reachable by its Tailscale hostname (clusterName).
	logf("[5/6] Reading node-token from control-plane...")
	cpSSH := nodeinstall.SSHConfig{
		Host:    clusterName,
		Port:    22,
		User:    "ubuntu",
		KeyPath: sshKeyPath,
	}
	nodeToken, err := nodeinstall.ReadRemoteFile(ctx, cpSSH, "/var/lib/rancher/k3s/server/node-token")
	if err != nil {
		return fmt.Errorf("[5/6] read node-token: %w", err)
	}
	nodeToken = strings.TrimSpace(nodeToken)
	if nodeToken == "" {
		return fmt.Errorf("[5/6] node-token is empty on control-plane")
	}

	// Step 6: Run clusterboxnode as a k3s agent on the worker.
	// k3s binds on the worker's private IP and connects to the control-plane
	// via the private IP so all cluster traffic stays off the Tailscale tunnel.
	logf("[6/6] Running clusterboxnode agent (joining via private network)...")
	agentSpec := &config.Spec{
		Hostname: nodeName,
		K3s: &config.K3sSpec{
			Enabled:      true,
			Role:         "agent",
			Version:      k3sVersion,
			ServerURL:    "https://" + cpPrivateIP + ":6443",
			Token:        nodeToken,
			NodeIP:       workerPrivateIP,
			FlannelIface: hetzner.HetznerPrivateIface,
			NodeLabels:   []string{"node-role.kubernetes.io/worker=worker"},
		},
	}
	agentSpecYAML, err := yaml.Marshal(agentSpec)
	if err != nil {
		return fmt.Errorf("[6/6] marshal agent spec: %w", err)
	}
	arch, err := nodeinstall.ProbeArch(ctx, workerSSH)
	if err != nil {
		return fmt.Errorf("[6/6] probe arch: %w", err)
	}
	agentBytes, err := agentbundle.ForArch(arch)
	if err != nil {
		return fmt.Errorf("[6/6] agent bundle: %w", err)
	}
	stdout, err := nodeinstall.RunAgent(ctx, workerSSH, agentBytes, agentSpecYAML, os.Stderr)
	if err != nil {
		return fmt.Errorf("[6/6] run agent: %w", err)
	}
	parsed, err := nodeinstall.ParseInstallOutput(stdout)
	if err != nil {
		return fmt.Errorf("[6/6] parse agent output: %w", err)
	}
	if parsed.IsError() {
		return fmt.Errorf("[6/6] agent install failed: %v", parsed.AsError(0, nil))
	}

	logf("[6/6] Node joined and Ready.")
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
			recordNodeInRegistry(ctx, AddNodeDeps{}, clusterName, r.name)
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
