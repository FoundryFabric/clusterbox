package hetzner

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/tailscale"
	hcloudsdk "github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// Name is the canonical provider identifier accepted by the
// `--provider` CLI flag.
const Name = "hetzner"

// SnapshotName is the Hetzner snapshot every clusterbox node boots from.
// Pinned here so cmd-side dispatchers do not need to know it.
const SnapshotName = "clusterbox-base-v0.1.0"

// Deps groups the injectable dependencies the Hetzner provider relies
// on. Tests and alternate cmd-paths (e.g. add-node, remove-node)
// substitute fields; nil fields fall back to production defaults.
//
// The default values in DefaultDeps wire the production hcloud-go /
// k3sup paths exactly as the pre-refactor cmd code did, so
// the existing test surface continues to pass without modification.
type Deps struct {
	// HetznerToken is the Hetzner Cloud API token used for SDK calls.
	// When empty Provider falls back to the HETZNER_API_TOKEN env var.
	HetznerToken string

	// KubeconfigPath is the on-disk path k3sup writes the cluster's
	// kubeconfig to. When empty the provider derives
	// $HOME/.kube/<clusterName>.yaml.
	KubeconfigPath string

	// SSHKeyPath is the private key k3sup uses to ssh into the
	// freshly-provisioned VM. When empty the provider derives
	// $HOME/.ssh/id_ed25519.
	SSHKeyPath string

	// K3sVersion is the k3s release k3sup installs on the
	// control-plane. When empty bootstrap.DefaultK3sVersion is used.
	K3sVersion string

	// Out is the destination for human-readable progress lines. When
	// nil the provider writes to os.Stderr.
	Out io.Writer

	// CreateResources provisions all Hetzner Cloud resources for one node.
	// Defaults to CreateClusterResources.
	CreateResources func(ctx context.Context, client *hcloudsdk.Client, cfg provision.ClusterConfig, userData string) (CreateResult, error)

	// NewLister builds an HCloudResourceLister around hetznerToken.
	// Defaults to wrapping hcloud.NewClient.
	NewLister func(token string) HCloudResourceLister

	// DeleteResource directly removes a single Hetzner resource by
	// (type, id) using the SDK. Defaults to deleteHCloudResource.
	DeleteResource func(ctx context.Context, token string, resourceType registry.HetznerResourceType, hetznerID string) error

	// OpenRegistry opens the local registry. Used by Destroy to walk
	// inventory rows for the sweep step. Defaults to
	// registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)

	// GenerateTailscaleAuthKey produces an ephemeral Tailscale auth
	// key from the OAuth credentials in cfg. Defaults to
	// tailscale.GenerateAuthKey.
	GenerateTailscaleAuthKey func(ctx context.Context, clientID, clientSecret string) (string, error)

	// Bootstrap runs k3sup against the freshly-provisioned VM.
	// Defaults to bootstrap.Bootstrap.
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

// Provision stands up a Hetzner-Cloud-hosted cluster. It generates an
// ephemeral Tailscale auth key, renders cloud-init, creates all Hetzner
// resources via direct hcloud-go SDK calls, then bootstraps k3s via k3sup
// over the Tailscale-resolved hostname.
//
// The returned ProvisionResult carries the kubeconfig path k3sup
// wrote and a single control-plane Node row keyed by cluster name.
// HetznerLB is left nil — the current provisioning shape does not
// create a load balancer; future tasks may add one.
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
	tsAuthKey, err := genTSKey(ctx, cfg.TailscaleClientID, cfg.TailscaleClientSecret)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("[1/5] failed: %w", err)
	}

	// Step 2: Render cloud-init.
	_, _ = fmt.Fprintln(out, "[2/5] Rendering cloud-init...")
	userData, err := RenderCloudInit(cfg.ClusterName, tsAuthKey)
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
	if _, err := createResources(ctx, hcloudClient, cfg, userData); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("[3/5] failed: %w", err)
	}

	// Step 4: (Tailscale activates at first boot via cloud-init — no action needed.)
	_, _ = fmt.Fprintln(out, "[4/5] Tailscale activates at first boot via cloud-init (no action required).")

	// Step 5: k3sup — bootstrap k3s at pinned version over Tailscale SSH.
	_, _ = fmt.Fprintln(out, "[5/5] Bootstrapping k3s via k3sup over Tailscale SSH...")
	k3sVersion := p.deps.K3sVersion
	if k3sVersion == "" {
		k3sVersion = bootstrap.DefaultK3sVersion
	}
	bootstrapFn := p.deps.Bootstrap
	if bootstrapFn == nil {
		bootstrapFn = bootstrap.Bootstrap
	}
	k3sCfg := bootstrap.K3sConfig{
		TailscaleIP:    cfg.ClusterName, // Tailscale resolves the hostname
		K3sVersion:     k3sVersion,
		KubeconfigPath: kubeconfigPath,
		SSHKeyPath:     sshKeyPath,
	}
	if err := bootstrapFn(ctx, k3sCfg); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("[5/5] failed: %w", err)
	}

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
