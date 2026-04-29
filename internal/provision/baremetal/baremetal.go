package baremetal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/foundryfabric/clusterbox/internal/agentbundle"
	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/provision/nodeinstall"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/secrets"
)

// Deps groups the injectable dependencies the baremetal provider needs.
// Tests substitute fields; nil fields fall back to production defaults.
type Deps struct {
	// Host is the SSH target (host[:port]). Required.
	Host string
	// User is the SSH login user. Required. The remote process is
	// escalated via sudo, which must be passwordless for User.
	User string
	// SSHKeyPath is the on-disk path to the private key used to dial
	// Host. Required.
	SSHKeyPath string

	// ConfigPath, when non-empty, is the YAML config the provider
	// loads via config.Load. Empty means "use DefaultSpec".
	ConfigPath string

	// KubeconfigPath is the destination path the rewritten kubeconfig
	// is written to. When empty the provider derives
	// $HOME/.kube/<clusterName>.yaml.
	KubeconfigPath string

	// AgentBundleForArch returns the embedded clusterboxnode binary
	// bytes for the given linux arch. Defaults to agentbundle.ForArch.
	AgentBundleForArch func(arch string) ([]byte, error)

	// AgentVersion is the version string the cmd layer can record in
	// the registry once the T10 schema lands. The baremetal provider
	// does not currently persist it (the registry.Cluster/Node structs
	// have no agent_version column yet); the field is wired through
	// Deps so cmd-side wiring can pass it without a follow-up Deps
	// change.
	AgentVersion string

	// SecretsResolver, when non-nil, is consulted by
	// ResolveSecretsForSpec to populate envOverlay for the install
	// command. Nil disables secret resolution.
	SecretsResolver secrets.Resolver

	// SecretsEnv / SecretsApp / SecretsRegion are the lookup keys
	// passed through to the SecretsResolver. They are otherwise unused
	// by the provider.
	SecretsApp    string
	SecretsEnv    string
	SecretsRegion string

	// Dial constructs a Transport for Host. Defaults to baremetal.Dial.
	Dial func(ctx context.Context, cfg DialConfig) (Transport, error)

	// Out is the destination for human-readable progress lines. When
	// nil the provider writes to os.Stderr.
	Out io.Writer

	// OpenRegistry opens the local registry. Defaults to
	// registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)

	// Now returns the wall-clock time used in registry rows. Defaults
	// to time.Now. Tests override for determinism.
	Now func() time.Time
}

// Provider is the bare-metal implementation of provision.Provider. A
// zero-value Provider is NOT usable: Host, User, and SSHKeyPath must be
// supplied via Deps because there is no sensible default for any of
// them.
type Provider struct {
	deps Deps
}

// New constructs a baremetal Provider with the given dependencies.
func New(deps Deps) *Provider {
	return &Provider{deps: deps}
}

// Name returns the canonical provider identifier ("baremetal").
func (p *Provider) Name() string { return Name }

