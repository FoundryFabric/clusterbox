package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/registry/sync"
	"github.com/spf13/cobra"
)

// SyncDeps groups injectable dependencies for the sync command. Tests
// replace individual fields; nil fields fall back to production defaults.
type SyncDeps struct {
	// OpenRegistry opens the local registry. Defaults to registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)

	// Pulumi is the PulumiClient used to enumerate cluster nodes. Defaults
	// to a registry-backed client that treats the local registry as the
	// source of truth for node membership.
	Pulumi sync.PulumiClient

	// Kubectl runs kubectl commands. Defaults to an os/exec implementation.
	Kubectl sync.KubectlRunner
}

// syncFlags holds CLI flags for the sync command.
type syncFlags struct {
	prune  bool
	dryRun bool
}

var syncF syncFlags

var syncCmd = &cobra.Command{
	Use:   "sync [<cluster>]",
	Short: "Reconcile the local registry against the hcloud inventory and kubectl",
	Long: `Reconcile the local registry against the source-of-truth systems:
the hcloud inventory for nodes and kubectl for service deployments.

By default, drift is reported as warnings on stderr and the registry is left
untouched. Pass --prune to delete clusters that no longer have tracked nodes.
Node rows that have been destroyed are always deleted.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSync,
}

func init() {
	syncCmd.Flags().BoolVar(&syncF.prune, "prune", false, "Delete registry rows whose source-of-truth has disappeared")
	syncCmd.Flags().BoolVar(&syncF.dryRun, "dry-run", false, "Compute the diff without writing to the registry")
}

// runSync is the cobra RunE handler for `clusterbox sync`.
func runSync(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	var clusterName string
	if len(args) == 1 {
		clusterName = args[0]
	}

	deps := SyncDeps{}
	return RunSync(ctx, clusterName, sync.Options{Prune: syncF.prune, DryRun: syncF.dryRun}, deps, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// RunSync is the exported entry point used by both the cobra command and
// tests. It opens the registry, wires the requested dependencies (falling
// back to real implementations when fields are nil), runs Reconcile, and
// emits the human-readable summary on stdout.
func RunSync(ctx context.Context, clusterName string, opts sync.Options, deps SyncDeps, stdout, stderr io.Writer) error {
	open := deps.OpenRegistry
	if open == nil {
		open = registry.NewRegistry
	}
	reg, err := open(ctx)
	if err != nil {
		return fmt.Errorf("sync: open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	pulumi := deps.Pulumi
	if pulumi == nil {
		pulumi = &registryNodeClient{reg: reg}
	}
	kctl := deps.Kubectl
	if kctl == nil {
		kctl = execKubectlRunner{}
	}

	r := &sync.Reconciler{
		Registry: reg,
		Pulumi:   pulumi,
		Kubectl:  kctl,
		Warn:     stderr,
	}
	summary, err := r.Reconcile(ctx, clusterName, opts)
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	writeSyncSummary(stdout, summary, opts.DryRun)
	return nil
}

// writeSyncSummary prints the one-line summary required by T9. It is split
// out so tests can assert exact text without depending on a buffer.
func writeSyncSummary(out io.Writer, s sync.Summary, dryRun bool) {
	prefix := "synced"
	if dryRun {
		prefix = "would sync"
	}
	_, _ = fmt.Fprintf(out, "%s %d cluster(s); added %d service(s); updated %d; flagged %d drift item(s)\n",
		prefix, s.Clusters, s.ServicesAdded, s.ServicesUpdated, s.DriftItems)
}

// ---- production PulumiClient (registry-backed) ----

// registryNodeClient is the production PulumiClient implementation now that
// Pulumi stacks are no longer used for provisioning. It reads node membership
// directly from the local registry, which is the single source of truth for
// cluster node state after the hcloud-go migration.
type registryNodeClient struct {
	reg registry.Registry
}

// ListClusterNodes returns the nodes tracked for the cluster in the local
// registry. Returns sync.ErrStackNotFound when no nodes are found, matching
// the contract expected by the sync reconciler.
func (c *registryNodeClient) ListClusterNodes(ctx context.Context, clusterName string) ([]sync.PulumiNode, error) {
	nodes, err := c.reg.ListNodes(ctx, clusterName)
	if err != nil {
		return nil, fmt.Errorf("registry: list nodes for %q: %w", clusterName, err)
	}
	if len(nodes) == 0 {
		return nil, sync.ErrStackNotFound
	}
	out := make([]sync.PulumiNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, sync.PulumiNode{Hostname: n.Hostname, Role: n.Role})
	}
	return out, nil
}

// ---- production KubectlRunner ----

// execKubectlRunner is the production KubectlRunner. It shells out to
// kubectl with --kubeconfig prepended to the supplied args, propagating ctx
// so a cancelled cobra context kills the child process.
type execKubectlRunner struct{}

// Run shells out to kubectl. When stderr is non-empty on a failed exit, it
// is appended to the returned error so callers can distinguish "deployment
// not found" from connection issues.
func (execKubectlRunner) Run(ctx context.Context, kubeconfig string, args ...string) ([]byte, error) {
	full := append([]string{"--kubeconfig", kubeconfig}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out, fmt.Errorf("kubectl: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return out, fmt.Errorf("kubectl: %w", err)
	}
	return out, nil
}
