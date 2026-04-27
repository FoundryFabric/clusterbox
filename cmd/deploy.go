package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/release"
	"github.com/foundryfabric/clusterbox/internal/secrets"
	"github.com/spf13/cobra"
)

// deployFlags holds all CLI flags for the deploy command.
type deployFlags struct {
	cluster string
	env     string
}

var deployF deployFlags

var deployCmd = &cobra.Command{
	Use:   "deploy <service> <version>",
	Short: "Deploy a service to a cluster",
	Long:  `Deploy a service at a given version to the specified cluster by fetching the release manifest, resolving secrets, and applying via kubectl.`,
	Args:  cobra.ExactArgs(2),
	RunE:  runDeploy,
}

func init() {
	deployCmd.Flags().StringVar(&deployF.cluster, "cluster", "", "Target cluster name (required)")
	_ = deployCmd.MarkFlagRequired("cluster")
	deployCmd.Flags().StringVar(&deployF.env, "env", "prod", "Target environment (dev|prod)")
}

// DeployDeps groups injectable dependencies for the deploy command.
// Tests replace individual fields; nil fields fall back to production defaults.
type DeployDeps struct {
	// FetchManifest downloads the release manifest. Defaults to release.FetchManifest.
	FetchManifest func(ctx context.Context, owner, repo, version, token string) ([]byte, error)
	// SecretsResolver resolves deployment secrets.
	SecretsResolver secrets.Resolver
	// Runner executes kubectl commands.
	Runner secrets.CommandRunner
	// OpenRegistry opens the local registry. Defaults to registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)
}

// runDeploy is the cobra RunE handler for `clusterbox deploy`.
func runDeploy(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	service := args[0]
	version := args[1]

	return RunDeploy(ctx, service, version, deployF.cluster, deployF.env, DeployDeps{})
}

// RunDeploy executes the full deploy sequence and is exported so tests can call
// it directly with injected dependencies.
func RunDeploy(ctx context.Context, service, version, cluster, env string, deps DeployDeps) error {
	// Capture attempt start so the history row can carry an accurate
	// rollout duration even on early failure paths.
	attemptedAt := time.Now().UTC()

	err := runDeploySteps(ctx, service, version, cluster, env, deps)
	if err != nil {
		recordDeployFailure(ctx, deps, cluster, service, version, attemptedAt, err)
		return err
	}
	recordDeploySuccess(ctx, deps, cluster, service, version, attemptedAt)
	return nil
}

// runDeploySteps performs the actual 4-step deploy. Split out from RunDeploy so
// the wrapper can centralize registry recording on every exit path without
// reordering the deploy steps themselves.
func runDeploySteps(ctx context.Context, service, version, cluster, env string, deps DeployDeps) error {
	// -------------------------------------------------------------------------
	// Guard: GITHUB_TOKEN must be present before any network call.
	// -------------------------------------------------------------------------
	githubToken := os.Getenv("GITHUB_TOKEN")
	if githubToken == "" {
		return fmt.Errorf("deploy: GITHUB_TOKEN environment variable is required but not set")
	}

	// Determine kubeconfig path: ~/.kube/<cluster>.yaml
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("deploy: resolve home dir: %w", err)
	}
	kubeconfigPath := filepath.Join(home, ".kube", cluster+".yaml")

	// Set up defaults for injectable dependencies.
	fetchFn := deps.FetchManifest
	if fetchFn == nil {
		fetchFn = release.FetchManifest
	}

	var resolver secrets.Resolver
	if deps.SecretsResolver != nil {
		resolver = deps.SecretsResolver
	} else {
		// Production: select backend via SECRETS_BACKEND env var (dev|onepassword|vault).
		// Defaults to "dev" when unset.
		p, err := secrets.NewProvider(ctx)
		if err != nil {
			return fmt.Errorf("deploy: init secrets provider: %w", err)
		}
		resolver = secrets.NewResolverFromProvider(p)
	}

	runner := deps.Runner
	if runner == nil {
		runner = secrets.ExecCommandRunner{}
	}

	// -------------------------------------------------------------------------
	// Step 1: Fetch the release manifest from GitHub releases.
	// -------------------------------------------------------------------------
	fmt.Fprintf(os.Stderr, "[1/4] Fetching manifest for %s@%s...\n", service, version)
	manifestBytes, err := fetchFn(ctx, "FoundryFabric", service, version, githubToken)
	if err != nil {
		return fmt.Errorf("[1/4] failed: %w", err)
	}

	// -------------------------------------------------------------------------
	// Step 2: Resolve secrets and create/update the k8s Secret.
	// -------------------------------------------------------------------------
	fmt.Fprintf(os.Stderr, "[2/4] Resolving secrets (env=%s)...\n", env)
	secretMap, err := resolver.Resolve(ctx, service, env, "hetzner", "ash")
	if err != nil {
		return fmt.Errorf("[2/4] failed: %w", err)
	}

	secretName := service + "-secrets"
	fmt.Fprintf(os.Stderr, "[2/4] Creating/updating k8s Secret %q...\n", secretName)
	if err := applyGenericSecret(ctx, runner, kubeconfigPath, secretName, secretMap); err != nil {
		return fmt.Errorf("[2/4] failed: %w", err)
	}

	// -------------------------------------------------------------------------
	// Step 3: kubectl apply -f manifest.yaml
	// -------------------------------------------------------------------------
	fmt.Fprintln(os.Stderr, "[3/4] Applying manifest...")
	manifestFile, cleanup, err := writeTempManifest(manifestBytes)
	if err != nil {
		return fmt.Errorf("[3/4] failed: %w", err)
	}
	defer cleanup()

	if _, err := runner.Run(ctx, "kubectl",
		"--kubeconfig", kubeconfigPath,
		"apply", "-f", manifestFile,
	); err != nil {
		return fmt.Errorf("[3/4] kubectl apply failed: %w", err)
	}

	// -------------------------------------------------------------------------
	// Step 4: kubectl rollout status
	// -------------------------------------------------------------------------
	fmt.Fprintf(os.Stderr, "[4/4] Waiting for rollout of %s...\n", service)
	if _, err := runner.Run(ctx, "kubectl",
		"--kubeconfig", kubeconfigPath,
		"rollout", "status", "deployment/"+service,
		"--timeout=120s",
	); err != nil {
		return fmt.Errorf("[4/4] rollout status failed: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Deploy of %s@%s to cluster %q complete.\n", service, version, cluster)
	return nil
}