// Provision installs clusterboxnode against a single bare-metal host
// over SSH. The flow:
//
//  1. Dial the target and verify passwordless sudo.
//  2. Probe the host arch via `uname -m`.
//  3. Resolve the embedded clusterboxnode binary for that arch.
//  4. Build the install Spec (DefaultSpec or config.Load).
//  5. Resolve any secrets the Spec references and pass them via
//     envOverlay on the install Run call.
//  6. Upload binary + YAML config to /tmp under content-hashed paths.
//  7. Run the install; parse the JSON success/error envelope.
//  8. Rewrite the returned kubeconfig server URL to <host>:6443 and
//     write it to KubeconfigPath (mode 0600). Overwrites with a
//     warning on stderr.
//  9. Best-effort: record the cluster + node in the local registry.
//
// 10. Best-effort: clean up uploaded files.
//
// Re-running against the same host converges: the install walker is
// idempotent (sections report "already installed"), and the kubeconfig
// + registry rows are refreshed.
func (p *Provider) Provision(ctx context.Context, cfg provision.ClusterConfig) (provision.ProvisionResult, error) {
	if err := p.validateRequired(); err != nil {
		return provision.ProvisionResult{}, err
	}
	out := p.out()

	host := canonicalHost(p.deps.Host)
	hostNoPort := stripPort(p.deps.Host)

	kubeconfigPath, err := p.kubeconfigPath(cfg.ClusterName)
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	// Step 1: dial. Passwordless-sudo verification happens inside the
	// Transport on first Run; ErrSudoNotPasswordless is wrapped by the
	// caller below.
	_, _ = fmt.Fprintf(out, "[1/8] Dialing %s as %s...\n", host, p.deps.User)
	dial := p.deps.Dial
	if dial == nil {
		dial = Dial
	}
	tr, err := dial(ctx, DialConfig{
		Host:       p.deps.Host,
		User:       p.deps.User,
		SSHKeyPath: p.deps.SSHKeyPath,
	})
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: dial: %w", err)
	}
	defer func() { _ = tr.Close() }()

	// Step 2: probe arch. The first Run also exercises passwordless
	// sudo via ssh.ensureSudoNoPassword.
	_, _ = fmt.Fprintln(out, "[2/8] Probing host architecture (uname -m)...")
	stdout, stderr, exit, err := tr.Run(ctx, "uname -m", nil)
	if err != nil {
		if errors.Is(err, ErrSudoNotPasswordless) {
			return provision.ProvisionResult{}, fmt.Errorf("baremetal: passwordless sudo unavailable for user %q: %w", p.deps.User, err)
		}
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: uname -m: %w", err)
	}
	if exit != 0 {
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: uname -m exit=%d stderr=%q", exit, string(stderr))
	}
	arch, err := nodeinstall.MapArch(string(stdout))
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	// Step 3: load embedded agent bytes.
	_, _ = fmt.Fprintf(out, "[3/8] Loading clusterboxnode bytes for linux/%s...\n", arch)
	loader := p.deps.AgentBundleForArch
	if loader == nil {
		loader = agentbundle.ForArch
	}
	agentBytes, err := loader(arch)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: agent bundle: %w", err)
	}

	// Step 4: build Spec.
	_, _ = fmt.Fprintln(out, "[4/8] Building install Spec...")
	spec, err := p.loadOrDefaultSpec(cfg.ClusterName)
	if err != nil {
		return provision.ProvisionResult{}, err
	}
	specYAML, err := yaml.Marshal(spec)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: marshal spec: %w", err)
	}

	// Step 5: resolve secrets (no logging of values).
	envOverlay, err := ResolveSecretsForSpec(
		ctx, spec, p.deps.SecretsResolver,
		p.deps.SecretsApp, p.deps.SecretsEnv, Name, p.deps.SecretsRegion,
	)
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	// Step 6: upload binary + config to /tmp under content-hashed paths.
	binPath := "/tmp/clusterboxnode-" + nodeinstall.ShortSHA(agentBytes)
	cfgPath := "/tmp/clusterbox-node-" + nodeinstall.ShortSHA(specYAML) + ".yaml"

	_, _ = fmt.Fprintf(out, "[5/8] Uploading clusterboxnode -> %s\n", binPath)
	if err := tr.Upload(ctx, binPath, agentBytes); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: upload binary: %w", err)
	}
	_, _ = fmt.Fprintf(out, "[6/8] Uploading config -> %s\n", cfgPath)
	if err := tr.Upload(ctx, cfgPath, specYAML); err != nil {
		_ = tr.Remove(ctx, binPath)
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: upload config: %w", err)
	}
	defer func() {
		// Best-effort cleanup. Errors are logged but never fail the
		// provision: the next run hashes to the same paths and is
		// idempotent.
		if err := tr.Remove(ctx, binPath); err != nil {
			_, _ = fmt.Fprintf(out, "warning: cleanup %s: %v\n", binPath, err)
		}
		if err := tr.Remove(ctx, cfgPath); err != nil {
			_, _ = fmt.Fprintf(out, "warning: cleanup %s: %v\n", cfgPath, err)
		}
	}()

	// Step 7: run install. Use a /bin/sh -c wrapper so the binary path
	// is invoked with sudo (the Transport already wraps in sudo itself).
	installCmd := fmt.Sprintf("%s install --config %s",
		shellQuote(binPath), shellQuote(cfgPath))
	_, _ = fmt.Fprintln(out, "[7/8] Running install on remote host...")
	stdout, stderr, exit, err = tr.Run(ctx, installCmd, envOverlay)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: install: %w (stderr=%q)", err, string(stderr))
	}
	parsed, parseErr := nodeinstall.ParseInstallOutput(stdout)
	if parseErr != nil {
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: parse install output: %w (exit=%d, stderr=%q)", parseErr, exit, string(stderr))
	}
	if exit != 0 || parsed.IsError() {
		return provision.ProvisionResult{}, parsed.AsError(exit, stderr)
	}
	if parsed.KubeconfigYAML == "" {
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: install output missing kubeconfig_yaml")
	}

	// Step 8: rewrite kubeconfig and persist.
	_, _ = fmt.Fprintf(out, "[8/8] Writing kubeconfig to %s\n", kubeconfigPath)
	rewritten, err := nodeinstall.RewriteKubeconfigServer(parsed.KubeconfigYAML, hostNoPort)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: rewrite kubeconfig: %w", err)
	}
	if err := nodeinstall.WriteKubeconfig(kubeconfigPath, rewritten, out); err != nil {
		return provision.ProvisionResult{}, err
	}

	// Best-effort: record cluster + node in registry. Failures must
	// not fail the provision.
	now := p.now()
	hostname := cfg.ClusterName
	p.recordRegistry(ctx, registry.Cluster{
		Name:           cfg.ClusterName,
		Provider:       Name,
		Region:         cfg.Location,
		CreatedAt:      now,
		KubeconfigPath: kubeconfigPath,
	}, registry.Node{
		ClusterName: cfg.ClusterName,
		Hostname:    hostname,
		Role:        "control-plane",
		JoinedAt:    now,
	})

	return provision.ProvisionResult{
		KubeconfigPath: kubeconfigPath,
		Nodes: []registry.Node{
			{
				ClusterName: cfg.ClusterName,
				Hostname:    hostname,
				Role:        "control-plane",
				JoinedAt:    now,
			},
		},
	}, nil
}

