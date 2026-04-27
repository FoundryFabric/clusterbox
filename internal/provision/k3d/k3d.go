// Package k3d implements a local-cluster provision.Provider that wraps
// the k3d CLI (k3s in Docker). It works on any platform where Docker
// and k3d are available — Mac, Linux, and Windows — without requiring
// a cloud account or SSH access.
//
// If k3d is not in PATH, the provider downloads the pinned release
// from GitHub and caches it under the OS cache directory so subsequent
// runs are instant.
package k3d

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/registry"
)

// Name is the canonical --provider value for this provider.
const Name = "k3d"

// BundledK3dVersion is the k3d release downloaded when k3d is absent
// from PATH. Kept in sync with the version validated in CI.
const BundledK3dVersion = "v5.7.5"

// Runner is the minimal interface the k3d provider needs to execute
// external commands. Production code uses execRunner; tests substitute
// a stub that captures invocations without spawning real processes.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Deps groups the injectable dependencies for the k3d provider.
// Tests replace fields; nil fields fall back to production defaults.
type Deps struct {
	// Nodes is the total number of cluster nodes to create:
	// 1 server + (Nodes-1) agents. Zero and one both yield a
	// single-server cluster with no agents.
	Nodes int

	// K3sVersion, when non-empty, is forwarded to k3d as
	// --image rancher/k3s:<K3sVersion>. Empty lets k3d pick its
	// bundled default.
	K3sVersion string

	// KubeconfigPath is the destination the provider writes the
	// merged kubeconfig to. When empty the provider derives
	// $HOME/.kube/<clusterName>.yaml.
	KubeconfigPath string

	// K3dBin is the path to the k3d binary. When non-empty it is used
	// directly, skipping PATH lookup and auto-download. Tests set this
	// to a stub value so the Runner intercepts calls without triggering
	// a real download.
	K3dBin string

	// Runner executes external commands. Defaults to execRunner{}.
	Runner Runner

	// Out is the destination for human-readable progress lines.
	// When nil the provider writes to os.Stderr.
	Out io.Writer
}

// Provider is the k3d implementation of provision.Provider.
type Provider struct {
	deps Deps
}

// New constructs a k3d Provider with the given dependencies.
func New(deps Deps) *Provider {
	return &Provider{deps: deps}
}

// Name returns the canonical provider identifier ("k3d").
func (p *Provider) Name() string { return Name }

// Provision creates a k3d cluster and writes its kubeconfig to disk.
//
// The flow:
//  1. Locate k3d (PATH → cache → auto-download from GitHub).
//  2. k3d cluster create <name> [--agents N] [--image …]
//  3. k3d kubeconfig get <name> → write to KubeconfigPath (mode 0600).
//  4. Return a ProvisionResult with one Node row per node.
//
// Re-running is idempotent: if the cluster already exists k3d exits
// with a non-zero code containing "already exists"; the provider
// detects this, skips create, and refreshes the kubeconfig.
func (p *Provider) Provision(ctx context.Context, cfg provision.ClusterConfig) (provision.ProvisionResult, error) {
	out := p.out()
	run := p.runner()
	name := cfg.ClusterName

	kubeconfigPath, err := p.kubeconfigPath(name)
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	// Step 1: locate or download k3d.
	fmt.Fprintln(out, "[1/3] Locating k3d...")
	bin, err := p.resolveK3dBin(ctx, out)
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	// Step 2: create the cluster (idempotent).
	fmt.Fprintf(out, "[2/3] Creating k3d cluster %q...\n", name)
	if err := p.createCluster(ctx, run, bin, name); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("k3d cluster create: %w", err)
	}

	// Step 3: export kubeconfig.
	fmt.Fprintf(out, "[3/3] Writing kubeconfig to %s...\n", kubeconfigPath)
	if err := p.exportKubeconfig(ctx, run, bin, name, kubeconfigPath); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("k3d kubeconfig get: %w", err)
	}

	nodes := p.nodeRows(name)
	return provision.ProvisionResult{
		KubeconfigPath: kubeconfigPath,
		Nodes:          nodes,
	}, nil
}

// Destroy deletes the k3d cluster and removes the kubeconfig file.
//
// It is idempotent: if the cluster does not exist k3d exits cleanly
// (or with a "not found" message that the provider ignores).
func (p *Provider) Destroy(ctx context.Context, cluster registry.Cluster) error {
	out := p.out()
	run := p.runner()
	name := cluster.Name

	bin, err := p.resolveK3dBin(ctx, out)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "[1/2] Deleting k3d cluster %q...\n", name)
	output, err := run.Run(ctx, bin, "cluster", "delete", name)
	if err != nil {
		if strings.Contains(strings.ToLower(string(output)), "no cluster found") {
			fmt.Fprintf(out, "Cluster %q not found in k3d; nothing to delete.\n", name)
		} else {
			return fmt.Errorf("k3d cluster delete %s: %w", name, err)
		}
	}

	fmt.Fprintln(out, "[2/2] Removing kubeconfig...")
	kubeconfigPath := cluster.KubeconfigPath
	if kubeconfigPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			kubeconfigPath = filepath.Join(home, ".kube", name+".yaml")
		}
	}
	if kubeconfigPath != "" {
		if err := os.Remove(kubeconfigPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(out, "warning: could not remove kubeconfig %s: %v\n", kubeconfigPath, err)
		}
	}

	return nil
}

