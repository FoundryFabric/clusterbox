// Package addon's installer.go applies a parsed Addon to a target cluster by
// resolving the addon's required secrets, validating its prerequisites,
// rendering its manifests with simple ${SECRET_NAME} placeholder substitution,
// and shelling out to kubectl. Successful installs (and uninstalls) are
// recorded in the local registry so `clusterbox status` and `clusterbox diff`
// can see them.
//
// The installer mirrors cmd/deploy.go's kubectl-shellout convention rather
// than using the Kubernetes Go client: every k8s side-effect is a single
// `kubectl apply -f <tmpfile>` (or `kubectl delete -f <tmpfile>
// --ignore-not-found`) so the installer behaves identically to a human
// operator pasting the same command into a terminal.
package addon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/secrets"
)

// gitkeepName is preserved by the catalog loader so empty manifest trees can
// round-trip through the embedded FS; the installer skips it by name.
const gitkeepName = ".gitkeep"

// helmChartManifestPath is the well-known path (relative to the addon root)
// of the HelmChart resource file. For StrategyHelmChart addons, this file is
// applied after all other manifests so the namespace and credential Secret
// are present when the k3s HelmChart controller wakes up.
const helmChartManifestPath = "manifests/helmchart.yaml"

// secretPlaceholderRE matches a single ${NAME} token inside a manifest body.
// The character class is deliberately tight: NAME must be a non-empty run of
// uppercase ASCII letters, digits, or underscore, mirroring shell-style
// environment-variable conventions and matching the keys produced by the
// secrets backends. Anything not matching the pattern (e.g. ${ } or
// ${not-a-secret}) is left in place by Render so accidental substitutions
// inside Helm/Kustomize templates do not happen.
var secretPlaceholderRE = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

// Installer renders and applies an addon's manifests to a target cluster,
// then records the install in the registry. Every external dependency is an
// interface so tests can drive the full happy/error matrix without touching
// the network, the filesystem, or kubectl.
type Installer struct {
	// Catalog is the source of Addon definitions. Required.
	Catalog *Catalog

	// Secrets resolves a flat key→value map for (app=addonName, env, provider,
	// region) tuples. Required.
	Secrets secrets.Resolver

	// Kubectl runs kubectl. The installer never invokes any other binary
	// through this runner, but the secrets.CommandRunner interface is reused
	// to keep dependency injection consistent with cmd/deploy.go.
	Kubectl secrets.CommandRunner

	// Registry is where install/uninstall outcomes are recorded. Required.
	Registry registry.Registry

	// Now returns the current time. nil falls back to time.Now.
	Now func() time.Time

	// DeployedBy returns the operator's audit identity. nil falls back to
	// the USER environment variable, then to "unknown".
	DeployedBy func() string
}

// Install runs the full install sequence:
//
//  1. Look up the addon in the catalog.
//  2. Look up the cluster in the registry to derive the env/provider/region
//     used for secret resolution and the kubeconfig path used by kubectl.
//  3. Resolve the secret bundle for that cluster and verify every required
//     addon secret is present (collecting all missing keys before erroring).
//  4. Verify every addon listed in addon.Requires is currently installed on
//     the cluster as a kind=addon deployment.
//  5. Render every manifest file with ${SECRET_NAME} placeholder substitution.
//  6. Write the rendered manifests to a single temp file and run
//     `kubectl --kubeconfig <path> apply -f <tmpfile>`.
//  7. On success, upsert a deployments row (kind=addon) and append a
//     rolled_out history row.
//  8. On any failure, append a failed history row and return the original
//     error. The deployments row is never written on failure paths.
//
// Supported strategies: StrategyManifests, StrategyHelmChart, StrategyStaged.
// For helmchart, supporting manifests are applied before the HelmChart CR.
// For staged, mode selects the manifest subdirectory; an empty mode defaults
// to the first entry in addon.Modes.
func (i *Installer) Install(ctx context.Context, addonName, clusterName, mode string) error {
	attemptedAt := i.now()
	a, c, err := i.lookup(ctx, addonName, clusterName)
	if err != nil {
		i.recordFailure(ctx, addonName, "", clusterName, attemptedAt, err)
		return err
	}

	resolved, err := i.resolveSecrets(ctx, a, c)
	if err != nil {
		i.recordFailure(ctx, a.Name, a.Version, clusterName, attemptedAt, err)
		return err
	}

	if err := i.checkRequires(ctx, a, clusterName); err != nil {
		i.recordFailure(ctx, a.Name, a.Version, clusterName, attemptedAt, err)
		return err
	}

	if err := i.applyManifests(ctx, a, c, resolved, mode); err != nil {
		i.recordFailure(ctx, a.Name, a.Version, clusterName, attemptedAt, err)
		return err
	}

	finishedAt := i.now()
	if err := i.recordSuccess(ctx, a, clusterName, finishedAt, attemptedAt); err != nil {
		// recordSuccess returns the first hard failure; the kubectl apply
		// already succeeded so we wrap the error to make it clear the cluster
		// state is ahead of the registry.
		return fmt.Errorf("addon %q installed on %q but registry write failed: %w", a.Name, clusterName, err)
	}
	return nil
}

