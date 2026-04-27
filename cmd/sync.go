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
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/spf13/cobra"
)

// SyncDeps groups injectable dependencies for the sync command. Tests
// replace individual fields; nil fields fall back to production defaults.
type SyncDeps struct {
	// OpenRegistry opens the local registry. Defaults to registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)

	// Pulumi is the PulumiClient used to enumerate cluster nodes. Defaults
	// to a real auto-API-backed client.
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
	Short: "Reconcile the local registry against Pulumi and kubectl",
	Long: `Reconcile the local registry against the source-of-truth systems:
Pulumi for nodes and kubectl for service deployments.

By default, drift is reported as warnings on stderr and the registry is left
untouched. Pass --prune to delete clusters that no longer have a matching
Pulumi stack. Node rows whose Pulumi stack disappeared are always deleted
(nodes are owned by Pulumi).`,
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
		pulumi = newAutoPulumiClient()
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

// ---- production PulumiClient ----

// autoPulumiClient is the production PulumiClient. It uses the Pulumi
// auto-API local workspace to enumerate stacks for the cluster's two
// projects: "clusterbox" for the control-plane stack and the per-cluster
// project for worker stacks (mirroring the layout used by `up` and
// `add-node`).
type autoPulumiClient struct{}

func newAutoPulumiClient() sync.PulumiClient { return autoPulumiClient{} }

// ListClusterNodes enumerates control-plane and worker stacks for cluster.
//
// Layout (mirrors cmd/up.go and cmd/add_node.go):
//   - Project "clusterbox", stack <cluster>           → control-plane
//   - Project <cluster>,    stack <cluster>-node-*    → worker
//
// Implementation note: the auto-API requires a workspace rooted in a
// project. We construct an inline source (no-op program) per project so
// LocalWorkspace.ListStacks can return the matching summaries. Stacks for
// other clusters that happen to live under "clusterbox" are filtered out by
// matching on stack name == cluster.
func (autoPulumiClient) ListClusterNodes(ctx context.Context, clusterName string) ([]sync.PulumiNode, error) {
	var nodes []sync.PulumiNode

	// Control-plane stack (project "clusterbox", stack name = cluster).
	cpFound, err := pulumiHasStack(ctx, "clusterbox", clusterName)
	if err != nil {
		return nil, fmt.Errorf("pulumi: list clusterbox stacks: %w", err)
	}
	if cpFound {
		nodes = append(nodes, sync.PulumiNode{Hostname: clusterName, Role: "control-plane"})
	}

	// Worker stacks (project <cluster>, stack names like "<cluster>-node...").
	workers, err := pulumiListWorkerStacks(ctx, clusterName)
	if err != nil {
		// A missing project here is normal when no workers were ever
		// added; treat it like an empty worker set rather than failing.
		if !isPulumiProjectMissing(err) {
			return nil, fmt.Errorf("pulumi: list worker stacks for %q: %w", clusterName, err)
		}
	}
	for _, w := range workers {
		nodes = append(nodes, sync.PulumiNode{Hostname: w, Role: "worker"})
	}

	if len(nodes) == 0 {
		return nil, sync.ErrStackNotFound
	}
	return nodes, nil
}

// pulumiHasStack reports whether project/stack exists in the local
// auto-workspace.
func pulumiHasStack(ctx context.Context, project, stackName string) (bool, error) {
	ws, err := auto.NewLocalWorkspace(ctx, auto.Project(autoProject(project)))
	if err != nil {
		return false, err
	}
	stacks, err := ws.ListStacks(ctx)
	if err != nil {
		return false, err
	}
	for _, s := range stacks {
		if s.Name == stackName {
			return true, nil
		}
	}
	return false, nil
}

// pulumiListWorkerStacks returns every stack name belonging to the
// per-cluster project, which corresponds to that cluster's worker nodes.
func pulumiListWorkerStacks(ctx context.Context, clusterName string) ([]string, error) {
	ws, err := auto.NewLocalWorkspace(ctx, auto.Project(autoProject(clusterName)))
	if err != nil {
		return nil, err
	}
	stacks, err := ws.ListStacks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(stacks))
	for _, s := range stacks {
		out = append(out, s.Name)
	}
	return out, nil
}

// autoProject builds the minimal workspace.Project the auto-API needs to
// address a project. We intentionally avoid loading a Pulumi.yaml from disk
// — sync only reads metadata, never runs a program.
func autoProject(name string) workspace.Project {
	return workspace.Project{
		Name:    tokens.PackageName(name),
		Runtime: workspace.NewProjectRuntimeInfo("go", nil),
	}
}

// isPulumiProjectMissing recognises the "no project" error so a worker-less
// cluster does not fail the sync. Pulumi returns this as a string match —
// not exposed as a sentinel — so we string-match the typical phrases. Any
// false negative here just degrades to a hard error, which is the safe
// direction.
func isPulumiProjectMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no project"),
		strings.Contains(msg, "could not find a project"),
		strings.Contains(msg, "no Pulumi.yaml"):
		return true
	}
	return false
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
