package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/foundryfabric/clusterbox/internal/addon"
	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/provision/baremetal"
	"github.com/foundryfabric/clusterbox/internal/provision/hetzner"
	"github.com/foundryfabric/clusterbox/internal/provision/k3d"
	"github.com/foundryfabric/clusterbox/internal/provision/qemu"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/secrets"
	"github.com/spf13/cobra"
)

// UpDeps groups injectable dependencies for the up command. Tests replace
// individual fields; nil fields fall back to production defaults.
type UpDeps struct {
	// OpenRegistry opens the local registry. Defaults to registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)

	// ProviderRegistry overrides the production --provider lookup
	// table. Tests inject stub factories so dispatch by --provider
	// can be exercised without standing up real Hetzner / Pulumi
	// resources. nil falls back to the package-level providerRegistry.
	ProviderRegistry map[string]providerFactory
}

// upFlags holds all CLI flags for the up command.
type upFlags struct {
	provider     string
	region       string
	env          string
	nodes        int
	cluster      string
	k3sVersion   string
	tailscaleTag string
	serverType   string
	noVolume     bool
	noPublicIP   bool
	volumeSize   int
	skipAddons   []string

	// Baremetal-only flags. Required when --provider=baremetal,
	// ignored otherwise.
	bmHost       string
	bmUser       string
	bmSSHKeyPath string
	bmConfigPath string
}

var upF upFlags

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Provision a new cluster",
	Long:  `Provision a new k3s cluster on Hetzner using the hcloud API and bootstrap it with k3sup.`,
	RunE:  runUp,
}

func init() {
	upCmd.Flags().StringVar(&upF.provider, "provider", hetzner.Name, "Infrastructure provider")
	upCmd.Flags().StringVar(&upF.region, "region", "ash", "Region / datacenter location")
	upCmd.Flags().StringVar(&upF.env, "env", "", "Environment label (e.g. prod, staging). Required for cloud providers; local providers always use \"dev\".")
	upCmd.Flags().IntVar(&upF.nodes, "nodes", 1, "Number of nodes to provision")
	upCmd.Flags().StringVar(&upF.cluster, "cluster", "", "Cluster name (default: <provider>-<region>)")
	upCmd.Flags().StringVar(&upF.k3sVersion, "k3s-version", bootstrap.DefaultK3sVersion, "k3s version to install")
	upCmd.Flags().StringVar(&upF.tailscaleTag, "tailscale-tag", "tag:server", "ACL tag assigned to Tailscale devices (must exist in your tailnet ACL)")
	upCmd.Flags().StringVar(&upF.serverType, "server-type", "", "Hetzner server type (default: cpx21)")
	upCmd.Flags().BoolVar(&upF.noVolume, "no-volume", true, "Skip creating the separate data volume (saves ~€5/month)")
	upCmd.Flags().BoolVar(&upF.noPublicIP, "no-public-ip", false, "Disable public IPv4/IPv6 addresses (requires a NAT gateway on the private network)")
	upCmd.Flags().IntVar(&upF.volumeSize, "volume-size", 100, "Data volume size in GB (ignored when --no-volume is set)")
	upCmd.Flags().StringArrayVar(&upF.skipAddons, "skip-addon", nil, "Skip auto-installing a default addon (repeatable). Use 'addon install <name>' to install it later.")

	// Baremetal-only flags.
	upCmd.Flags().StringVar(&upF.bmHost, "host", "", "Baremetal host (host[:port]) -- required when --provider=baremetal")
	upCmd.Flags().StringVar(&upF.bmUser, "user", "", "Baremetal SSH user -- required when --provider=baremetal")
	upCmd.Flags().StringVar(&upF.bmSSHKeyPath, "ssh-key", "", "Path to baremetal SSH private key -- required when --provider=baremetal")
	upCmd.Flags().StringVar(&upF.bmConfigPath, "config", "", "Optional path to a clusterboxnode YAML config (overrides default Spec)")
}

