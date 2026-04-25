package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/foundryfabric/clusterbox/internal/apply"
	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/secrets"
	"github.com/foundryfabric/clusterbox/internal/tailscale"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/spf13/cobra"
)

// UpDeps groups injectable dependencies for the up command. Tests replace
// individual fields; nil fields fall back to production defaults.
type UpDeps struct {
	// OpenRegistry opens the local registry. Defaults to registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)
}

// upFlags holds all CLI flags for the up command.
type upFlags struct {
	provider   string
	region     string
	nodes      int
	cluster    string
	k3sVersion string
}

var upF upFlags

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Provision a new cluster",
	Long:  `Provision a new k3s cluster on Hetzner using Pulumi and bootstrap it with k3sup.`,
	RunE:  runUp,
}

func init() {
	upCmd.Flags().StringVar(&upF.provider, "provider", "hetzner", "Infrastructure provider")
	upCmd.Flags().StringVar(&upF.region, "region", "ash", "Region / datacenter location")
	upCmd.Flags().IntVar(&upF.nodes, "nodes", 1, "Number of nodes to provision")
	upCmd.Flags().StringVar(&upF.cluster, "cluster", "", "Cluster name (default: <provider>-<region>)")
	upCmd.Flags().StringVar(&upF.k3sVersion, "k3s-version", bootstrap.DefaultK3sVersion, "k3s version to install")
}

// runUp is the cobra RunE handler for `clusterbox up`.
func runUp(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	// Derive cluster name from provider+region when not explicitly set.
	clusterName := upF.cluster
	if clusterName == "" {
		clusterName = upF.provider + "-" + upF.region
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

	// -------------------------------------------------------------------------
	// Step 1: Generate Tailscale ephemeral auth key
	// -------------------------------------------------------------------------
	fmt.Fprintln(os.Stderr, "[1/6] Generating Tailscale auth key...")
	tsAuthKey, err := tailscale.GenerateAuthKey(ctx, tsClientID, tsClientSecret)
	if err != nil {
		return fmt.Errorf("[1/6] failed: %w", err)
	}

	// -------------------------------------------------------------------------
	// Step 2: Pulumi — VM + volume + firewall + DNS A record
	// -------------------------------------------------------------------------
	fmt.Fprintln(os.Stderr, "[2/6] Running Pulumi (VM + volume + firewall + DNS)...")
	cfg := provision.ClusterConfig{
		ClusterName:           clusterName,
		SnapshotName:          "clusterbox-base-v0.1.0",
		Location:              upF.region,
		DNSDomain:             clusterName + ".foundryfabric.dev",
		TailscaleClientID:     tsClientID,
		TailscaleClientSecret: tsClientSecret,
		ResourceRole:          "control-plane",
	}
	if err := runPulumiStack(ctx, clusterName, hetznerToken, pulumiToken, tsAuthKey, cfg); err != nil {
		return fmt.Errorf("[2/6] failed: %w", err)
	}

	// -------------------------------------------------------------------------
	// Step 3: (Tailscale activates at first boot via cloud-init — no action needed)
	// -------------------------------------------------------------------------
	fmt.Fprintln(os.Stderr, "[3/6] Tailscale activates at first boot via cloud-init (no action required).")

	// -------------------------------------------------------------------------
	// Step 4: k3sup — bootstrap k3s at pinned version over Tailscale SSH
	// -------------------------------------------------------------------------
	fmt.Fprintln(os.Stderr, "[4/6] Bootstrapping k3s via k3sup over Tailscale SSH...")
	k3sCfg := bootstrap.K3sConfig{
		TailscaleIP:    clusterName, // Tailscale resolves the hostname
		K3sVersion:     upF.k3sVersion,
		KubeconfigPath: kubeconfigPath,
		SSHKeyPath:     filepath.Join(home, ".ssh", "id_ed25519"),
	}
	if err := bootstrap.Bootstrap(ctx, k3sCfg); err != nil {
		return fmt.Errorf("[4/6] failed: %w", err)
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
	recordClusterInRegistry(ctx, UpDeps{}, clusterName, upF.provider, upF.region, kubeconfigPath, []string{clusterName})

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

// runPulumiStack creates or updates the Pulumi stack for the given cluster
// using the Automation API. Running it a second time is idempotent.
func runPulumiStack(ctx context.Context, clusterName, hetznerToken, pulumiToken, tsAuthKey string, cfg provision.ClusterConfig) error {
	program := func(pCtx *pulumi.Context) error {
		userData, err := provision.RenderCloudInit(cfg.ClusterName, tsAuthKey)
		if err != nil {
			return err
		}
		return provision.ProvisionStackWithUserData(pCtx, cfg, userData)
	}

	if pulumiToken != "" {
		_ = os.Setenv("PULUMI_ACCESS_TOKEN", pulumiToken)
	}

	s, err := auto.UpsertStackInlineSource(ctx, clusterName, "clusterbox", program)
	if err != nil {
		return fmt.Errorf("pulumi: upsert stack: %w", err)
	}

	// Configure provider credentials.
	if err := s.SetConfig(ctx, "hcloud:token", auto.ConfigValue{Value: hetznerToken, Secret: true}); err != nil {
		return fmt.Errorf("pulumi: set hcloud token: %w", err)
	}

	// Run pulumi up. Idempotent: a second call converges to the same state.
	if _, err = s.Up(ctx, optup.ProgressStreams(os.Stderr)); err != nil {
		return fmt.Errorf("pulumi: up: %w", err)
	}
	return nil
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
