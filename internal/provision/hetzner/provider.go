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
	// bootstrap.DefaultK3sVersion is used.
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
	DeleteResource func(ctx context.Context, token string, resourceType registry.HetznerResourceType, hetznerID string) error

	// OpenRegistry opens the local registry. Defaults to registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)

	// GenerateTailscaleAuthKey produces an ephemeral Tailscale auth
	// key from the OAuth credentials in cfg. Defaults to
	// tailscale.GenerateAuthKey.
	GenerateTailscaleAuthKey func(ctx context.Context, clientID, clientSecret string, tags []string) (string, error)

	// Bootstrap, when non-nil, replaces the SSH wait + kubeconfig-read
	// step. Tests inject a stub that writes a fake kubeconfig without
	// making real SSH calls.
	Bootstrap func(ctx context.Context, cfg bootstrap.K3sConfig) error
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

	// Step 2: Build spec → base64 → render cloud-init (Tailscale bootstrap).
	_, _ = fmt.Fprintln(out, "[2/5] Building spec and rendering cloud-init...")
	k3sVersion := p.deps.K3sVersion
	if k3sVersion == "" {
		k3sVersion = bootstrap.DefaultK3sVersion
	}
	spec := &config.Spec{
		Hostname: cfg.ClusterName,
		K3s: &config.K3sSpec{
			Enabled: true,
			Role:    "server-init",
			Version: k3sVersion,
			TLSSANs: []string{cfg.ClusterName},
		},
	}
	specYAML, err := yaml.Marshal(spec)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("[2/5] marshal spec: %w", err)
	}
	configB64 := base64.StdEncoding.EncodeToString(specYAML)
	sshPubKey, err := p.loadSSHPubKey(sshKeyPath)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("[2/5] %w", err)
	}
	userData, err := RenderCloudInit(sshPubKey, configB64, tsAuthKey, cfg.ClusterName)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("[2/5] failed: %w", err)
	}

	// Step 3: Provision Hetzner resources (VM + volume + firewall).
	_, _ = fmt.Fprintln(out, "[3/5] Provisioning Hetzner resources (VM + volume + firewall)...")
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
	onCreated := func(rt registry.HetznerResourceType, hetznerID, hostname string) {
		if reg == nil {
			return
		}
		if _, wErr := reg.RecordResource(ctx, registry.HetznerResource{
			ClusterName:  cfg.ClusterName,
			ResourceType: rt,
			HetznerID:    hetznerID,
			Hostname:     hostname,
		}); wErr != nil {
			_, _ = fmt.Fprintf(out, "warning: record resource %s/%s: %v\n", rt, hetznerID, wErr)
		}
	}

	if _, err := createResources(ctx, hcloudClient, cfg, userData, onCreated); err != nil {
		if reg != nil {
			_ = reg.Close()
		}
		return provision.ProvisionResult{}, fmt.Errorf("[3/5] failed: %w", err)
	}
	if reg != nil {
		_ = reg.Close()
	}

	// Step 4+5: Wait for Tailscale SSH then upload and run clusterboxnode.
	sshCfg := nodeinstall.SSHConfig{
		Host:    cfg.ClusterName,
		Port:    22,
		User:    "ubuntu",
		KeyPath: sshKeyPath,
	}
	if bootstrapFn := p.deps.Bootstrap; bootstrapFn != nil {
		if err := bootstrapFn(ctx, bootstrap.K3sConfig{
			TailscaleIP:    cfg.ClusterName,
			K3sVersion:     k3sVersion,
			KubeconfigPath: kubeconfigPath,
			SSHKeyPath:     sshKeyPath,
		}); err != nil {
			return provision.ProvisionResult{}, fmt.Errorf("[4/5] bootstrap: %w", err)
		}
	} else {
		_, _ = fmt.Fprintf(out, "[4/5] Waiting for Tailscale SSH on %s (up to 10 min)...\n", cfg.ClusterName)
		if err := nodeinstall.WaitForSSH(ctx, sshCfg, 10*time.Minute, out); err != nil {
			return provision.ProvisionResult{}, fmt.Errorf("[4/5] failed: %w", err)
		}

		_, _ = fmt.Fprintln(out, "[5/5] Uploading and running clusterboxnode via SSH...")
		arch, err := nodeinstall.ProbeArch(ctx, sshCfg)
		if err != nil {
			return provision.ProvisionResult{}, fmt.Errorf("[5/5] probe arch: %w", err)
		}
		loader := p.deps.AgentBundleForArch
		if loader == nil {
			loader = agentbundle.ForArch
		}
		agentBytes, err := loader(arch)
		if err != nil {
			return provision.ProvisionResult{}, fmt.Errorf("[5/5] agent bundle: %w", err)
		}
		stdout, err := nodeinstall.RunAgent(ctx, sshCfg, agentBytes, specYAML, out)
		if err != nil {
			return provision.ProvisionResult{}, fmt.Errorf("[5/5] run agent: %w", err)
		}
		parsed, err := nodeinstall.ParseInstallOutput(stdout)
		if err != nil {
			return provision.ProvisionResult{}, fmt.Errorf("[5/5] parse output: %w", err)
		}
		if parsed.KubeconfigYAML == "" {
			return provision.ProvisionResult{}, fmt.Errorf("[5/5] install output missing kubeconfig_yaml")
		}
		rewritten, err := nodeinstall.RewriteKubeconfigServer(parsed.KubeconfigYAML, cfg.ClusterName)
		if err != nil {
			return provision.ProvisionResult{}, fmt.Errorf("[5/5] rewrite kubeconfig: %w", err)
		}
		if err := nodeinstall.WriteKubeconfig(kubeconfigPath, rewritten, out); err != nil {
			return provision.ProvisionResult{}, fmt.Errorf("[5/5] failed: %w", err)
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
	_, _ = fmt.Fprintf(out, "[2/3] Sweeping %d straggler(s) via direct SDK delete...\n", len(stragglers))
	for _, row := range stragglers {
		if err := deleteResource(ctx, hetznerToken, row.ResourceType, row.HetznerID); err != nil {
			_, _ = fmt.Fprintf(out, "warning: direct delete %s/%s failed: %v\n", row.ResourceType, row.HetznerID, err)
			// Continue: still tombstone so the row is not perpetually
			// active. The warning surfaces the gap to the operator.
		}
		if err := reg.MarkResourceDestroyed(ctx, row.ID, time.Now().UTC()); err != nil {
			_, _ = fmt.Fprintf(out, "warning: tombstone resource id=%d: %v\n", row.ID, err)
		}
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
func deleteHCloudResource(ctx context.Context, token string, resourceType registry.HetznerResourceType, hetznerID string) error {
	id, err := strconv.ParseInt(hetznerID, 10, 64)
	if err != nil {
		return fmt.Errorf("parse hetzner id %q: %w", hetznerID, err)
	}
	c := hcloudsdk.NewClient(hcloudsdk.WithToken(token))

	switch resourceType {
	case registry.ResourceServer:
		_, _, err := c.Server.DeleteWithResult(ctx, &hcloudsdk.Server{ID: id})
		return err
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
	case registry.ResourceTailscaleDevice:
		// Tailscale devices are tracked alongside Hetzner resources
		// but removed via the Tailscale API. Direct deletion from
		// the destroy path is intentionally a no-op for now:
		// ephemeral auth keys mean devices age out automatically. A
		// future task can wire in an explicit Tailscale delete.
		return nil
	default:
		return fmt.Errorf("delete: unknown resource type %q", resourceType)
	}
}

// Compile-time check: *Provider satisfies provision.Provider.
var _ provision.Provider = (*Provider)(nil)