// runUp is the cobra RunE handler for `clusterbox up`.
func runUp(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	// Derive cluster name from provider+region when not explicitly set.
	// k3d and qemu use "local" as the default name since they have no region concept.
	clusterName := upF.cluster
	if clusterName == "" {
		if upF.provider == k3d.Name || upF.provider == qemu.Name {
			clusterName = "local"
		} else {
			clusterName = upF.provider + "-" + upF.region
		}
	}

	ghcrToken, err := resolveToken("ghcr_token", "GHCR_TOKEN")
	if err != nil {
		return fmt.Errorf("up: %w", err)
	}
	ghcrUser, err := resolveToken("ghcr_user", "GHCR_USER")
	if err != nil {
		return fmt.Errorf("up: %w", err)
	}

	// Resolve infra tokens: config/1Password first, env var as fallback.
	// Local providers (k3d, baremetal) do not require Hetzner/Tailscale.
	var hetznerToken, tsClientID, tsClientSecret string
	if !isLocalProvider(upF.provider) {
		var cfgErr error
		hetznerToken, cfgErr = resolveToken("hetzner", "HETZNER_API_TOKEN")
		if cfgErr != nil {
			return fmt.Errorf("up: %w", cfgErr)
		}
		tsClientID, cfgErr = resolveToken("tailscale_client_id", "TAILSCALE_OAUTH_CLIENT_ID")
		if cfgErr != nil {
			return fmt.Errorf("up: %w", cfgErr)
		}
		tsClientSecret, cfgErr = resolveToken("tailscale_client_secret", "TAILSCALE_OAUTH_CLIENT_SECRET")
		if cfgErr != nil {
			return fmt.Errorf("up: %w", cfgErr)
		}
	}

	// Determine kubeconfig path: ~/.kube/<clusterName>.yaml
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("up: resolve home dir: %w", err)
	}
	kubeconfigPath := filepath.Join(home, ".kube", clusterName+".yaml")

	// Cloud providers require an explicit --env (e.g. prod, staging).
	// Local providers always use "dev" and ignore --env.
	isLocal := isLocalProvider(upF.provider)
	if !isLocal && upF.env == "" {
		return fmt.Errorf("up: --env is required for provider %q (e.g. --env prod)", upF.provider)
	}

	// Validate baremetal-only flags up front so a typo doesn't reach
	// the provider with a partially-populated DialConfig.
	if upF.provider == baremetal.Name {
		var missing []string
		if upF.bmHost == "" {
			missing = append(missing, "--host")
		}
		if upF.bmUser == "" {
			missing = append(missing, "--user")
		}
		if upF.bmSSHKeyPath == "" {
			missing = append(missing, "--ssh-key")
		}
		if len(missing) > 0 {
			return fmt.Errorf("up: --provider=baremetal requires: %s", strings.Join(missing, ", "))
		}
		// Baremetal does not support multi-node provisioning; --nodes > 1 is
		// an error rather than a silent no-op.
		if upF.nodes > 1 {
			return fmt.Errorf("up: --provider=baremetal does not support --nodes > 1 (add nodes manually)")
		}
	}

	// Dispatch by --provider. Unknown provider returns a descriptive
	// error rather than silently falling back, so a typo in the flag
	// is caught immediately.
	prov, err := resolveProvider(upF.provider, providerOptions{
		HetznerToken:          hetznerToken,
		TailscaleClientID:     tsClientID,
		TailscaleClientSecret: tsClientSecret,
		KubeconfigPath:        kubeconfigPath,
		K3sVersion:            upF.k3sVersion,
		HetznerRegion:         upF.region,
		HetznerTailscaleTag:   upF.tailscaleTag,
		BaremetalHost:         upF.bmHost,
		BaremetalUser:         upF.bmUser,
		BaremetalSSHKeyPath:   upF.bmSSHKeyPath,
		BaremetalConfigPath:   upF.bmConfigPath,
		BaremetalAgentVersion: Version(),
		K3dNodes:              upF.nodes,
	}, UpDeps{}.ProviderRegistry)
	if err != nil {
		return fmt.Errorf("up: %w", err)
	}

	// Steps 1–4 (Tailscale auth, Pulumi, boot info, k3sup) are
	// provider-specific and run inside the provider. The cmd layer
	// handles the cluster-agnostic post-provision wiring (GHCR
	// secret, base manifests, registry).
	cfg := provision.ClusterConfig{
		ClusterName:           clusterName,
		SnapshotName:          hetzner.SnapshotName,
		ServerType:            upF.serverType,
		NoVolume:              upF.noVolume,
		NoPublicIP:            upF.noPublicIP,
		VolumeSize:            upF.volumeSize,
		Env:                   upF.env,
		Location:              upF.region,
		DNSDomain:             clusterName + ".foundryfabric.dev",
		TailscaleClientID:     tsClientID,
		TailscaleClientSecret: tsClientSecret,
		TailscaleTags:         []string{upF.tailscaleTag},
		ResourceRole:          "control-plane",
	}
	res, err := prov.Provision(ctx, cfg)
	if err != nil {
		return err
	}
	// Use the kubeconfig path the provider produced so we stay in
	// agreement with what k3sup / clusterboxnode wrote.
	if res.KubeconfigPath != "" {
		kubeconfigPath = res.KubeconfigPath
	}

	// Record cluster + nodes immediately after Provision succeeds so the
	// registry stays accurate even if later steps (GHCR secret, manifests)
	// fail. This is best-effort — failures warn but do not abort.
	{
		region := upF.region
		env := upF.env
		if isLocal {
			env = "dev"
			if prov.Name() != baremetal.Name {
				region = ""
			}
		}
		recordClusterInRegistry(ctx, UpDeps{}, clusterName, prov.Name(), region, env, kubeconfigPath, extractHostnames(res, clusterName))
	}

	// Add worker nodes when --nodes > 1. k3d handles this internally via
	// K3dNodes so we only act for QEMU and Hetzner here. baremetal rejects
	// --nodes > 1 above; other providers are no-ops (return ErrAddNodeNotSupported).
	if upF.nodes > 1 {
		workerCount := upF.nodes - 1
		if err := addProviderWorkers(ctx, prov, clusterName, workerCount); err != nil {
			return fmt.Errorf("up: add workers: %w", err)
		}
	}

	// Merge the fresh kubeconfig into ~/.kube/config so `kubectl` works without
	// extra flags. k3d manages its own kubectl context; all other providers that
	// produce a per-cluster kubeconfig file are merged automatically.
	// Best-effort: a merge failure warns but does not abort a successful provision.
	if upF.provider != k3d.Name {
		if kcPath, err := defaultKubeconfigPath(); err == nil {
			if mergeErr := mergeKubeconfig(kcPath, kubeconfigPath, clusterName); mergeErr != nil {
				_, _ = fmt.Fprintf(os.Stderr, "warning: kubeconfig merge failed: %v\n", mergeErr)
			} else {
				_, _ = fmt.Fprintf(os.Stderr, "kubectl context %q set (current-context updated)\n", clusterName)
			}
		}
	}

	// Install provider default addons (best-effort: warnings only, never abort).
	installDefaultAddons(ctx, UpDeps{}, prov.Name(), clusterName, upF.skipAddons, os.Stderr)

	// Baremetal, k3d, and qemu targets: stop after Provision. The GHCR /
	// manifest steps below are Hetzner-specific.
	if isLocal {
		_, _ = fmt.Fprintf(os.Stderr, "Cluster %q is up. Kubeconfig: %s\n", clusterName, kubeconfigPath)
		return nil
	}

	// -------------------------------------------------------------------------
	// Step 5: Create ghcr.io imagePullSecrets in cluster
	// -------------------------------------------------------------------------
	_, _ = fmt.Fprintln(os.Stderr, "[5/5] Creating ghcr.io imagePullSecrets...")
	if err := secrets.CreateGHCRSecret(ctx, secrets.ExecCommandRunner{}, kubeconfigPath, ghcrToken, ghcrUser); err != nil {
		return fmt.Errorf("[5/5] failed: %w", err)
	}

	// Best-effort: reconcile the local inventory against Hetzner. Any
	// failure logs a warning but does not fail the command — the
	// cluster came up successfully, the registry is a local cache.
	runReconcileHook(ctx, ReconcileDeps{}, clusterName, hetznerToken)

	_, _ = fmt.Fprintf(os.Stderr, "Cluster %q is up. Kubeconfig: %s\n", clusterName, kubeconfigPath)
	return nil
}

