package hetzner

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/foundryfabric/clusterbox/internal/agentbundle"
	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/provision/nodeinstall"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/tailscale"
	hcloudsdk "github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// Name is the canonical provider identifier accepted by the
// `--provider` CLI flag.
const Name = "hetzner"

// SnapshotName is the Hetzner image every clusterbox node boots from.
// Pinned here so cmd-side dispatchers do not need to know it.
const SnapshotName = "ubuntu-24.04"

// Deps groups the injectable dependencies the Hetzner provider relies
// on. Tests and alternate cmd-paths (e.g. add-node, remove-node)
// substitute fields; nil fields fall back to production defaults.
type Deps struct {
	// HetznerToken is the Hetzner Cloud API token used for SDK calls.
	// When empty Provider falls back to the HETZNER_API_TOKEN env var.
	HetznerToken string

	// KubeconfigPath is the on-disk path the kubeconfig is written to.
	// When empty the provider derives $HOME/.kube/<clusterName>.yaml.
	KubeconfigPath string

	// SSHKeyPath is the private key used to SSH into the node over
	// Tailscale once cloud-init has completed. When empty the provider
	// derives $HOME/.ssh/id_ed25519.
	SSHKeyPath string

	// SSHPubKey is the public key content written to the cloud-init
	// users block. When empty the provider reads SSHKeyPath + ".pub".
	SSHPubKey string

	// K3sVersion is the k3s release to install. When empty
	// bootstrap.DefaultK3sVersion is used as the default.
	K3sVersion string

	// AgentBundleForArch returns the embedded clusterboxnode binary bytes
	// for the given linux arch. Defaults to agentbundle.ForArch.
	AgentBundleForArch func(arch string) ([]byte, error)

	// Out is the destination for human-readable progress lines.
	// When nil the provider writes to os.Stderr.
	Out io.Writer

	// CreateResources provisions all Hetzner Cloud resources for one node.
	// Defaults to CreateClusterResources.
	CreateResources func(ctx context.Context, client *hcloudsdk.Client, cfg provision.ClusterConfig, userData string, onCreated OnResourceCreated) (CreateResult, error)

	// NewLister builds an HCloudResourceLister around hetznerToken.
	// Defaults to wrapping hcloud.NewClient.
	NewLister func(token string) HCloudResourceLister

	// DeleteResource directly removes a single Hetzner resource by
	// (type, id) using the SDK. Defaults to deleteHCloudResource.
	DeleteResource func(ctx context.Context, token string, resourceType registry.ResourceType, hetznerID string) error

	// OpenRegistry opens the local registry. Defaults to registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)

	// TailscaleClientID and TailscaleClientSecret are the OAuth credentials
	// used both to generate ephemeral auth keys at provision time and to
	// delete devices from the tailnet at destroy time.
	TailscaleClientID     string
	TailscaleClientSecret string

	// GenerateTailscaleAuthKey produces an ephemeral Tailscale auth
	// key from the OAuth credentials in cfg. Defaults to
	// tailscale.GenerateAuthKey.
	GenerateTailscaleAuthKey func(ctx context.Context, clientID, clientSecret string, tags []string) (string, error)

	// DeleteTailscaleDevice removes a device from the tailnet by hostname.
	// Defaults to tailscale.DeleteDevice. Tests can inject a no-op.
	DeleteTailscaleDevice func(ctx context.Context, clientID, clientSecret, hostname string) error

	// FindTailscaleDeviceID looks up the Tailscale device ID for a hostname.
	// Defaults to tailscale.FindDeviceID. Used at provision time to record
	// the device in the registry inventory.
	FindTailscaleDeviceID func(ctx context.Context, clientID, clientSecret, hostname string) (string, error)

	// Region is the Hetzner datacenter location used when AddNode creates a
	// new worker server (e.g. "ash", "fsn1"). Defaults to "ash".
	Region string

	// TailscaleTag is the ACL tag applied to Tailscale devices created by
	// AddNode (e.g. "tag:server"). Defaults to "tag:server".
	TailscaleTag string
}

