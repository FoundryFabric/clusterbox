package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/spf13/cobra"
)

const arcRunnerNamespace = "arc-runners"
const arcControllerVersion = "0.14.1"
const arcRunnerImage = "ghcr.io/actions/actions-runner:2.334.0"

type runnerAddFlags struct {
	cluster string
	repo    string
	min     int
	max     int
	image   string
}

var runnerAddF runnerAddFlags

var runnerAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add a GitHub Actions runner scale set to a cluster",
	Long: `Add installs an ARC AutoscalingRunnerSet onto the cluster and records it
in the local registry. The gha-runner-scale-set addon must already be installed.`,
	Example: `  # Add a repo-scoped runner set
  clusterbox runner add clusterbox-runners --repo FoundryFabric/clusterbox

  # Add an org-scoped runner set with custom concurrency
  clusterbox runner add org-runners --repo FoundryFabric --min 1 --max 8

  # Target a specific cluster
  clusterbox runner add clusterbox-runners --repo FoundryFabric/clusterbox --cluster my-cluster`,
	Args: cobra.ExactArgs(1),
	RunE: runRunnerAdd,
}

func init() {
	runnerAddCmd.Flags().StringVar(&runnerAddF.cluster, "cluster", "", "Target cluster name (default: active context cluster)")
	runnerAddCmd.Flags().StringVar(&runnerAddF.repo, "repo", "", "GitHub repo or org/repo (e.g. FoundryFabric/clusterbox or full URL)")
	runnerAddCmd.Flags().IntVar(&runnerAddF.min, "min", 0, "Minimum number of runners")
	runnerAddCmd.Flags().IntVar(&runnerAddF.max, "max", 4, "Maximum number of runners")
	runnerAddCmd.Flags().StringVar(&runnerAddF.image, "image", arcRunnerImage, "Runner container image")
	_ = runnerAddCmd.MarkFlagRequired("repo")
}

func runRunnerAdd(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return RunRunnerAdd(ctx, args[0], runnerAddF.repo, runnerAddF.cluster, runnerAddF.min, runnerAddF.max, runnerAddF.image, cmd.OutOrStdout(), RunnerCmdDeps{})
}

// RunRunnerAdd applies an AutoscalingRunnerSet manifest to the cluster and
// records it in the registry. It is exported so tests can drive it with
// injected deps and captured output.
func RunRunnerAdd(ctx context.Context, name, repo, cluster string, min, max int, image string, out io.Writer, deps RunnerCmdDeps) error {
	if min > max {
		return fmt.Errorf("runner add: --min (%d) must not exceed --max (%d)", min, max)
	}

	var err error
	cluster, err = resolveCluster(cluster)
	if err != nil {
		return fmt.Errorf("runner add: %w", err)
	}

	if image == "" {
		image = arcRunnerImage
	}

	openReg := deps.OpenRegistry
	if openReg == nil {
		openReg = registry.NewRegistry
	}
	reg, err := openReg(ctx)
	if err != nil {
		return fmt.Errorf("runner add: open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	if _, err := reg.GetDeployment(ctx, cluster, "gha-runner-scale-set"); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return fmt.Errorf("runner add: gha-runner-scale-set addon is not installed on cluster %q — run: clusterbox addon install gha-runner-scale-set", cluster)
		}
		return fmt.Errorf("runner add: check addon: %w", err)
	}

	if _, err := reg.GetDeployment(ctx, cluster, name); err == nil {
		return fmt.Errorf("runner add: runner scale set %q already exists on cluster %q — use a different name or remove it first with: clusterbox runner remove %s", name, cluster, name)
	} else if !errors.Is(err, registry.ErrNotFound) {
		return fmt.Errorf("runner add: check existing runner: %w", err)
	}

	cl, err := reg.GetCluster(ctx, cluster)
	if err != nil {
		return fmt.Errorf("runner add: get cluster %q: %w", cluster, err)
	}

	repoURL := repo
	if !strings.HasPrefix(repoURL, "https://") {
		repoURL = "https://github.com/" + repoURL
	}

	manifest := fmt.Sprintf(`apiVersion: actions.github.com/v1alpha1
kind: AutoscalingRunnerSet
metadata:
  name: %s
  namespace: %s
  labels:
    actions.github.com/scale-set-version: "%s"
    app.kubernetes.io/version: "%s"
spec:
  githubConfigUrl: %s
  githubConfigSecret: controller-manager-gh-credentials
  minRunners: %d
  maxRunners: %d
  template:
    spec:
      containers:
        - name: runner
          image: %s
          command: ["/home/runner/run.sh"]
`, name, arcRunnerNamespace, arcControllerVersion, arcControllerVersion, repoURL, min, max, image)

	tmpf, err := os.CreateTemp("", "clusterbox-runner-*.yaml")
	if err != nil {
		return fmt.Errorf("runner add: create temp manifest: %w", err)
	}
	tmpPath := tmpf.Name()
	if _, err := tmpf.WriteString(manifest); err != nil {
		_ = tmpf.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("runner add: write manifest: %w", err)
	}
	if err := tmpf.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("runner add: close manifest: %w", err)
	}
	defer func() { _ = os.Remove(tmpPath) }()

	runner := deps.Runner
	if runner == nil {
		runner = bootstrap.ExecRunner{}
	}
	if _, err := runner.Run(ctx, "kubectl", "--kubeconfig", cl.KubeconfigPath, "apply", "-f", tmpPath); err != nil {
		return fmt.Errorf("runner add: kubectl apply: %w", err)
	}

	if err := reg.UpsertDeployment(ctx, registry.Deployment{
		ClusterName: cluster,
		Service:     name,
		Version:     repoURL,
		Status:      registry.StatusRolledOut,
		Kind:        registry.KindRunnerScaleSet,
	}); err != nil {
		return fmt.Errorf("runner add: record deployment: %w", err)
	}

	_, _ = fmt.Fprintf(out, "runner scale set %q connected to %s (runs-on: %s)\n", name, cluster, name)
	return nil
}