// installDefaultAddons installs the provider's default addons onto clusterName
// in role order. Addons listed in skipNames are skipped. All errors are logged
// as warnings; the function never returns an error so a registry or kubectl
// failure cannot abort a successful provision.
func installDefaultAddons(ctx context.Context, deps UpDeps, providerName, clusterName string, skipNames []string, out io.Writer) {
	names := provision.DefaultAddons(providerName)
	if len(names) == 0 {
		return
	}

	skip := make(map[string]bool, len(skipNames))
	for _, n := range skipNames {
		skip[n] = true
	}

	cat := addon.DefaultCatalog()
	open := deps.OpenRegistry
	if open == nil {
		open = registry.NewRegistry
	}
	reg, err := open(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(out, "warning: default addons: open registry: %v\n", err)
		return
	}
	defer func() { _ = reg.Close() }()

	resolver, closer, err := defaultNewResolver(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(out, "warning: default addons: init secrets: %v\n", err)
		return
	}
	if closer != nil {
		defer func() { _ = closer.Close() }()
	}

	inst := &addon.Installer{
		Catalog:  cat,
		Secrets:  resolver,
		Kubectl:  secrets.ExecCommandRunner{},
		Registry: reg,
	}

	// Sort by role order so cloud-controller installs before csi-driver before ingress.
	type namedAddon struct {
		name string
		role addon.Role
	}
	var toInstall []namedAddon
	for _, name := range names {
		if skip[name] {
			_, _ = fmt.Fprintf(out, "skipping default addon %q (--skip-addon)\n", name)
			continue
		}
		a, err := cat.Get(name)
		if err != nil {
			_, _ = fmt.Fprintf(out, "warning: default addons: look up %q: %v\n", name, err)
			continue
		}
		toInstall = append(toInstall, namedAddon{name: name, role: a.Role})
	}
	sort.SliceStable(toInstall, func(i, j int) bool {
		return toInstall[i].role.RoleOrder() < toInstall[j].role.RoleOrder()
	})

	for _, a := range toInstall {
		_, _ = fmt.Fprintf(out, "installing default addon %q...\n", a.name)
		if err := inst.Install(ctx, a.name, clusterName, ""); err != nil {
			_, _ = fmt.Fprintf(out, "warning: default addon %q: %v\n", a.name, err)
		}
	}
}