// Uninstall removes an addon from a cluster. The flow is the inverse of
// Install: look up the registry row to learn which version is on the cluster,
// re-render the matching catalog manifests, run `kubectl delete -f
// --ignore-not-found`, then delete the deployments row and append a
// StatusUninstalled history entry.
//
// Behaviour notes:
//   - If the addon has no deployments row for this cluster, Uninstall
//     returns a descriptive "not installed" error.
//   - If the catalog version differs from the registry's recorded version,
//     a warning is printed to stderr and the uninstall proceeds with what is
//     in the catalog. (The alternative — refusing to uninstall — leaves the
//     operator stuck if the catalog has rolled forward.)
//   - kubectl delete uses --ignore-not-found so the operation is idempotent
//     against partially-installed addons.
//   - Best-effort registry/kubectl errors after the kubectl delete returns
//     are logged; Uninstall returns the first hard error.
func (i *Installer) Uninstall(ctx context.Context, addonName, clusterName string) error {
	attemptedAt := i.now()

	c, err := i.Registry.GetCluster(ctx, clusterName)
	if err != nil {
		return fmt.Errorf("addon %q: lookup cluster %q: %w", addonName, clusterName, err)
	}

	dep, err := i.Registry.GetDeployment(ctx, clusterName, addonName)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return fmt.Errorf("addon %q: not installed on cluster %q", addonName, clusterName)
		}
		return fmt.Errorf("addon %q: lookup deployment on %q: %w", addonName, clusterName, err)
	}
	if dep.Kind != registry.KindAddon {
		return fmt.Errorf("addon %q: registry row on %q is kind=%q, refusing to uninstall as addon",
			addonName, clusterName, dep.Kind)
	}

	a, err := i.Catalog.Get(addonName)
	if err != nil {
		return fmt.Errorf("addon %q: %w", addonName, err)
	}
	if a.Version != dep.Version {
		_, _ = fmt.Fprintf(os.Stderr,
			"warning: addon %q catalog version %q differs from installed version %q on %q; uninstalling using catalog manifests\n",
			a.Name, a.Version, dep.Version, clusterName)
	}

	// Resolve secrets so manifests with placeholders re-render identically
	// to install. We do not enforce required-secret validation here: missing
	// secrets must not block a removal. We do log resolution failures so the
	// operator knows secrets are absent and can investigate if needed.
	resolved, resolveErr := i.Secrets.Resolve(ctx, a.Name, c.Env, c.Provider, c.Region)
	if resolveErr != nil {
		_, _ = fmt.Fprintf(os.Stderr,
			"warning: addon %q: resolve secrets for cluster %q: %v (proceeding with empty secrets)\n",
			a.Name, clusterName, resolveErr)
	}
	if resolved == nil {
		resolved = map[string]string{}
	}

	rendered, err := renderManifests(a, resolved)
	if err != nil {
		i.recordFailure(ctx, a.Name, dep.Version, clusterName, attemptedAt, err)
		return err
	}
	if len(rendered) == 0 {
		// Empty manifest tree (only .gitkeep): nothing to delete on the
		// cluster. We still tear the registry row down below.
	} else {
		path, cleanup, err := writeTempManifests(rendered)
		if err != nil {
			i.recordFailure(ctx, a.Name, dep.Version, clusterName, attemptedAt, err)
			return err
		}
		defer cleanup()

		if _, err := i.Kubectl.Run(ctx, "kubectl",
			"--kubeconfig", c.KubeconfigPath,
			"delete", "-f", path,
			"--ignore-not-found",
		); err != nil {
			err = fmt.Errorf("addon %q: kubectl delete on %q failed: %w", a.Name, clusterName, err)
			i.recordFailure(ctx, a.Name, dep.Version, clusterName, attemptedAt, err)
			return err
		}
	}

	// Capture duration immediately after kubectl returns so registry cleanup
	// time is not included in the rollout duration written to history.
	finishedAt := i.now()

	// Best-effort: delete the deployments row, then append the history entry.
	// If the row delete fails we still try to record the uninstall in history
	// so the audit trail captures intent.
	var firstHardErr error
	if err := i.Registry.DeleteDeployment(ctx, clusterName, a.Name); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: registry delete deployment failed: %v\n", err)
		firstHardErr = fmt.Errorf("addon %q: registry delete on %q: %w", a.Name, clusterName, err)
	}
	if err := i.Registry.AppendHistory(ctx, registry.DeploymentHistoryEntry{
		ClusterName:       clusterName,
		Service:           a.Name,
		Version:           dep.Version,
		AttemptedAt:       attemptedAt,
		Status:            registry.StatusUninstalled,
		RolloutDurationMs: finishedAt.Sub(attemptedAt).Milliseconds(),
		Kind:              registry.KindAddon,
	}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: registry append history failed: %v\n", err)
		if firstHardErr == nil {
			firstHardErr = fmt.Errorf("addon %q: registry append history on %q: %w", a.Name, clusterName, err)
		}
	}
	return firstHardErr
}