// Reconcile inspects the local k3d state and returns a summary. It
// never mutates provider-side state.
func (p *Provider) Reconcile(ctx context.Context, clusterName string) (provision.ReconcileSummary, error) {
	out := p.out()
	run := p.runner()

	bin, err := p.resolveK3dBin(ctx, out)
	if err != nil {
		return provision.ReconcileSummary{}, err
	}

	output, err := run.Run(ctx, bin, "cluster", "list", "-o", "json")
	if err != nil {
		return provision.ReconcileSummary{}, fmt.Errorf("k3d cluster list: %w", err)
	}

	var clusters []k3dCluster
	if err := json.Unmarshal(output, &clusters); err != nil {
		return provision.ReconcileSummary{}, fmt.Errorf("k3d cluster list: parse JSON: %w", err)
	}

	var summary provision.ReconcileSummary
	found := false
	for _, c := range clusters {
		if c.Name == clusterName {
			found = true
			summary.Existing++
		} else {
			summary.Unmanaged = append(summary.Unmanaged, c.Name)
		}
	}
	if !found {
		summary.MarkedDestroyed++
	}

	return summary, nil
}

// ---- resolution ------------------------------------------------------------

// resolveK3dBin returns the path to the k3d binary, downloading it if
// necessary. Resolution order:
//
//  1. Deps.K3dBin (tests / explicit override)
//  2. k3d in PATH
//  3. Cached binary at <UserCacheDir>/clusterbox/k3d/k3d
//  4. Download BundledK3dVersion from GitHub releases
func (p *Provider) resolveK3dBin(ctx context.Context, out io.Writer) (string, error) {
	if p.deps.K3dBin != "" {
		return p.deps.K3dBin, nil
	}
	return resolveK3dBin(ctx, out)
}

// resolveK3dBin is the package-level resolution function, split out so
// it can be called without a Provider receiver in tests.
func resolveK3dBin(ctx context.Context, out io.Writer) (string, error) {
	// 1. PATH lookup.
	if path, err := exec.LookPath("k3d"); err == nil {
		return path, nil
	}

	// 2. Cached download.
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("k3d: locate cache dir: %w", err)
	}
	binDir := filepath.Join(cacheDir, "clusterbox", "k3d", BundledK3dVersion)
	cached := filepath.Join(binDir, "k3d")
	if _, err := os.Stat(cached); err == nil {
		return cached, nil
	}

	// 3. Download.
	fmt.Fprintf(out, "k3d not found in PATH; downloading %s to %s...\n", BundledK3dVersion, cached)
	if err := downloadK3d(ctx, cached, BundledK3dVersion); err != nil {
		return "", fmt.Errorf("k3d auto-download failed — install manually from https://k3d.io: %w", err)
	}
	fmt.Fprintln(out, "k3d downloaded successfully.")
	return cached, nil
}

// downloadK3d fetches the k3d binary for the current OS/arch from
// GitHub releases and writes it to dest with mode 0755.
func downloadK3d(ctx context.Context, dest, version string) error {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	url := fmt.Sprintf(
		"https://github.com/k3d-io/k3d/releases/download/%s/k3d-%s-%s",
		version, goos, goarch,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dest), err)
	}

	// Atomic write: temp file → rename.
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write k3d binary: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// ---- helpers ---------------------------------------------------------------

func (p *Provider) out() io.Writer {
	if p.deps.Out != nil {
		return p.deps.Out
	}
	return os.Stderr
}

func (p *Provider) runner() Runner {
	if p.deps.Runner != nil {
		return p.deps.Runner
	}
	return execRunner{}
}

func (p *Provider) kubeconfigPath(clusterName string) (string, error) {
	if p.deps.KubeconfigPath != "" {
		return p.deps.KubeconfigPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("k3d: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".kube", clusterName+".yaml"), nil
}

// createCluster runs k3d cluster create, treating "already exists" as
// success so Provision is idempotent.
func (p *Provider) createCluster(ctx context.Context, run Runner, bin, name string) error {
	args := []string{"cluster", "create", name}

	agents := 0
	if p.deps.Nodes > 1 {
		agents = p.deps.Nodes - 1
	}
	if agents > 0 {
		args = append(args, "--agents", fmt.Sprintf("%d", agents))
	}
	if p.deps.K3sVersion != "" {
		args = append(args, "--image", "rancher/k3s:"+p.deps.K3sVersion)
	}
	// Don't merge into ~/.kube/config — clusterbox writes its own file.
	args = append(args, "--kubeconfig-update-default=false")

	output, err := run.Run(ctx, bin, args...)
	if err != nil {
		if bytes.Contains(bytes.ToLower(output), []byte("already exists")) {
			return nil
		}
		return fmt.Errorf("%w\n%s", err, output)
	}
	return nil
}

// exportKubeconfig runs k3d kubeconfig get and writes the result to
// path with mode 0600, creating parent directories as needed.
func (p *Provider) exportKubeconfig(ctx context.Context, run Runner, bin, name, path string) error {
	output, err := run.Run(ctx, bin, "kubeconfig", "get", name)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, output)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, output, 0o600); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}
	return nil
}

// nodeRows builds the registry Node slice for the cluster.
func (p *Provider) nodeRows(clusterName string) []registry.Node {
	total := p.deps.Nodes
	if total < 1 {
		total = 1
	}
	now := time.Now().UTC()
	nodes := make([]registry.Node, total)
	nodes[0] = registry.Node{
		ClusterName: clusterName,
		Hostname:    "k3d-" + clusterName + "-server-0",
		Role:        "control-plane",
		JoinedAt:    now,
	}
	for i := 1; i < total; i++ {
		nodes[i] = registry.Node{
			ClusterName: clusterName,
			Hostname:    fmt.Sprintf("k3d-%s-agent-%d", clusterName, i-1),
			Role:        "worker",
			JoinedAt:    now,
		}
	}
	return nodes
}

// k3dCluster is the minimal JSON shape returned by `k3d cluster list -o json`.
type k3dCluster struct {
	Name string `json:"name"`
}

// execRunner is the production Runner implementation.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}