// Destroy is intentionally minimal for a single-node baremetal target.
// We have no provider-side resources to delete; the registry tombstone
// happens in cmd/destroy.go after we return. Reconcile-style sweeps do
// not apply because we never created cloud objects.
func (p *Provider) Destroy(ctx context.Context, _ registry.Cluster) error {
	// Future: optionally SSH to the host and run `clusterboxnode
	// uninstall`. For T7b we're scope-limited; document and return.
	return nil
}

// Reconcile refreshes the local registry's last_inspected_at for the
// cluster. For a baremetal target there is no provider-side inventory
// to walk; the registry is the source of truth and we only update the
// observation timestamp.
func (p *Provider) Reconcile(ctx context.Context, _ string) (provision.ReconcileSummary, error) {
	return provision.ReconcileSummary{}, nil
}

// validateRequired ensures Host, User, and SSHKeyPath are populated.
func (p *Provider) validateRequired() error {
	var missing []string
	if p.deps.Host == "" {
		missing = append(missing, "Host")
	}
	if p.deps.User == "" {
		missing = append(missing, "User")
	}
	if p.deps.SSHKeyPath == "" {
		missing = append(missing, "SSHKeyPath")
	}
	if len(missing) > 0 {
		return fmt.Errorf("baremetal: required Deps fields unset: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (p *Provider) out() io.Writer {
	if p.deps.Out != nil {
		return p.deps.Out
	}
	return os.Stderr
}

func (p *Provider) now() time.Time {
	if p.deps.Now != nil {
		return p.deps.Now()
	}
	return time.Now().UTC()
}

func (p *Provider) kubeconfigPath(clusterName string) (string, error) {
	if p.deps.KubeconfigPath != "" {
		return p.deps.KubeconfigPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("baremetal: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".kube", clusterName+".yaml"), nil
}

func (p *Provider) loadOrDefaultSpec(clusterName string) (*config.Spec, error) {
	if p.deps.ConfigPath == "" {
		return DefaultSpec(clusterName, "control-plane"), nil
	}
	return config.Load(p.deps.ConfigPath)
}

// recordRegistry writes the cluster + node row best-effort. Failures
// are logged to Out but never returned: a successful install with a
// failed registry write is recoverable on the next run, but a failed
// install with a successful registry write is not.
func (p *Provider) recordRegistry(ctx context.Context, cluster registry.Cluster, node registry.Node) {
	open := p.deps.OpenRegistry
	if open == nil {
		open = registry.NewRegistry
	}
	reg, err := open(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(p.out(), "warning: registry write failed: %v\n", err)
		return
	}
	defer func() { _ = reg.Close() }()

	if err := reg.UpsertCluster(ctx, cluster); err != nil {
		_, _ = fmt.Fprintf(p.out(), "warning: registry write failed: %v\n", err)
		return
	}
	if err := reg.UpsertNode(ctx, node); err != nil {
		_, _ = fmt.Fprintf(p.out(), "warning: registry write failed: %v\n", err)
		return
	}
}

// canonicalHost returns host[:port], appending :22 when no port is
// present, mirroring Dial's behaviour for log messages.
func canonicalHost(h string) string {
	if _, _, err := net.SplitHostPort(h); err == nil {
		return h
	}
	return net.JoinHostPort(h, "22")
}

// stripPort returns h with any trailing ":port" removed. Used when
// composing the kubeconfig server URL (which always uses :6443).
func stripPort(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

// AddNode is not supported by the baremetal provider. Baremetal hosts must be
// added manually; clusterbox does not know how to acquire new physical machines.
func (p *Provider) AddNode(_ context.Context, _ string) (string, error) {
	return "", provision.ErrAddNodeNotSupported
}

// RemoveNode is not supported by the baremetal provider. Physical machines
// must be decommissioned out-of-band.
func (p *Provider) RemoveNode(_ context.Context, _, _ string) error {
	return provision.ErrRemoveNodeNotSupported
}

// Compile-time check: *Provider satisfies provision.Provider.
var _ provision.Provider = (*Provider)(nil)