// recordClusterInRegistry writes the cluster and its nodes to the local
// registry on a best-effort basis. Any error is logged to stderr; the
// function never returns an error so that registry-write failures cannot
// break a successful provision.
//
// nodeHostnames lists the node hostnames in role order: index 0 is the
// control-plane, the rest are workers. The slice mirrors what add-node
// records for joined workers.
func recordClusterInRegistry(ctx context.Context, deps UpDeps, clusterName, provider, region, env, kubeconfigPath string, nodeHostnames []string) {
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

	now := time.Now().UTC()
	if err := reg.UpsertCluster(ctx, registry.Cluster{
		Name:           clusterName,
		Provider:       provider,
		Region:         region,
		Env:            env,
		CreatedAt:      now,
		KubeconfigPath: kubeconfigPath,
		LastSynced:     time.Time{},
	}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", err)
		return
	}

	for i, hostname := range nodeHostnames {
		role := "worker"
		if i == 0 {
			role = "control-plane"
		}
		if err := reg.UpsertNode(ctx, registry.Node{
			ClusterName: clusterName,
			Hostname:    hostname,
			Role:        role,
			JoinedAt:    now,
		}); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", err)
			return
		}
	}
}

// extractHostnames returns the node hostnames from a ProvisionResult,
// falling back to clusterName when the provider surfaced no nodes.
func extractHostnames(res provision.ProvisionResult, clusterName string) []string {
	hostnames := make([]string, 0, len(res.Nodes))
	for _, n := range res.Nodes {
		hostnames = append(hostnames, n.Hostname)
	}
	if len(hostnames) == 0 {
		hostnames = []string{clusterName}
	}
	return hostnames
}
