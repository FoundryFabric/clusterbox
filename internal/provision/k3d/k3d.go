// Package k3d implements a local-cluster provision.Provider that wraps
// the k3d CLI (k3s in Docker). It works on any platform where Docker
// and k3d are available — Mac, Linux, and Windows — without requiring
// a cloud account or SSH access.
//
// The provider is intentionally thin: it shells out to `k3d` for every
// operation rather than importing the k3d libraries directly. This
// avoids a heavy dependency tree and keeps the integration surface
// identical to what an operator would run by hand.
package k3d

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/registry"
)

// Name is the canonical --provider value for this provider.
const Name = "k3d"

// Runner is the minimal interface the k3d provider needs to execute
// external commands. Production code uses execRunner; tests substitute
// a stub that captures invocations without spawning real processes.
type Runner interface {
	// Run executes name with args in the caller's environment and
	// returns its combined stdout+stderr output and exit status.
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
//  1. Verify k3d is installed.
//  2. k3d cluster create <name> [--servers N] [--agents N] [--image …]
//  3. k3d kubeconfig get <name> → write to KubeconfigPath (mode 0600).
//  4. Return a ProvisionResult with one Node row per server.
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

	// Step 1: verify k3d is present.
	fmt.Fprintln(out, "[1/3] Checking k3d is installed...")
	if _, err := run.Run(ctx, "k3d", "version"); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("k3d not found — install it from https://k3d.io: %w", err)
	}

	// Step 2: create the cluster (idempotent).
	fmt.Fprintf(out, "[2/3] Creating k3d cluster %q...\n", name)
	if err := p.createCluster(ctx, run, name); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("k3d cluster create: %w", err)
	}

	// Step 3: export kubeconfig.
	fmt.Fprintf(out, "[3/3] Writing kubeconfig to %s...\n", kubeconfigPath)
	if err := p.exportKubeconfig(ctx, run, name, kubeconfigPath); err != nil {
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
// (or with a "not found" message that the provider ignores). Removing
// a kubeconfig that does not exist is also a no-op.
func (p *Provider) Destroy(ctx context.Context, cluster registry.Cluster) error {
	out := p.out()
	run := p.runner()
	name := cluster.Name

	fmt.Fprintf(out, "[1/2] Deleting k3d cluster %q...\n", name)
	output, err := run.Run(ctx, "k3d", "cluster", "delete", name)
	if err != nil {
		// k3d prints "No cluster found" when the cluster doesn't exist —
		// treat that as success so destroy is safe to re-run.
		if strings.Contains(strings.ToLower(string(output)), "no cluster found") {
			fmt.Fprintf(out, "Cluster %q not found in k3d; nothing to delete.\n", name)
		} else {
			return fmt.Errorf("k3d cluster delete %s: %w", name, err)
		}
	}

	fmt.Fprintln(out, "[2/2] Removing kubeconfig...")
	kubeconfigPath := cluster.KubeconfigPath
	if kubeconfigPath == "" {
		// Best-effort: derive from $HOME when the registry row is stale.
		home, err := os.UserHomeDir()
		if err == nil {
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
// never mutates provider-side state (k3d cluster list is read-only).
//
// A cluster that exists in k3d but is not tracked in the local
// registry appears in ReconcileSummary.Unmanaged. The reverse (tracked
// but absent from k3d) is MarkedDestroyed.
func (p *Provider) Reconcile(ctx context.Context, clusterName string) (provision.ReconcileSummary, error) {
	run := p.runner()

	output, err := run.Run(ctx, "k3d", "cluster", "list", "-o", "json")
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
func (p *Provider) createCluster(ctx context.Context, run Runner, name string) error {
	args := []string{"cluster", "create", name}

	servers := 1
	agents := 0
	if p.deps.Nodes > 1 {
		agents = p.deps.Nodes - 1
	}
	if servers != 1 {
		args = append(args, "--servers", fmt.Sprintf("%d", servers))
	}
	if agents > 0 {
		args = append(args, "--agents", fmt.Sprintf("%d", agents))
	}
	if p.deps.K3sVersion != "" {
		args = append(args, "--image", "rancher/k3s:"+p.deps.K3sVersion)
	}
	// Write kubeconfig to a well-known path rather than merging into
	// ~/.kube/config so clusterbox clusters don't pollute the default
	// kubeconfig.
	args = append(args, "--kubeconfig-update-default=false")

	output, err := run.Run(ctx, "k3d", args...)
	if err != nil {
		// k3d exits non-zero when the cluster already exists; the
		// error message contains "already exists". Treat as success.
		if bytes.Contains(bytes.ToLower(output), []byte("already exists")) {
			return nil
		}
		return fmt.Errorf("%w\n%s", err, output)
	}
	return nil
}

// exportKubeconfig runs k3d kubeconfig get and writes the result to
// path with mode 0600, creating parent directories as needed.
func (p *Provider) exportKubeconfig(ctx context.Context, run Runner, name, path string) error {
	output, err := run.Run(ctx, "k3d", "kubeconfig", "get", name)
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

// nodeRows builds the registry Node slice for the cluster. k3d does
// not surface per-node metadata at provision time without an extra
// kubectl/docker call, so the provider produces one stub row per
// requested node using a synthetic hostname.
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

// execRunner is the production Runner implementation. It shells out via
// os/exec and returns combined stdout+stderr along with the error.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}
