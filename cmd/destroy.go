package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/provision/hetzner"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/spf13/cobra"
)

// DestroyDeps groups injectable dependencies for the destroy command.
// Tests replace fields; nil fields fall back to production defaults.
//
// The Hetzner-flavoured fields (NewLister, DeleteResource) survive from
// the pre-Provider-interface era so the existing destroy test surface
// keeps working. The dispatcher wires them through to the Hetzner
// provider's Deps when no explicit Provider override is supplied.
type DestroyDeps struct {
	// OpenRegistry opens the local registry. Defaults to
	// registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)

	// ProviderRegistry overrides the production --provider lookup
	// table. Tests inject stub factories so dispatch by --provider
	// can be exercised without standing up real Hetzner resources.
	// nil falls back to the package-level providerRegistry.
	ProviderRegistry map[string]providerFactory

	// Provider, when non-nil, short-circuits the registry lookup and
	// is used directly. Production callers leave it nil and rely on
	// ProviderRegistry / cluster.Provider; the field exists primarily
	// so unit tests can inject a stubbed-out provider.
	Provider provision.Provider

	// NewLister builds a HCloudResourceLister around the Hetzner
	// API token. When non-nil it is plumbed into the Hetzner
	// provider via its Deps. Defaults wrap hcloud.NewClient.
	NewLister func(token string) hetzner.HCloudResourceLister

	// DeleteResource directly removes a single Hetzner resource by
	// (type, id) using the SDK. When non-nil it is plumbed into the
	// Hetzner provider via its Deps. Defaults dispatch through
	// hcloud-go's per-type Delete methods.
	DeleteResource func(ctx context.Context, token string, resourceType registry.ResourceType, hetznerID string) error

	// Resolver resolves infra credentials (Hetzner token, Tailscale OAuth
	// credentials, etc.). When nil the production opTokenResolver is used.
	// Tests inject a staticTokenResolver to avoid calling the 1Password CLI.
	Resolver TokenResolver

	// In is the prompt input source. Defaults to os.Stdin.
	In io.Reader
	// Out is the prompt output sink. Defaults to os.Stderr.
	Out io.Writer
}

// destroyFlags holds the CLI flags accepted by `clusterbox destroy`.
type destroyFlags struct {
	yes           bool
	keepSnapshots bool
	dryRun        bool
	hetznerToken  string
	withDeps      DestroyDeps
}

var destroyF destroyFlags

var destroyCmd = &cobra.Command{
	Use:   "destroy <cluster>",
	Short: "Destroy a cluster",
	Long: `Tear down a clusterbox-managed cluster: reconcile the local inventory against
Hetzner, sweep straggler resources, soft-delete the cluster row, and warn about leftovers.

DNS records are NOT auto-removed.`,
	Args: cobra.ExactArgs(1),
	RunE: runDestroy,
}

func init() {
	destroyCmd.Flags().BoolVarP(&destroyF.yes, "yes", "y", false, "Skip confirmation prompt")
	destroyCmd.Flags().BoolVar(&destroyF.keepSnapshots, "keep-snapshots", false, "Leave Hetzner snapshots in place (default; clusterbox does not own snapshots)")
	destroyCmd.Flags().BoolVar(&destroyF.dryRun, "dry-run", false, "Print the plan without making any changes")
}

// runDestroy is the cobra RunE handler for `clusterbox destroy`.
//
// The Hetzner API token is resolved lazily inside RunDestroyWith (after the
// cluster row is looked up) so QEMU / k3d / baremetal destroys do not require
// Hetzner credentials to be present in the environment.
func runDestroy(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	clusterName := args[0]
	return RunDestroyWith(ctx, clusterName, destroyF.hetznerToken, destroyF.yes, destroyF.dryRun, destroyF.withDeps)
}