// Provider is the Hetzner Cloud implementation of provision.Provider.
//
// A zero-value Provider is usable: every Deps field defaults to the
// production wiring. Callers (cmd/up, cmd/destroy) construct it with
// New(...) and pass it through the provision.Provider interface so
// dispatch by --provider flag stays uniform across providers.
type Provider struct {
	deps Deps
}

// New constructs a Hetzner provider with the given dependencies. Nil
// fields on deps fall back to production defaults, so passing a
// zero-value Deps yields a ready-to-use provider.
func New(deps Deps) *Provider {
	return &Provider{deps: deps}
}

// Name returns the canonical provider identifier ("hetzner").
func (p *Provider) Name() string { return Name }

// Provision stands up a Hetzner-Cloud-hosted cluster.
//
// Flow:
//  1. Generate Tailscale ephemeral auth key.
//  2. Build the clusterboxnode spec (tailscale + k3s) and render cloud-init
//     that downloads and runs clusterboxnode on first boot.
//  3. Provision Hetzner resources (VM + volume + firewall).
//  4. Wait for Tailscale SSH (cloud-init ran tailscale up via clusterboxnode).
//  5. Poll for /etc/rancher/k3s/k3s.yaml (cloud-init ran k3s install via
//     clusterboxnode), rewrite the server URL, and write the kubeconfig.
func (p *Provider) Provision(ctx context.Context, cfg provision.ClusterConfig) (provision.ProvisionResult, error) {
	out := p.out()
	hetznerToken := p.hetznerToken()

	kubeconfigPath, err := p.kubeconfigPath(cfg.ClusterName)
	if err != nil {
		return provision.ProvisionResult{}, err
	}
	sshKeyPath, err := p.sshKeyPath()
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	// Step 1: Generate Tailscale ephemeral auth key.
	_, _ = fmt.Fprintln(out, "[1/5] Generating Tailscale auth key...")
	genTSKey := p.deps.GenerateTailscaleAuthKey
	if genTSKey == nil {
		genTSKey = tailscale.GenerateAuthKey
	}
	tsAuthKey, err := genTSKey(ctx, cfg.TailscaleClientID, cfg.TailscaleClientSecret, cfg.TailscaleTags)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("[1/5] failed: %w", err)
	}

	// Step 2: Render cloud-init with a placeholder spec. The spec baked into
	// cloud-init only needs to seed /etc/clusterboxnode.yaml — the real spec
	// (with private IP, flannel-iface) is uploaded fresh by RunAgent in step 5
	// after we know the private IP assigned by Hetzner.
	_, _ = fmt.Fprintln(out, "[2/5] Building cloud-init...")
	k3sVersion := p.deps.K3sVersion
	if k3sVersion == "" {
		k3sVersion = bootstrap.DefaultK3sVersion
	}
	sshPubKey, err := p.loadSSHPubKey(sshKeyPath)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("[2/5] %w", err)
	}
	// Minimal placeholder spec for cloud-init. The actual k3s spec (NodeIP,
	// FlannelIface, TLSSANs) is finalized after the private IP is known.
	placeholderSpec := &config.Spec{Hostname: cfg.ClusterName}
	placeholderYAML, err := yaml.Marshal(placeholderSpec)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("[2/5] marshal placeholder spec: %w", err)
	}
	configB64 := base64.StdEncoding.EncodeToString(placeholderYAML)
	userData, err := RenderCloudInit(sshPubKey, configB64, tsAuthKey, cfg.ClusterName)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("[2/5] failed: %w", err)
	}

	// Step 3: Provision Hetzner resources (network + VM + volume + firewall).
	_, _ = fmt.Fprintln(out, "[3/5] Provisioning Hetzner resources (network + VM + firewall)...")
	createResources := p.deps.CreateResources
	if createResources == nil {
		createResources = CreateClusterResources
	}
	hcloudClient := hcloudsdk.NewClient(hcloudsdk.WithToken(hetznerToken))

	// Register the cluster row before creating any resources so that destroy
	// can find it even if provision fails partway through.
	openRegistry := p.deps.OpenRegistry
	if openRegistry == nil {
		openRegistry = registry.NewRegistry
	}
	reg, regErr := openRegistry(ctx)
	if regErr != nil {
		_, _ = fmt.Fprintf(out, "warning: open registry before provision: %v\n", regErr)
		reg = nil
	}
	if reg != nil {
		if uErr := reg.UpsertCluster(ctx, registry.Cluster{
			Name:     cfg.ClusterName,
			Provider: Name,
			Region:   cfg.Location,
			Env:      cfg.Env,
		}); uErr != nil {
			_, _ = fmt.Fprintf(out, "warning: register cluster before provision: %v\n", uErr)
		}
	}

	// onCreated writes each resource to the registry immediately after it is
	// created so destroy can recover from partial failures.
	onCreated := func(rt registry.ResourceType, hetznerID, hostname string) {
		if reg == nil {
			return
		}
		if _, wErr := reg.RecordResource(ctx, registry.ClusterResource{
			ClusterName:  cfg.ClusterName,
			Provider:     registry.ProviderHetzner,
			ResourceType: rt,
			ExternalID:   hetznerID,
			Hostname:     hostname,
		}); wErr != nil {
			_, _ = fmt.Fprintf(out, "warning: record resource %s/%s: %v\n", rt, hetznerID, wErr)
		}
	}

	createResult, err := createResources(ctx, hcloudClient, cfg, userData, onCreated)
	if err != nil {
		if reg != nil {
			_ = reg.Close()
		}
		return provision.ProvisionResult{}, fmt.Errorf("[3/5] failed: %w", err)
	}
	if reg != nil {
		_ = reg.Close()
	}
	if createResult.PrivateIP == "" {
		return provision.ProvisionResult{}, fmt.Errorf("[3/5] server has no private network IP — ensure the cluster network was created")
	}

	// Build the full k3s spec now that we know the private IP. k3s binds on
	// the private IP so all node-to-node and Flannel VXLAN traffic uses the
	// Hetzner private network (eth1) instead of the Tailscale tunnel.
	// The kubeconfig server URL is rewritten to the Tailscale hostname so
	// operators can reach the API server from outside the private network.
	spec := &config.Spec{
		Hostname: cfg.ClusterName,
		K3s: &config.K3sSpec{
			Enabled:      true,
			Role:         "server-init",
			Version:      k3sVersion,
			NodeIP:       createResult.PrivateIP,
			FlannelIface: HetznerPrivateIface,
			TLSSANs:      []string{cfg.ClusterName, createResult.PrivateIP},
		},
	}
	specYAML, err := yaml.Marshal(spec)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("[3/5] marshal k3s spec: %w", err)
	}

	// Step 4: Wait for Tailscale SSH — cloud-init must finish before we can
	// upload and run clusterboxnode.
	sshCfg := nodeinstall.SSHConfig{
		Host:    cfg.ClusterName,
		Port:    22,
		User:    "ubuntu",
		KeyPath: sshKeyPath,
	}
	_, _ = fmt.Fprintf(out, "[4/5] Waiting for Tailscale SSH on %s (up to 10 min)...\n", cfg.ClusterName)
	if err := nodeinstall.WaitForSSH(ctx, sshCfg, 10*time.Minute, out); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("[4/5] failed: %w", err)
	}

	// Step 5: Upload and run clusterboxnode to install k3s server-init.
	_, _ = fmt.Fprintln(out, "[5/5] Uploading and running clusterboxnode via SSH...")
	loader := p.deps.AgentBundleForArch
	if loader == nil {
		loader = agentbundle.ForArch
	}
	result, err := nodeinstall.RunNodeAgent(ctx, sshCfg, specYAML, loader, out)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("[5/5] run agent: %w", err)
	}
	if result.KubeconfigYAML == "" {
		return provision.ProvisionResult{}, fmt.Errorf("[5/5] install output missing kubeconfig_yaml")
	}
	rewritten, err := nodeinstall.RewriteKubeconfigServer(result.KubeconfigYAML, cfg.ClusterName)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("[5/5] rewrite kubeconfig: %w", err)
	}
	if err := nodeinstall.WriteKubeconfig(kubeconfigPath, rewritten, out); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("[5/5] failed: %w", err)
	}

	// Record the Tailscale device in the inventory so destroy has the ID.
	if cfg.TailscaleClientID != "" && cfg.TailscaleClientSecret != "" {
		findDevice := p.deps.FindTailscaleDeviceID
		if findDevice == nil {
			findDevice = tailscale.FindDeviceID
		}
		if deviceID, ferr := findDevice(ctx, cfg.TailscaleClientID, cfg.TailscaleClientSecret, cfg.ClusterName); ferr == nil && deviceID != "" {
			if reg2, rerr := openRegistry(ctx); rerr == nil {
				if _, werr := reg2.RecordResource(ctx, registry.ClusterResource{
					ClusterName:  cfg.ClusterName,
					Provider:     registry.ProviderTailscale,
					ResourceType: registry.ResourceDevice,
					ExternalID:   deviceID,
					Hostname:     cfg.ClusterName,
				}); werr != nil {
					_, _ = fmt.Fprintf(out, "warning: record Tailscale device: %v\n", werr)
				}
				_ = reg2.Close()
			}
		} else if ferr != nil {
			_, _ = fmt.Fprintf(out, "warning: find Tailscale device ID: %v\n", ferr)
		}
	}

	_, _ = fmt.Fprintf(out, "Cluster %q is up. Kubeconfig: %s\n", cfg.ClusterName, kubeconfigPath)
	return provision.ProvisionResult{
		KubeconfigPath: kubeconfigPath,
		Nodes: []registry.Node{
			{
				ClusterName: cfg.ClusterName,
				Hostname:    cfg.ClusterName,
				Role:        "control-plane",
				JoinedAt:    time.Now().UTC(),
			},
		},
	}, nil
}

