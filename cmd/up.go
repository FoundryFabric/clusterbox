package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/foundryfabric/clusterbox/internal/apply"
	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/provision/baremetal"
	"github.com/foundryfabric/clusterbox/internal/provision/hetzner"
	"github.com/foundryfabric/clusterbox/internal/provision/k3d"
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
	provider   string
	region     string
	nodes      int
	cluster    string
	k3sVersion string

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
	Long:  `Provision a new k3s cluster on Hetzner using Pulumi and bootstrap it with k3sup.`,
	RunE:  runUp,
}

func init() {
	upCmd.Flags().StringVar(&upF.provider, "provider", hetzner.Name, "Infrastructure provider")
	upCmd.Flags().StringVar(&upF.region, "region", "ash", "Region / datacenter location")
	upCmd.Flags().IntVar(&upF.nodes, "nodes", 1, "Number of nodes to provision")
	upCmd.Flags().StringVar(&upF.cluster, "cluster", "", "Cluster name (default: <provider>-<region>)")
	upCmd.Flags().StringVar(&upF.k3sVersion, "k3s-version", bootstrap.DefaultK3sVersion, "k3s version to install")

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
	// k3d uses "local" as the default name since it has no region concept.
	clusterName := upF.cluster
	if clusterName == "" {
		if upF.provider == k3d.Name {
			clusterName = "local"
		} else {
			clusterName = upF.provider + "-" + upF.region
		}
	}

	// Read required env vars.
	hetznerToken := os.Getenv("HETZNER_API_TOKEN")
	tsClientID := os.Getenv("TAILSCALE_OAUTH_CLIENT_ID")
	tsClientSecret := os.Getenv("TAILSCALE_OAUTH_CLIENT_SECRET")
	ghcrToken := os.Getenv("GHCR_TOKEN")
	ghcrUser := os.Getenv("GHCR_USER")
	pulumiToken := os.Getenv("PULUMI_ACCESS_TOKEN")

	// Determine kubeconfig path: ~/.kube/<clusterName>.yaml
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("up: resolve home dir: %w", err)
	}
	kubeconfigPath := filepath.Join(home, ".kube", clusterName+".yaml")

	// Determine the manifests directory relative to the binary location.
	// In production the binary lives next to manifests/; fall back to the
	// working directory for local dev.
	manifestDir := resolveManifestDir()

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
	}

	// Dispatch by --provider. Unknown provider returns a descriptive
	// error rather than silently falling back, so a typo in the flag
	// is caught immediately.
	prov, err := resolveProvider(upF.provider, providerOptions{
		HetznerToken:          hetznerToken,
		PulumiToken:           pulumiToken,
		KubeconfigPath:        kubeconfigPath,
		K3sVersion:            upF.k3sVersion,
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
		Location:              upF.region,
		DNSDomain:             clusterName + ".foundryfabric.dev",
		TailscaleClientID:     tsClientID,
		TailscaleClientSecret: tsClientSecret,
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

	// Baremetal and k3d targets: stop after Provision. The GHCR /
	// manifest steps below are Hetzner-specific.
	if prov.Name() == baremetal.Name || prov.Name() == k3d.Name {
		hs := extractHostnames(res, clusterName)
		// Local providers (k3d, baremetal) have no meaningful cloud region;
		// store "" so the 1Password item title stays clean (e.g. "dev-k3d"
		// not "dev-k3d-ash").
		localRegion := ""
		if prov.Name() == baremetal.Name {
			localRegion = upF.region
		}
		recordClusterInRegistry(ctx, UpDeps{}, clusterName, prov.Name(), localRegion, kubeconfigPath, hs)
		fmt.Fprintf(os.Stderr, "Cluster %q is up. Kubeconfig: %s\n", clusterName, kubeconfigPath)
		return nil
	}

	// -------------------------------------------------------------------------
	// Step 5: Create ghcr.io imagePullSecrets in cluster
	// -------------------------------------------------------------------------
	fmt.Fprintln(os.Stderr, "[5/6] Creating ghcr.io imagePullSecrets...")
	if err := secrets.CreateGHCRSecret(ctx, secrets.ExecCommandRunner{}, kubeconfigPath, ghcrToken, ghcrUser); err != nil {
		return fmt.Errorf("[5/6] failed: %w", err)
	}

	// -------------------------------------------------------------------------
	// Step 6: Apply base manifests (FDB operator, OTel Collector, Traefik)
	// -------------------------------------------------------------------------
	fmt.Fprintln(os.Stderr, "[6/6] Applying base manifests...")
	if err := apply.ApplyManifests(ctx, kubeconfigPath, manifestDir); err != nil {
		return fmt.Errorf("[6/6] failed: %w", err)
	}

	// -------------------------------------------------------------------------
	// Best-effort: record the cluster and its nodes in the local registry.
	// The registry is a local cache; the source of truth lives in
	// Pulumi/kubectl/Hetzner. Failures here must not fail the command.
	// -------------------------------------------------------------------------
	recordClusterInRegistry(ctx, UpDeps{}, clusterName, prov.Name(), upF.region, kubeconfigPath, extractHostnames(res, clusterName))

	// Best-effort: reconcile the local inventory against Hetzner. Any
	// failure logs a warning but does not fail the command — the
	// cluster came up successfully, the registry is a local cache.
	runReconcileHook(ctx, ReconcileDeps{}, clusterName, hetznerToken)

	fmt.Fprintf(os.Stderr, "Cluster %q is up. Kubeconfig: %s\n", clusterName, kubeconfigPath)
	return nil
}

// recordClusterInRegistry writes the cluster and its nodes to the local
// registry on a best-effort basis. Any error is logged to stderr; the
// function never returns an error so that registry-write failures cannot
// break a successful provision.
//
// nodeHostnames lists the node hostnames in role order: index 0 is the
// control-plane, the rest are workers. The slice mirrors what add-node
// records for joined workers.
func recordClusterInRegistry(ctx context.Context, deps UpDeps, clusterName, provider, region, kubeconfigPath string, nodeHostnames []string) {
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

	now := time.Now().UTC()
	if err := reg.UpsertCluster(ctx, registry.Cluster{
		Name:           clusterName,
		Provider:       provider,
		Region:         region,
		CreatedAt:      now,
		KubeconfigPath: kubeconfigPath,
		LastSynced:     time.Time{},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", err)
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

// resolveManifestDir returns the path to the manifests/ directory.
// It looks for manifests/ next to the running binary first, then falls back
// to ./manifests relative to the working directory.
func resolveManifestDir() string {
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "manifests")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return "manifests"
}