// RunDestroyWith is the injectable variant of destroy used by tests. It
// performs the full flow: confirm, dispatch to the provider for cloud
// teardown, mark cluster destroyed.
func RunDestroyWith(
	ctx context.Context,
	clusterName, hetznerToken string,
	yes, dryRun bool,
	deps DestroyDeps,
) error {
	if clusterName == "" {
		return fmt.Errorf("destroy: cluster name is required")
	}

	out := deps.Out
	if out == nil {
		out = os.Stderr
	}
	in := deps.In
	if in == nil {
		in = os.Stdin
	}

	openRegistry := deps.OpenRegistry
	if openRegistry == nil {
		openRegistry = registry.NewRegistry
	}

	reg, err := openRegistry(ctx)
	if err != nil {
		return fmt.Errorf("destroy: open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	cluster, err := reg.GetCluster(ctx, clusterName)
	if err != nil {
		return fmt.Errorf("destroy: lookup cluster %q: %w", clusterName, err)
	}
	if !cluster.DestroyedAt.IsZero() {
		_, _ = fmt.Fprintf(out, "Cluster %q is already marked destroyed at %s; nothing to do.\n", clusterName, cluster.DestroyedAt.UTC().Format(time.RFC3339))
		return nil
	}

	resources, err := reg.ListResources(ctx, clusterName, false)
	if err != nil {
		return fmt.Errorf("destroy: list resources: %w", err)
	}

	// Print the destruction plan.
	printDestroyPlan(out, clusterName, resources, dryRun)

	if dryRun {
		_, _ = fmt.Fprintln(out, "(dry-run) no changes were made.")
		return nil
	}

	// Confirmation prompt.
	if !yes {
		if !confirm(in, out, "Proceed?") {
			_, _ = fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}

	// Resolve the provider that owns this cluster. The provider name
	// is recorded on the cluster row at provision time; legacy rows
	// without a provider value fall back to hetzner for backward
	// compatibility.
	//
	// The Hetzner token (and Tailscale credentials) are resolved here —
	// after the cluster row lookup — so local-provider destroys (QEMU,
	// k3d, baremetal) do not require cloud credentials to be present.
	resolver := deps.Resolver
	if resolver == nil {
		resolver = opTokenResolver{}
	}

	prov := deps.Provider
	if prov == nil {
		providerName := cluster.Provider
		if providerName == "" {
			providerName = hetzner.Name
		}

		// Only resolve cloud credentials when the cluster is Hetzner-backed.
		resolvedHetznerToken := hetznerToken
		var tsClientID, tsClientSecret string
		if providerName == hetzner.Name {
			if resolvedHetznerToken == "" {
				var tokErr error
				resolvedHetznerToken, tokErr = resolver.ResolveToken("hetzner", "HETZNER_API_TOKEN")
				if tokErr != nil {
					return fmt.Errorf("destroy: %w", tokErr)
				}
			}
			tsClientID, _ = resolver.ResolveToken("tailscale_client_id", "TAILSCALE_OAUTH_CLIENT_ID")
			tsClientSecret, _ = resolver.ResolveToken("tailscale_client_secret", "TAILSCALE_OAUTH_CLIENT_SECRET")
		}

		var err error
		prov, err = resolveProvider(providerName, providerOptions{
			HetznerToken:          resolvedHetznerToken,
			TailscaleClientID:     tsClientID,
			TailscaleClientSecret: tsClientSecret,
			HetznerOpenRegistry:   deps.OpenRegistry,
			HetznerNewLister:      deps.NewLister,
			HetznerDeleteResource: deps.DeleteResource,
			HetznerOut:            out,
		}, deps.ProviderRegistry)
		if err != nil {
			return fmt.Errorf("destroy: %w", err)
		}
	}

	// Cloud-side teardown is delegated to the provider so destroy
	// stays uniform across providers. The provider prints its own
	// per-step progress lines ([1/4]...[3/4]).
	if err := prov.Destroy(ctx, cluster); err != nil {
		return err
	}

	// Step 3: Soft-delete the cluster row. This is registry-only and
	// stays in cmd so the registry layer remains the sole owner of
	// cluster-level state transitions.
	_, _ = fmt.Fprintln(out, "[3/3] Marking cluster row destroyed...")
	if err := reg.MarkClusterDestroyed(ctx, clusterName, time.Now().UTC()); err != nil {
		return fmt.Errorf("[4/4] mark cluster destroyed: %w", err)
	}

	// Best-effort: remove the cluster/user/context from ~/.kube/config so
	// kubectl no longer references the destroyed cluster.
	if kcPath, kcErr := defaultKubeconfigPath(); kcErr == nil {
		if rmErr := removeKubeconfigContext(kcPath, clusterName); rmErr != nil {
			_, _ = fmt.Fprintf(out, "warning: kubeconfig cleanup failed: %v\n", rmErr)
		}
	}

	_, _ = fmt.Fprintf(out, "Cluster %q destroyed.\n", clusterName)
	_, _ = fmt.Fprintln(out, "NOTE: DNS records are not auto-removed. Remove them manually if desired.")
	return nil
}

// printDestroyPlan writes a human-readable summary of what destroy will
// touch. It is shared by the dry-run path and the confirmation prompt
// so the operator sees identical text in both flows.
func printDestroyPlan(out io.Writer, clusterName string, resources []registry.ClusterResource, dryRun bool) {
	_, _ = fmt.Fprintln(out, "You are about to destroy:")
	_, _ = fmt.Fprintf(out, "  cluster %s\n", clusterName)

	// Exclude Tailscale devices from the plan count — they are handled
	// separately by the provider's step 3 via the Tailscale API.
	hetznerResources := make([]registry.ClusterResource, 0, len(resources))
	for _, r := range resources {
		if r.Provider != registry.ProviderTailscale {
			hetznerResources = append(hetznerResources, r)
		}
	}

	counts := make(map[registry.ResourceType]int)
	for _, r := range hetznerResources {
		counts[r.ResourceType]++
	}

	// Stable ordering for predictable output and tests.
	types := make([]string, 0, len(counts))
	for t := range counts {
		types = append(types, string(t))
	}
	sort.Strings(types)
	for _, t := range types {
		_, _ = fmt.Fprintf(out, "  %d %s\n", counts[registry.ResourceType(t)], pluraliseType(t, counts[registry.ResourceType(t)]))
	}
	if len(hetznerResources) == 0 {
		_, _ = fmt.Fprintln(out, "  (no active resources tracked in inventory)")
	}
	if dryRun {
		_, _ = fmt.Fprintln(out, "Plan only — no changes will be made.")
	}
}

// pluraliseType returns a human-friendly plural for a resource_type value.
func pluraliseType(t string, n int) string {
	human := strings.ReplaceAll(t, "_", " ")
	if n == 1 {
		return human
	}
	switch t {
	case "ssh_key":
		return "ssh keys"
	default:
		return human + "s"
	}
}

// confirm prompts the user with prompt + " (y/N) " and returns true only
// for an explicit "y" / "yes" response. Default is N (false).
func confirm(in io.Reader, out io.Writer, prompt string) bool {
	_, _ = fmt.Fprintf(out, "%s (y/N) ", prompt)
	br := bufio.NewReader(in)
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}