// loadSSHPubKey returns the SSH public key content for cloud-init injection.
// Uses deps.SSHPubKey when set; otherwise reads sshKeyPath + ".pub".
func (p *Provider) loadSSHPubKey(sshKeyPath string) (string, error) {
	if p.deps.SSHPubKey != "" {
		return p.deps.SSHPubKey, nil
	}
	data, err := os.ReadFile(sshKeyPath + ".pub")
	if err != nil {
		return "", fmt.Errorf("hetzner: read ssh pub key %s.pub: %w", sshKeyPath, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// Destroy tears down every Hetzner-side resource owned by the cluster:
// it reconciles the local inventory against hcloud (surfacing drift on
// stderr), and then sweeps any stragglers via direct SDK delete calls.
//
// The cmd-side caller is expected to have already prompted the user,
// printed the destruction plan, and resolved the registry row for the
// cluster. Destroy never modifies the cluster or node rows; that
// stays in cmd/destroy.go so the registry layer is the single owner
// of cluster-level state transitions.
func (p *Provider) Destroy(ctx context.Context, cluster registry.Cluster) error {
	out := p.out()
	hetznerToken := p.hetznerToken()
	clusterName := cluster.Name

	openRegistry := p.deps.OpenRegistry
	if openRegistry == nil {
		openRegistry = registry.NewRegistry
	}
	reg, err := openRegistry(ctx)
	if err != nil {
		return fmt.Errorf("destroy: open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	// Step 1: Reconcile — confirms what is still alive in hcloud, surfaces drift.
	_, _ = fmt.Fprintln(out, "[1/3] Reconciling local inventory against Hetzner...")
	newLister := p.deps.NewLister
	if newLister == nil {
		newLister = func(token string) HCloudResourceLister {
			return NewHCloudLister(hcloudsdk.NewClient(hcloudsdk.WithToken(token)))
		}
	}
	r := &Reconciler{Registry: reg, Lister: newLister(hetznerToken)}
	summary, err := r.Reconcile(ctx, clusterName)
	if err != nil {
		_, _ = fmt.Fprintf(out, "warning: reconciler failed: %v\n", err)
	} else {
		_, _ = fmt.Fprintf(out,
			"reconciler: added=%d existing=%d marked_destroyed=%d unmanaged=%d\n",
			summary.Added, summary.Existing, summary.MarkedDestroyed, len(summary.Unmanaged),
		)
		if len(summary.Unmanaged) > 0 {
			_, _ = fmt.Fprintf(out, "warning: %d unmanaged resources detected: %v (not auto-deleted)\n", len(summary.Unmanaged), summary.Unmanaged)
		}
	}

	// Step 2: Sweep stragglers — anything still active in the
	// registry after reconcile is a leaked resource. Try a direct SDK
	// delete and tombstone the row regardless so a re-run converges.
	deleteResource := p.deps.DeleteResource
	if deleteResource == nil {
		deleteResource = deleteHCloudResource
	}
	stragglers, err := reg.ListResources(ctx, clusterName, false)
	if err != nil {
		_, _ = fmt.Fprintf(out, "warning: list stragglers: %v\n", err)
		stragglers = nil
	}
	// Filter out non-Hetzner resources (e.g. Tailscale devices) — they are
	// handled separately in step 3 and cannot be deleted via the hcloud SDK.
	hetznerStragglers := stragglers[:0:0]
	for _, r := range stragglers {
		if r.Provider == registry.ProviderHetzner {
			hetznerStragglers = append(hetznerStragglers, r)
		}
	}
	// Delete in waves so dependency ordering is respected: servers must be
	// fully gone before volumes and firewalls can be removed (Hetzner rejects
	// deleting resources that are still in use).
	waves := deletionWaves(hetznerStragglers)
	total := len(hetznerStragglers)
	_, _ = fmt.Fprintf(out, "[2/3] Sweeping %d straggler(s) in %d wave(s)...\n", total, len(waves))
	for _, wave := range waves {
		for _, row := range wave {
			if err := deleteResource(ctx, hetznerToken, row.ResourceType, row.ExternalID); err != nil {
				_, _ = fmt.Fprintf(out, "warning: direct delete %s/%s failed: %v\n", row.ResourceType, row.ExternalID, err)
			}
			if err := reg.MarkResourceDestroyed(ctx, row.ID, time.Now().UTC()); err != nil {
				_, _ = fmt.Fprintf(out, "warning: tombstone resource id=%d: %v\n", row.ID, err)
			}
		}
	}

	// Step 3: Remove the Tailscale device so it doesn't linger in the tailnet.
	tsClientID := p.deps.TailscaleClientID
	tsClientSecret := p.deps.TailscaleClientSecret
	if tsClientID != "" && tsClientSecret != "" {
		_, _ = fmt.Fprintf(out, "[3/3] Removing Tailscale device %q...\n", clusterName)
		deleteDevice := p.deps.DeleteTailscaleDevice
		if deleteDevice == nil {
			deleteDevice = tailscale.DeleteDevice
		}
		if err := deleteDevice(ctx, tsClientID, tsClientSecret, clusterName); err != nil {
			_, _ = fmt.Fprintf(out, "warning: remove Tailscale device: %v\n", err)
		}
		// Tombstone the Tailscale device registry row(s) if present.
		if allRows, lerr := reg.ListResources(ctx, clusterName, false); lerr == nil {
			for _, row := range allRows {
				if row.Provider == registry.ProviderTailscale {
					_ = reg.MarkResourceDestroyed(ctx, row.ID, time.Now().UTC())
				}
			}
		}
	} else {
		_, _ = fmt.Fprintln(out, "[3/3] Tailscale credentials not configured — skipping device removal.")
	}

	return nil
}

// Reconcile walks hcloud and brings the local registry into
// agreement with reality. It is a thin wrapper around the package's
// Reconciler so cmd-side callers can dispatch by provider.
func (p *Provider) Reconcile(ctx context.Context, clusterName string) (provision.ReconcileSummary, error) {
	openRegistry := p.deps.OpenRegistry
	if openRegistry == nil {
		openRegistry = registry.NewRegistry
	}
	reg, err := openRegistry(context.Background())
	if err != nil {
		return provision.ReconcileSummary{}, fmt.Errorf("reconcile: open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	newLister := p.deps.NewLister
	if newLister == nil {
		newLister = func(token string) HCloudResourceLister {
			return NewHCloudLister(hcloudsdk.NewClient(hcloudsdk.WithToken(token)))
		}
	}

	r := &Reconciler{Registry: reg, Lister: newLister(p.hetznerToken())}
	s, err := r.Reconcile(ctx, clusterName)
	if err != nil {
		return provision.ReconcileSummary{}, err
	}
	return provision.ReconcileSummary{
		Added:           s.Added,
		Existing:        s.Existing,
		MarkedDestroyed: s.MarkedDestroyed,
		Unmanaged:       s.Unmanaged,
	}, nil
}

// out returns the human-readable output sink, falling back to stderr.
func (p *Provider) out() io.Writer {
	if p.deps.Out != nil {
		return p.deps.Out
	}
	return os.Stderr
}

// hetznerToken returns the configured Hetzner API token, falling
// back to HETZNER_API_TOKEN.
func (p *Provider) hetznerToken() string {
	if p.deps.HetznerToken != "" {
		return p.deps.HetznerToken
	}
	return os.Getenv("HETZNER_API_TOKEN")
}

// kubeconfigPath returns the configured kubeconfig path, falling
// back to ~/.kube/<clusterName>.yaml.
func (p *Provider) kubeconfigPath(clusterName string) (string, error) {
	if p.deps.KubeconfigPath != "" {
		return p.deps.KubeconfigPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("hetzner: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".kube", clusterName+".yaml"), nil
}

// sshKeyPath returns the configured SSH private key path, falling
// back to ~/.ssh/id_ed25519.
func (p *Provider) sshKeyPath() (string, error) {
	if p.deps.SSHKeyPath != "" {
		return p.deps.SSHKeyPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("hetzner: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".ssh", "id_ed25519"), nil
}

// deleteHCloudResource is the production DeleteResource implementation:
// a thin dispatcher over hcloud-go's per-type Delete methods. ID
// strings originate from the registry (which stored them as decimal
// strings on insert) so a parse failure is treated as a hard error.
func deleteHCloudResource(ctx context.Context, token string, resourceType registry.ResourceType, hetznerID string) error {
	id, err := strconv.ParseInt(hetznerID, 10, 64)
	if err != nil {
		return fmt.Errorf("parse hetzner id %q: %w", hetznerID, err)
	}
	c := hcloudsdk.NewClient(hcloudsdk.WithToken(token))

	switch resourceType {
	case registry.ResourceServer:
		result, _, err := c.Server.DeleteWithResult(ctx, &hcloudsdk.Server{ID: id})
		if err != nil {
			return err
		}
		return c.Action.WaitFor(ctx, result.Action)
	case registry.ResourceLoadBalancer:
		_, err := c.LoadBalancer.Delete(ctx, &hcloudsdk.LoadBalancer{ID: id})
		return err
	case registry.ResourceSSHKey:
		_, err := c.SSHKey.Delete(ctx, &hcloudsdk.SSHKey{ID: id})
		return err
	case registry.ResourceFirewall:
		_, err := c.Firewall.Delete(ctx, &hcloudsdk.Firewall{ID: id})
		return err
	case registry.ResourceNetwork:
		_, err := c.Network.Delete(ctx, &hcloudsdk.Network{ID: id})
		return err
	case registry.ResourceVolume:
		_, err := c.Volume.Delete(ctx, &hcloudsdk.Volume{ID: id})
		return err
	case registry.ResourcePrimaryIP:
		_, err := c.PrimaryIP.Delete(ctx, &hcloudsdk.PrimaryIP{ID: id})
		return err
	default:
		return fmt.Errorf("delete: unknown resource type %q", resourceType)
	}
}

// AddNode provisions a single Hetzner worker VM and joins it to clusterName.
// Returns the canonical node hostname on success.
//
// The join flow mirrors cmd/add_node.go::addOneNode but lives here so the
// provider interface is the single dispatch point for node lifecycle.
func (p *Provider) AddNode(ctx context.Context, clusterName string) (string, error) {
	out := p.out()
	hetznerToken := p.hetznerToken()
	sshKeyPath, err := p.sshKeyPath()
	if err != nil {
		return "", err
	}

	region := p.deps.Region
	if region == "" {
		region = "ash"
	}
	tailscaleTag := p.deps.TailscaleTag
	if tailscaleTag == "" {
		tailscaleTag = "tag:server"
	}
	k3sVersion := p.deps.K3sVersion
	if k3sVersion == "" {
		k3sVersion = bootstrap.DefaultK3sVersion
	}

	nodeName := fmt.Sprintf("%s-node-%d", clusterName, time.Now().UnixMilli())

	logf := func(msg string) { _, _ = fmt.Fprintf(out, "[%s] %s\n", nodeName, msg) }

	// Step 1: Generate Tailscale ephemeral auth key.
	logf("[1/6] Generating Tailscale auth key...")
	genTSKey := p.deps.GenerateTailscaleAuthKey
	if genTSKey == nil {
		genTSKey = tailscale.GenerateAuthKey
	}
	tsAuthKey, err := genTSKey(ctx, p.deps.TailscaleClientID, p.deps.TailscaleClientSecret, []string{tailscaleTag})
	if err != nil {
		return "", fmt.Errorf("[1/6] tailscale key: %w", err)
	}

	// Step 2: Look up the cluster's private network and the control-plane's
	// private IP. Both are required before we can provision the worker.
	logf("[2/6] Resolving cluster network and control-plane private IP...")
	hcloudClient := hcloudsdk.NewClient(hcloudsdk.WithToken(hetznerToken))
	cpServer, _, err := hcloudClient.Server.GetByName(ctx, clusterName)
	if err != nil {
		return "", fmt.Errorf("[2/6] lookup control-plane: %w", err)
	}
	if cpServer == nil {
		return "", fmt.Errorf("[2/6] control-plane server %q not found in hcloud", clusterName)
	}
	if len(cpServer.PrivateNet) == 0 {
		return "", fmt.Errorf("[2/6] control-plane %q has no private network attachment", clusterName)
	}
	cpPrivateIP := cpServer.PrivateNet[0].IP.String()

	// Step 3: Provision the worker VM attached to the cluster private network.
	logf("[3/6] Provisioning worker VM...")
	sshPubKey, err := p.loadSSHPubKey(sshKeyPath)
	if err != nil {
		return "", fmt.Errorf("[3/6] %w", err)
	}
	placeholderSpec := &config.Spec{Hostname: nodeName}
	placeholderYAML, err := yaml.Marshal(placeholderSpec)
	if err != nil {
		return "", fmt.Errorf("[3/6] marshal placeholder spec: %w", err)
	}
	configB64 := base64.StdEncoding.EncodeToString(placeholderYAML)
	userData, err := RenderCloudInit(sshPubKey, configB64, tsAuthKey, nodeName)
	if err != nil {
		return "", fmt.Errorf("[3/6] render cloud-init: %w", err)
	}
	workerCfg := provision.ClusterConfig{
		ClusterName:           nodeName,
		ClusterLabel:          clusterName,
		SnapshotName:          SnapshotName,
		Location:              region,
		DNSDomain:             clusterName + ".foundryfabric.dev",
		TailscaleClientID:     p.deps.TailscaleClientID,
		TailscaleClientSecret: p.deps.TailscaleClientSecret,
		ResourceRole:          "worker",
	}
	createResources := p.deps.CreateResources
	if createResources == nil {
		createResources = CreateClusterResources
	}
	createResult, err := createResources(ctx, hcloudClient, workerCfg, userData, nil)
	if err != nil {
		return "", fmt.Errorf("[3/6] provision: %w", err)
	}
	if createResult.PrivateIP == "" {
		return "", fmt.Errorf("[3/6] worker has no private network IP")
	}

	// Step 4: Wait for Tailscale SSH on the worker.
	logf("[4/6] Waiting for Tailscale SSH (up to 10 min)...")
	workerSSH := nodeinstall.SSHConfig{Host: nodeName, Port: 22, User: "ubuntu", KeyPath: sshKeyPath}
	if err := nodeinstall.WaitForSSH(ctx, workerSSH, 10*time.Minute, out); err != nil {
		return "", fmt.Errorf("[4/6] ssh wait: %w", err)
	}

	// Step 5: Read the k3s node-token from the control plane.
	logf("[5/6] Reading node-token from control-plane...")
	cpSSH := nodeinstall.SSHConfig{Host: clusterName, Port: 22, User: "ubuntu", KeyPath: sshKeyPath}
	nodeToken, err := nodeinstall.ReadRemoteFile(ctx, cpSSH, "/var/lib/rancher/k3s/server/node-token")
	if err != nil {
		return "", fmt.Errorf("[5/6] read node-token: %w", err)
	}
	nodeToken = strings.TrimSpace(nodeToken)
	if nodeToken == "" {
		return "", fmt.Errorf("[5/6] node-token is empty on control-plane")
	}

	// Step 6: Run clusterboxnode as a k3s agent on the worker.
	logf("[6/6] Running clusterboxnode agent (joining via private network)...")
	agentSpec := &config.Spec{
		Hostname: nodeName,
		K3s: &config.K3sSpec{
			Enabled:      true,
			Role:         "agent",
			Version:      k3sVersion,
			ServerURL:    "https://" + cpPrivateIP + ":6443",
			Token:        nodeToken,
			NodeIP:       createResult.PrivateIP,
			FlannelIface: HetznerPrivateIface,
			NodeLabels:   []string{"node-role.kubernetes.io/worker=worker"},
		},
	}
	agentSpecYAML, err := yaml.Marshal(agentSpec)
	if err != nil {
		return "", fmt.Errorf("[6/6] marshal agent spec: %w", err)
	}
	loader := p.deps.AgentBundleForArch
	if loader == nil {
		loader = agentbundle.ForArch
	}
	if _, err := nodeinstall.RunNodeAgent(ctx, workerSSH, agentSpecYAML, loader, out); err != nil {
		nodeinstall.CollectAgentDiagnostics(ctx, workerSSH, "https://"+cpPrivateIP+":6443", out)
		return "", fmt.Errorf("[6/6] run agent: %w", err)
	}

	logf("[6/6] Node joined and Ready.")
	return nodeName, nil
}

// RemoveNode tears down the Hetzner VM for nodeName. It is called by the cmd
// layer after kubectl drain + delete have already completed.
//
// The function is idempotent: if the server is already gone it returns nil.
func (p *Provider) RemoveNode(ctx context.Context, _, nodeName string) error {
	out := p.out()
	hetznerToken := p.hetznerToken()
	hcloudClient := hcloudsdk.NewClient(hcloudsdk.WithToken(hetznerToken))

	_, _ = fmt.Fprintf(out, "hetzner: removing server %q...\n", nodeName)

	server, _, err := hcloudClient.Server.GetByName(ctx, nodeName)
	if err != nil {
		return fmt.Errorf("hetzner: lookup server %q: %w", nodeName, err)
	}
	if server == nil {
		_, _ = fmt.Fprintf(out, "hetzner: server %q not found; nothing to delete.\n", nodeName)
	} else {
		deleteResource := p.deps.DeleteResource
		if deleteResource == nil {
			deleteResource = deleteHCloudResource
		}
		if err := deleteResource(ctx, hetznerToken, registry.ResourceServer, strconv.FormatInt(server.ID, 10)); err != nil {
			return fmt.Errorf("hetzner: delete server %q: %w", nodeName, err)
		}
	}

	// Remove Tailscale device — best-effort; failure is logged but not fatal.
	if p.deps.TailscaleClientID != "" && p.deps.TailscaleClientSecret != "" {
		deleteDevice := p.deps.DeleteTailscaleDevice
		if deleteDevice == nil {
			deleteDevice = tailscale.DeleteDevice
		}
		if err := deleteDevice(ctx, p.deps.TailscaleClientID, p.deps.TailscaleClientSecret, nodeName); err != nil {
			_, _ = fmt.Fprintf(out, "warning: remove Tailscale device %q: %v\n", nodeName, err)
		}
	}

	return nil
}

// Compile-time check: *Provider satisfies provision.Provider.
var _ provision.Provider = (*Provider)(nil)
