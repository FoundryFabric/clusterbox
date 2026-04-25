package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/spf13/cobra"
)

// DestroyDeps groups injectable dependencies for the destroy command.
// Tests replace fields; nil fields fall back to production defaults.
type DestroyDeps struct {
	// OpenRegistry opens the local registry. Defaults to
	// registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)
	// PulumiDestroy tears down the cluster's Pulumi stack(s). Defaults
	// to destroyClusterPulumiStack which calls the Automation API.
	PulumiDestroy func(ctx context.Context, clusterName, hetznerToken, pulumiToken string) error
	// NewLister builds a HCloudResourceLister around the Hetzner API
	// token. Defaults to wrapping hcloud.NewClient.
	NewLister func(token string) provision.HCloudResourceLister
	// DeleteResource directly removes a single Hetzner resource by
	// (type, id) using the SDK. Defaults to deleteHCloudResource.
	DeleteResource func(ctx context.Context, token string, resourceType registry.HetznerResourceType, hetznerID string) error
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
	pulumiToken   string
	withDeps      DestroyDeps
}

var destroyF destroyFlags

var destroyCmd = &cobra.Command{
	Use:   "destroy <cluster>",
	Short: "Destroy a cluster",
	Long: `Tear down a clusterbox-managed cluster: run Pulumi destroy, reconcile the local
inventory against Hetzner, soft-delete the cluster row, and warn about leftovers.

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
func runDestroy(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	clusterName := args[0]

	hetznerToken := destroyF.hetznerToken
	if hetznerToken == "" {
		hetznerToken = os.Getenv("HETZNER_API_TOKEN")
	}
	pulumiToken := destroyF.pulumiToken
	if pulumiToken == "" {
		pulumiToken = os.Getenv("PULUMI_ACCESS_TOKEN")
	}

	return RunDestroyWith(ctx, clusterName, hetznerToken, pulumiToken, destroyF.yes, destroyF.dryRun, destroyF.withDeps)
}

// RunDestroyWith is the injectable variant of destroy used by tests. It
// performs the full flow: confirm, pulumi destroy, reconcile, sweep
// stragglers, mark cluster destroyed.
func RunDestroyWith(
	ctx context.Context,
	clusterName, hetznerToken, pulumiToken string,
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
		fmt.Fprintf(out, "Cluster %q is already marked destroyed at %s; nothing to do.\n", clusterName, cluster.DestroyedAt.UTC().Format(time.RFC3339))
		return nil
	}

	resources, err := reg.ListResources(ctx, clusterName, false)
	if err != nil {
		return fmt.Errorf("destroy: list resources: %w", err)
	}

	// Print the destruction plan.
	printDestroyPlan(out, clusterName, resources, dryRun)

	if dryRun {
		fmt.Fprintln(out, "(dry-run) no changes were made.")
		return nil
	}

	// Confirmation prompt.
	if !yes {
		if !confirm(in, out, "Proceed?") {
			fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}

	// Step 1: Pulumi destroy.
	pulumiDestroy := deps.PulumiDestroy
	if pulumiDestroy == nil {
		pulumiDestroy = destroyClusterPulumiStack
	}
	fmt.Fprintln(out, "[1/4] Running Pulumi destroy on the cluster stack...")
	if err := pulumiDestroy(ctx, clusterName, hetznerToken, pulumiToken); err != nil {
		// Pulumi destroy failure leaves the registry untouched so the
		// operator can re-run after fixing the underlying problem.
		return fmt.Errorf("[1/4] pulumi destroy failed (registry untouched, safe to re-run): %w", err)
	}

	// Step 2: Reconcile — confirms what Pulumi removed, surfaces drift.
	fmt.Fprintln(out, "[2/4] Reconciling local inventory against Hetzner...")
	newLister := deps.NewLister
	if newLister == nil {
		newLister = func(token string) provision.HCloudResourceLister {
			return provision.NewHCloudLister(hcloud.NewClient(hcloud.WithToken(token)))
		}
	}
	r := &provision.Reconciler{Registry: reg, Lister: newLister(hetznerToken)}
	summary, err := r.Reconcile(ctx, clusterName)
	if err != nil {
		fmt.Fprintf(out, "warning: reconciler failed: %v\n", err)
	} else {
		fmt.Fprintf(out,
			"reconciler: added=%d existing=%d marked_destroyed=%d unmanaged=%d\n",
			summary.Added, summary.Existing, summary.MarkedDestroyed, len(summary.Unmanaged),
		)
		if len(summary.Unmanaged) > 0 {
			fmt.Fprintf(out, "warning: %d unmanaged resources detected: %v (not auto-deleted)\n", len(summary.Unmanaged), summary.Unmanaged)
		}
	}

	// Step 3: Sweep stragglers — anything still active in the registry
	// after pulumi destroy + reconcile is a Pulumi-managed leak. Try a
	// direct SDK delete and tombstone the row regardless so a re-run
	// converges.
	deleteResource := deps.DeleteResource
	if deleteResource == nil {
		deleteResource = deleteHCloudResource
	}
	stragglers, err := reg.ListResources(ctx, clusterName, false)
	if err != nil {
		fmt.Fprintf(out, "warning: list stragglers: %v\n", err)
		stragglers = nil
	}
	fmt.Fprintf(out, "[3/4] Sweeping %d straggler(s) via direct SDK delete...\n", len(stragglers))
	for _, row := range stragglers {
		if err := deleteResource(ctx, hetznerToken, row.ResourceType, row.HetznerID); err != nil {
			fmt.Fprintf(out, "warning: direct delete %s/%s failed: %v\n", row.ResourceType, row.HetznerID, err)
			// Continue: still tombstone so the row is not perpetually
			// active. The warning surfaces the gap to the operator.
		}
		if err := reg.MarkResourceDestroyed(ctx, row.ID, time.Now().UTC()); err != nil {
			fmt.Fprintf(out, "warning: tombstone resource id=%d: %v\n", row.ID, err)
		}
	}

	// Step 4: Soft-delete the cluster row.
	fmt.Fprintln(out, "[4/4] Marking cluster row destroyed...")
	if err := reg.MarkClusterDestroyed(ctx, clusterName, time.Now().UTC()); err != nil {
		return fmt.Errorf("[4/4] mark cluster destroyed: %w", err)
	}

	fmt.Fprintf(out, "Cluster %q destroyed.\n", clusterName)
	fmt.Fprintln(out, "NOTE: DNS records are not auto-removed. Remove them manually if desired.")
	return nil
}

// printDestroyPlan writes a human-readable summary of what destroy will
// touch. It is shared by the dry-run path and the confirmation prompt
// so the operator sees identical text in both flows.
func printDestroyPlan(out io.Writer, clusterName string, resources []registry.HetznerResource, dryRun bool) {
	fmt.Fprintln(out, "You are about to destroy:")
	fmt.Fprintf(out, "  cluster %s\n", clusterName)

	counts := make(map[registry.HetznerResourceType]int)
	for _, r := range resources {
		counts[r.ResourceType]++
	}

	// Stable ordering for predictable output and tests.
	types := make([]string, 0, len(counts))
	for t := range counts {
		types = append(types, string(t))
	}
	sort.Strings(types)
	for _, t := range types {
		fmt.Fprintf(out, "  %d %s\n", counts[registry.HetznerResourceType(t)], pluraliseType(t, counts[registry.HetznerResourceType(t)]))
	}
	if len(resources) == 0 {
		fmt.Fprintln(out, "  (no active hetzner resources tracked in inventory)")
	}
	if dryRun {
		fmt.Fprintln(out, "Plan only — no changes will be made.")
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
	fmt.Fprintf(out, "%s (y/N) ", prompt)
	br := bufio.NewReader(in)
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// destroyClusterPulumiStack is the production PulumiDestroy implementation.
// It tears down the main cluster stack via the Automation API. Per-node
// stacks created by add-node live in their own (clusterName) project; the
// reconciler + sweep step handle their resources by label so the
// cluster-level destroy converges even when those stacks have not been
// individually torn down.
func destroyClusterPulumiStack(ctx context.Context, clusterName, hetznerToken, pulumiToken string) error {
	program := func(pCtx *pulumi.Context) error {
		// Inline program required even for destroy; resources will be
		// removed regardless of body.
		return provision.ProvisionStackWithUserData(pCtx, provision.ClusterConfig{
			ClusterName:  clusterName,
			SnapshotName: "clusterbox-base-v0.1.0",
			Location:     "ash",
			DNSDomain:    clusterName + ".foundryfabric.dev",
		}, "#cloud-config\nruncmd: []")
	}

	if pulumiToken != "" {
		_ = os.Setenv("PULUMI_ACCESS_TOKEN", pulumiToken)
	}

	s, err := auto.UpsertStackInlineSource(ctx, clusterName, "clusterbox", program)
	if err != nil {
		return fmt.Errorf("pulumi: upsert stack: %w", err)
	}
	if err := s.SetConfig(ctx, "hcloud:token", auto.ConfigValue{Value: hetznerToken, Secret: true}); err != nil {
		return fmt.Errorf("pulumi: set hcloud token: %w", err)
	}
	if _, err := s.Destroy(ctx, optdestroy.ProgressStreams(os.Stderr)); err != nil {
		return fmt.Errorf("pulumi: destroy: %w", err)
	}
	return nil
}

// deleteHCloudResource is the production DeleteResource implementation: a
// thin dispatcher over hcloud-go's per-type Delete methods. ID strings
// originate from the registry (which stored them as decimal strings on
// insert) so a parse failure is treated as a hard error.
func deleteHCloudResource(ctx context.Context, token string, resourceType registry.HetznerResourceType, hetznerID string) error {
	id, err := strconv.ParseInt(hetznerID, 10, 64)
	if err != nil {
		return fmt.Errorf("parse hetzner id %q: %w", hetznerID, err)
	}
	c := hcloud.NewClient(hcloud.WithToken(token))

	switch resourceType {
	case registry.ResourceServer:
		_, err := c.Server.Delete(ctx, &hcloud.Server{ID: id})
		return err
	case registry.ResourceLoadBalancer:
		_, err := c.LoadBalancer.Delete(ctx, &hcloud.LoadBalancer{ID: id})
		return err
	case registry.ResourceSSHKey:
		_, err := c.SSHKey.Delete(ctx, &hcloud.SSHKey{ID: id})
		return err
	case registry.ResourceFirewall:
		_, err := c.Firewall.Delete(ctx, &hcloud.Firewall{ID: id})
		return err
	case registry.ResourceNetwork:
		_, err := c.Network.Delete(ctx, &hcloud.Network{ID: id})
		return err
	case registry.ResourceVolume:
		_, err := c.Volume.Delete(ctx, &hcloud.Volume{ID: id})
		return err
	case registry.ResourcePrimaryIP:
		_, err := c.PrimaryIP.Delete(ctx, &hcloud.PrimaryIP{ID: id})
		return err
	case registry.ResourceTailscaleDevice:
		// Tailscale devices are tracked alongside Hetzner resources but
		// removed via the Tailscale API. Direct deletion from the
		// destroy path is intentionally a no-op for now: ephemeral
		// auth keys mean devices age out automatically. A future task
		// can wire in an explicit Tailscale delete.
		return nil
	default:
		return fmt.Errorf("delete: unknown resource type %q", resourceType)
	}
}