// recordDeploySuccess updates the deployments row and appends a rolled_out
// history entry. All registry writes are best-effort: any failure is logged to
// stderr and never propagates to the caller, since a successful kubectl
// rollout must not be reported as a failure just because the local cache could
// not be updated.
func recordDeploySuccess(ctx context.Context, deps DeployDeps, cluster, service, version string, attemptedAt time.Time) {
	reg, err := openRegistryForDeploy(ctx, deps)
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
	if err := reg.UpsertDeployment(ctx, registry.Deployment{
		ClusterName: cluster,
		Service:     service,
		Version:     version,
		DeployedAt:  now,
		DeployedBy:  currentUser(),
		Status:      registry.StatusRolledOut,
	}); err != nil {
		// The cluster was updated successfully; log at ERROR level so the
		// operator knows the local registry is now stale and status/diff
		// will show incorrect results until a sync resolves the divergence.
		fmt.Fprintf(os.Stderr, "ERROR: registry write failed — cluster updated but local registry is stale: %v\n", err)
		// Fall through: still try to append a history row so the audit
		// trail captures what happened.
	}

	if err := reg.AppendHistory(ctx, registry.DeploymentHistoryEntry{
		ClusterName:       cluster,
		Service:           service,
		Version:           version,
		AttemptedAt:       attemptedAt,
		Status:            registry.StatusRolledOut,
		RolloutDurationMs: now.Sub(attemptedAt).Milliseconds(),
		Error:             "",
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", err)
	}
}

// recordDeployFailure appends a failed history entry. It deliberately does NOT
// touch the deployments row so the "current version" cache remains the last
// successfully rolled-out version. Best-effort: failures only log to stderr.
func recordDeployFailure(ctx context.Context, deps DeployDeps, cluster, service, version string, attemptedAt time.Time, deployErr error) {
	reg, err := openRegistryForDeploy(ctx, deps)
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
	if err := reg.AppendHistory(ctx, registry.DeploymentHistoryEntry{
		ClusterName:       cluster,
		Service:           service,
		Version:           version,
		AttemptedAt:       attemptedAt,
		Status:            registry.StatusFailed,
		RolloutDurationMs: now.Sub(attemptedAt).Milliseconds(),
		Error:             deployErr.Error(),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: registry write failed: %v\n", err)
	}
}

// openRegistryForDeploy resolves the registry constructor, defaulting to the
// production registry.NewRegistry when deps does not override it.
func openRegistryForDeploy(ctx context.Context, deps DeployDeps) (registry.Registry, error) {
	open := deps.OpenRegistry
	if open == nil {
		open = registry.NewRegistry
	}
	return open(ctx)
}

// currentUser returns the operator's login name for audit purposes. Falls
// back through USER, LOGNAME, and finally a literal "unknown" so the
// DeployedBy column is never empty.
func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("LOGNAME"); u != "" {
		return u
	}
	return "unknown"
}

// applyGenericSecret creates or replaces a k8s generic Secret containing the
// provided key-value pairs. The secret values are never written to error messages.
func applyGenericSecret(ctx context.Context, runner secrets.CommandRunner, kubeconfig, name string, data map[string]string) error {
	// Delete the existing secret first (ignore not-found errors) then recreate.
	_, _ = runner.Run(ctx, "kubectl",
		"--kubeconfig", kubeconfig,
		"delete", "secret", name,
		"--namespace", "default",
		"--ignore-not-found",
	)

	args := []string{
		"--kubeconfig", kubeconfig,
		"create", "secret", "generic", name,
		"--namespace", "default",
	}
	for k, v := range data {
		args = append(args, fmt.Sprintf("--from-literal=%s=%s", k, v))
	}

	if _, err := runner.Run(ctx, "kubectl", args...); err != nil {
		return fmt.Errorf("secrets: create %q: kubectl exited non-zero", name)
	}
	return nil
}

// writeTempManifest writes bytes to a temp file and returns its path plus a
// cleanup function that removes it. The caller must invoke cleanup when done.
func writeTempManifest(data []byte) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "clusterbox-manifest-*.yaml")
	if err != nil {
		return "", nil, fmt.Errorf("deploy: create temp manifest file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("deploy: write temp manifest file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("deploy: close temp manifest file: %w", err)
	}
	name := f.Name()
	return name, func() { _ = os.Remove(name) }, nil
}

// execDeployRun is a CommandRunner that shells out via os/exec.
// It satisfies secrets.CommandRunner and is used by the deploy command for
// kubectl invocations that don't need stdin piping.
type execDeployRun struct{}

func (execDeployRun) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w\noutput: %s", name, err, out)
	}
	return out, nil
}