// Upgrade re-runs the install flow against the cluster. Since `kubectl apply`
// is idempotent and Install upserts the deployments row, an upgrade is
// effectively the same code path as Install — there is no special pre-step
// for "previous version exists". The deployments row's Version and DeployedAt
// columns are updated, and a fresh rolled_out history row is appended with
// the new version.
func (i *Installer) Upgrade(ctx context.Context, addonName, clusterName, mode string) error {
	return i.Install(ctx, addonName, clusterName, mode)
}

// lookup loads the addon and its target cluster. It is the first step of
// Install and centralises the catalog/registry calls so the error wrapping is
// uniform.
func (i *Installer) lookup(ctx context.Context, addonName, clusterName string) (*Addon, registry.Cluster, error) {
	a, err := i.Catalog.Get(addonName)
	if err != nil {
		return nil, registry.Cluster{}, fmt.Errorf("addon %q: %w", addonName, err)
	}
	c, err := i.Registry.GetCluster(ctx, clusterName)
	if err != nil {
		return nil, registry.Cluster{}, fmt.Errorf("addon %q: lookup cluster %q: %w", addonName, clusterName, err)
	}
	return a, c, nil
}

// resolveSecrets fetches the secret bundle for the addon's deployment target
// and validates that every required secret in the addon manifest resolved to
// a non-empty value. Missing required secrets are collected so the operator
// sees them all in a single error rather than a one-at-a-time stutter.
//
// Optional secrets that are absent are silently dropped from the rendered
// map so manifests can use `${OPTIONAL_KEY}` placeholders that fall back to
// the empty string when not configured.
func (i *Installer) resolveSecrets(ctx context.Context, a *Addon, c registry.Cluster) (map[string]string, error) {
	// Use the cluster name as the env identifier when no explicit env has been
	// stored (e.g. local k3d clusters provisioned without --env).
	env := c.Env
	if env == "" {
		env = c.Name
	}
	bundle, err := i.Secrets.Resolve(ctx, a.Name, env, c.Provider, c.Region)
	if err != nil {
		return nil, fmt.Errorf("addon %q: resolve secrets for cluster %q: %w", a.Name, c.Name, err)
	}
	if bundle == nil {
		bundle = map[string]string{}
	}

	var missing []string
	for _, s := range a.Secrets {
		if !s.Required {
			continue
		}
		if v, ok := bundle[s.Key]; !ok || v == "" {
			missing = append(missing, s.Key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("addon %q: missing required secrets for cluster %q (env=%s provider=%s region=%s): %s",
			a.Name, c.Name, c.Env, c.Provider, c.Region, strings.Join(missing, ", "))
	}
	return bundle, nil
}

// checkRequires verifies every name in addon.Requires has a kind=addon
// deployment row on the target cluster. Missing prerequisites are collected
// into a single error.
func (i *Installer) checkRequires(ctx context.Context, a *Addon, clusterName string) error {
	if len(a.Requires) == 0 {
		return nil
	}
	deps, err := i.Registry.ListDeployments(ctx, clusterName)
	if err != nil {
		return fmt.Errorf("addon %q: list deployments on %q: %w", a.Name, clusterName, err)
	}
	have := make(map[string]bool, len(deps))
	for _, d := range deps {
		if d.Kind == registry.KindAddon {
			have[d.Service] = true
		}
	}
	var missing []string
	for _, r := range a.Requires {
		if !have[r] {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("addon %q: required addons not installed on cluster %q: %s",
			a.Name, clusterName, strings.Join(missing, ", "))
	}
	return nil
}

// applyManifests renders and applies the addon's manifests to the cluster.
// The behaviour differs by strategy:
//
//   - StrategyManifests: all manifests are rendered and applied in a single
//     kubectl apply call, in lexicographic file-name order.
//
//   - StrategyHelmChart: supporting manifests (namespace, secrets, etc.) are
//     applied first, then manifests/helmchart.yaml is applied in a second
//     kubectl call. The two-pass order ensures the k3s HelmChart controller
//     finds the target namespace and credential Secret ready when it wakes up.
//
//   - StrategyStaged: mode selects a manifest subdirectory; see applyStaged.
func (i *Installer) applyManifests(ctx context.Context, a *Addon, c registry.Cluster, resolved map[string]string, mode string) error {
	switch a.Strategy {
	case StrategyManifests:
		return i.applyManifestSet(ctx, a.Name, c, a.Manifests, resolved, false)
	case StrategyHelmChart:
		return i.applyHelmChart(ctx, a, c, resolved)
	case StrategyStaged:
		return i.applyStaged(ctx, a, c, resolved, mode)
	default:
		return fmt.Errorf("addon %q: strategy %q is not supported", a.Name, a.Strategy)
	}
}

// applyHelmChart implements the two-pass apply for StrategyHelmChart addons.
func (i *Installer) applyHelmChart(ctx context.Context, a *Addon, c registry.Cluster, resolved map[string]string) error {
	hcData, ok := a.Manifests[helmChartManifestPath]
	if !ok {
		return fmt.Errorf("addon %q: helmchart strategy requires %s", a.Name, helmChartManifestPath)
	}

	// Pass 1: supporting manifests (namespace, secrets, …) — may be empty.
	supporting := make(map[string][]byte, len(a.Manifests))
	for p, data := range a.Manifests {
		if p != helmChartManifestPath {
			supporting[p] = data
		}
	}
	if err := i.applyManifestSet(ctx, a.Name, c, supporting, resolved, true); err != nil {
		return err
	}

	// Pass 2: HelmChart resource — k3s picks this up asynchronously.
	helmChart := map[string][]byte{helmChartManifestPath: hcData}
	return i.applyManifestSet(ctx, a.Name, c, helmChart, resolved, false)
}

// applyStaged implements StrategyStaged. It applies manifests in up to three
// phases depending on mode and directory layout:
//
//  1. Common manifests: top-level files directly under manifests/ (e.g. a
//     shared namespace). Applied before any mode-specific content.
//
//     2a. Simple mode (no operators/ sub-dir): all files under manifests/<mode>/
//     are applied in a single kubectl call.
//
//     2b. Phased mode (operators/ sub-dir present):
//     - Apply manifests/<mode>/operators/ files.
//     - Poll kubectl wait for every HelmChart job discovered in those files.
//     - Apply manifests/<mode>/instances/ files.
func (i *Installer) applyStaged(ctx context.Context, a *Addon, c registry.Cluster, resolved map[string]string, mode string) error {
	if mode == "" && len(a.Modes) > 0 {
		mode = a.Modes[0]
	}
	validMode := false
	for _, m := range a.Modes {
		if m == mode {
			validMode = true
			break
		}
	}
	if !validMode {
		return fmt.Errorf("addon %q: unsupported mode %q (supported: %s)", a.Name, mode, strings.Join(a.Modes, ", "))
	}

	// Phase 1: common top-level files (directly in manifests/, no subdirectory).
	common := topLevelManifests(a.Manifests, "manifests/")
	if err := i.applyManifestSet(ctx, a.Name, c, common, resolved, true); err != nil {
		return err
	}

	modePrefix := "manifests/" + mode + "/"
	operatorsPrefix := modePrefix + "operators/"
	operatorFiles := filterByPrefix(a.Manifests, operatorsPrefix)

	if len(operatorFiles) == 0 {
		// Simple mode: apply everything under manifests/<mode>/ in one pass.
		modeFiles := filterByPrefix(a.Manifests, modePrefix)
		return i.applyManifestSet(ctx, a.Name, c, modeFiles, resolved, false)
	}

	// Phased mode: operators → poll → instances.
	if err := i.applyManifestSet(ctx, a.Name, c, operatorFiles, resolved, false); err != nil {
		return err
	}
	if err := i.pollHelmChartJobs(ctx, c, operatorFiles); err != nil {
		return err
	}
	instanceFiles := filterByPrefix(a.Manifests, modePrefix+"instances/")
	return i.applyManifestSet(ctx, a.Name, c, instanceFiles, resolved, false)
}

// pollHelmChartJobs scans operatorFiles for HelmChart resources and waits for
// the corresponding helm-install-<name> Jobs to complete in kube-system.
// It is a no-op when no HelmChart resources are found.
func (i *Installer) pollHelmChartJobs(ctx context.Context, c registry.Cluster, operatorFiles map[string][]byte) error {
	type helmChartMeta struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
	}

	var jobNames []string
	for _, data := range operatorFiles {
		for _, doc := range bytes.Split(data, []byte("\n---")) {
			var m helmChartMeta
			if err := yaml.Unmarshal(doc, &m); err != nil {
				continue
			}
			if m.Kind == "HelmChart" && m.Metadata.Name != "" {
				jobNames = append(jobNames, "helm-install-"+m.Metadata.Name)
			}
		}
	}
	if len(jobNames) == 0 {
		return nil
	}
	sort.Strings(jobNames)

	for _, job := range jobNames {
		if _, err := i.Kubectl.Run(ctx, "kubectl",
			"--kubeconfig", c.KubeconfigPath,
			"wait",
			"--for=condition=complete",
			"job/"+job,
			"-n", "kube-system",
			"--timeout=5m",
		); err != nil {
			return fmt.Errorf("addon: timed out waiting for %s in kube-system: %w", job, err)
		}
	}
	return nil
}

// topLevelManifests returns files whose path matches prefix<filename> with no
// further slashes — i.e. files directly in the given directory, not nested.
func topLevelManifests(m map[string][]byte, prefix string) map[string][]byte {
	out := map[string][]byte{}
	for p, data := range m {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rel := p[len(prefix):]
		if !strings.Contains(rel, "/") {
			out[p] = data
		}
	}
	return out
}

// filterByPrefix returns all entries whose path starts with prefix.
func filterByPrefix(m map[string][]byte, prefix string) map[string][]byte {
	out := map[string][]byte{}
	for p, data := range m {
		if strings.HasPrefix(p, prefix) {
			out[p] = data
		}
	}
	return out
}

// applyManifestSet renders a manifest map and runs kubectl apply against the
// cluster. When allowEmpty is true an empty rendered result is a no-op;
// when false it is an error (used for the primary/only manifest pass).
func (i *Installer) applyManifestSet(ctx context.Context, addonName string, c registry.Cluster, manifests map[string][]byte, resolved map[string]string, allowEmpty bool) error {
	rendered, err := renderManifestMap(manifests, resolved)
	if err != nil {
		return fmt.Errorf("addon %q: render manifests: %w", addonName, err)
	}
	if len(rendered) == 0 {
		if allowEmpty {
			return nil
		}
		return fmt.Errorf("addon %q: no manifests to apply (empty manifests/ directory)", addonName)
	}

	path, cleanup, err := writeTempManifests(rendered)
	if err != nil {
		return fmt.Errorf("addon %q: %w", addonName, err)
	}
	defer cleanup()

	if _, err := i.Kubectl.Run(ctx, "kubectl",
		"--kubeconfig", c.KubeconfigPath,
		"apply", "-f", path,
	); err != nil {
		return fmt.Errorf("addon %q: kubectl apply on %q failed: %w", addonName, c.Name, err)
	}
	return nil
}

// recordSuccess upserts the deployments row and appends a rolled_out history
// entry. Unlike cmd/deploy.go, the install path returns registry write errors
// to the caller: an addon installer that silently loses track of what it
// installed is a debugging nightmare.
func (i *Installer) recordSuccess(ctx context.Context, a *Addon, clusterName string, finishedAt, attemptedAt time.Time) error {
	if err := i.Registry.UpsertDeployment(ctx, registry.Deployment{
		ClusterName: clusterName,
		Service:     a.Name,
		Version:     a.Version,
		DeployedAt:  finishedAt,
		DeployedBy:  i.deployedBy(),
		Status:      registry.StatusRolledOut,
		Kind:        registry.KindAddon,
	}); err != nil {
		return fmt.Errorf("upsert deployment: %w", err)
	}
	if err := i.Registry.AppendHistory(ctx, registry.DeploymentHistoryEntry{
		ClusterName:       clusterName,
		Service:           a.Name,
		Version:           a.Version,
		AttemptedAt:       attemptedAt,
		Status:            registry.StatusRolledOut,
		RolloutDurationMs: finishedAt.Sub(attemptedAt).Milliseconds(),
		Kind:              registry.KindAddon,
	}); err != nil {
		return fmt.Errorf("append history: %w", err)
	}
	return nil
}

// recordFailure appends a failed history row. Failures here are logged to
// stderr and never surface to the caller: returning a registry-write error in
// place of the original install error would be misleading.
func (i *Installer) recordFailure(ctx context.Context, addonName, addonVersion, clusterName string, attemptedAt time.Time, cause error) {
	finishedAt := i.now()
	if err := i.Registry.AppendHistory(ctx, registry.DeploymentHistoryEntry{
		ClusterName:       clusterName,
		Service:           addonName,
		Version:           addonVersion,
		AttemptedAt:       attemptedAt,
		Status:            registry.StatusFailed,
		RolloutDurationMs: finishedAt.Sub(attemptedAt).Milliseconds(),
		Error:             cause.Error(),
		Kind:              registry.KindAddon,
	}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: addon %q: append failed-history on %q: %v\n", addonName, clusterName, err)
	}
}

func (i *Installer) now() time.Time {
	if i.Now != nil {
		return i.Now().UTC()
	}
	return time.Now().UTC()
}

func (i *Installer) deployedBy() string {
	if i.DeployedBy != nil {
		if u := i.DeployedBy(); u != "" {
			return u
		}
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

// renderManifests substitutes ${SECRET_NAME} placeholders in every manifest
// file under the addon's manifests/ tree.
func renderManifests(a *Addon, resolved map[string]string) ([]byte, error) {
	return renderManifestMap(a.Manifests, resolved)
}

// renderManifestMap is the core rendering routine. It substitutes
// ${SECRET_NAME} placeholders in the given manifest map, skipping .gitkeep.
// Files are processed in lexicographic path order for deterministic output.
// Unknown placeholders are left untouched so the rendered manifest fails
// loudly at kubectl-apply time rather than silently substituting empty strings.
func renderManifestMap(manifests map[string][]byte, resolved map[string]string) ([]byte, error) {
	paths := make([]string, 0, len(manifests))
	for p := range manifests {
		base := p
		if idx := strings.LastIndex(base, "/"); idx >= 0 {
			base = base[idx+1:]
		}
		if base == gitkeepName {
			continue
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var out []byte
	for _, p := range paths {
		body := manifests[p]
		rendered := substitutePlaceholders(body, resolved)
		if len(out) > 0 {
			if !strings.HasSuffix(string(out), "\n") {
				out = append(out, '\n')
			}
			out = append(out, []byte("---\n")...)
		}
		out = append(out, rendered...)
	}
	return out, nil
}

// substitutePlaceholders runs the `${KEY}` substitution over a single manifest
// file. The regex only matches keys composed of [A-Z0-9_]+, so YAML constructs
// like `${{ }}` (empty body) or `${chart.version}` are not touched. Keys that
// match the regex but are not present in resolved are returned unchanged so
// the rendered manifest fails loudly at kubectl-apply time rather than
// silently substituting an empty string.
func substitutePlaceholders(body []byte, resolved map[string]string) []byte {
	return secretPlaceholderRE.ReplaceAllFunc(body, func(match []byte) []byte {
		// Strip the leading "${" and trailing "}" — guaranteed to be 3 bytes
		// of overhead by the regex pattern. Looking up the key directly off
		// the byte slice avoids a string allocation when the key is absent.
		if v, ok := resolved[string(match[2:len(match)-1])]; ok {
			return []byte(v)
		}
		return match
	})
}

// writeTempManifests writes the rendered manifests blob to a temp file and
// returns its path plus a cleanup func that always removes the file. The
// caller MUST defer the cleanup; even error paths in the caller benefit
// because we only return cleanup on success.
func writeTempManifests(rendered []byte) (string, func(), error) {
	f, err := os.CreateTemp("", "clusterbox-addon-*.yaml")
	if err != nil {
		return "", nil, fmt.Errorf("create temp manifest file: %w", err)
	}
	if _, err := f.Write(rendered); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("write temp manifest file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("close temp manifest file: %w", err)
	}
	name := f.Name()
	return name, func() { _ = os.Remove(name) }, nil
}
