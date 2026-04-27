package baremetal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	fmt.Fprintf(out, "[1/8] Dialing %s as %s...\n", host, p.deps.User)
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
	fmt.Fprintln(out, "[2/8] Probing host architecture (uname -m)...")
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
	arch, err := mapArch(string(stdout))
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	// Step 3: load embedded agent bytes.
	fmt.Fprintf(out, "[3/8] Loading clusterboxnode bytes for linux/%s...\n", arch)
	loader := p.deps.AgentBundleForArch
	if loader == nil {
		loader = agentbundle.ForArch
	}
	agentBytes, err := loader(arch)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: agent bundle: %w", err)
	}

	// Step 4: build Spec.
	fmt.Fprintln(out, "[4/8] Building install Spec...")
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
	binSha := shortSHA(agentBytes)
	cfgSha := shortSHA(specYAML)
	binPath := "/tmp/clusterboxnode-" + binSha
	cfgPath := "/tmp/clusterbox-node-" + cfgSha + ".yaml"

	fmt.Fprintf(out, "[5/8] Uploading clusterboxnode -> %s\n", binPath)
	if err := tr.Upload(ctx, binPath, agentBytes); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: upload binary: %w", err)
	}
	fmt.Fprintf(out, "[6/8] Uploading config -> %s\n", cfgPath)
	if err := tr.Upload(ctx, cfgPath, specYAML); err != nil {
		_ = tr.Remove(ctx, binPath)
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: upload config: %w", err)
	}
	defer func() {
		// Best-effort cleanup. Errors are logged but never fail the
		// provision: the next run hashes to the same paths and is
		// idempotent.
		if err := tr.Remove(ctx, binPath); err != nil {
			fmt.Fprintf(out, "warning: cleanup %s: %v\n", binPath, err)
		}
		if err := tr.Remove(ctx, cfgPath); err != nil {
			fmt.Fprintf(out, "warning: cleanup %s: %v\n", cfgPath, err)
		}
	}()

	// Step 7: run install. Use a /bin/sh -c wrapper so the binary path
	// is invoked with sudo (the Transport already wraps in sudo itself).
	installCmd := fmt.Sprintf("%s install --config %s",
		shellQuote(binPath), shellQuote(cfgPath))
	fmt.Fprintln(out, "[7/8] Running install on remote host...")
	stdout, stderr, exit, err = tr.Run(ctx, installCmd, envOverlay)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: install: %w (stderr=%q)", err, string(stderr))
	}
	parsed, parseErr := parseInstallOutput(stdout)
	if parseErr != nil {
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: parse install output: %w (exit=%d, stderr=%q)", parseErr, exit, string(stderr))
	}
	if exit != 0 || parsed.IsError() {
		return provision.ProvisionResult{}, parsed.AsError(exit, stderr)
	}

	// Step 8: rewrite kubeconfig and persist.
	fmt.Fprintf(out, "[8/8] Writing kubeconfig to %s\n", kubeconfigPath)
	rewritten, err := rewriteKubeconfigServer(parsed.KubeconfigYAML, hostNoPort)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("baremetal: rewrite kubeconfig: %w", err)
	}
	if err := writeKubeconfig(kubeconfigPath, rewritten, out); err != nil {
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
		fmt.Fprintf(p.out(), "warning: registry write failed: %v\n", err)
		return
	}
	defer func() { _ = reg.Close() }()

	if err := reg.UpsertCluster(ctx, cluster); err != nil {
		fmt.Fprintf(p.out(), "warning: registry write failed: %v\n", err)
		return
	}
	if err := reg.UpsertNode(ctx, node); err != nil {
		fmt.Fprintf(p.out(), "warning: registry write failed: %v\n", err)
		return
	}
}

// installEnvelope is the parsed shape of clusterboxnode's stdout. It
// carries either a success doc (sections map) or an error doc (top-
// level error/section keys).
type installEnvelope struct {
	// Success-shape fields.
	Sections map[string]map[string]interface{} `json:"sections,omitempty"`
	// Error-shape fields.
	ErrorMsg      string                            `json:"error,omitempty"`
	ErrorSection  string                            `json:"section,omitempty"`
	SectionsSoFar map[string]map[string]interface{} `json:"sections_so_far,omitempty"`

	// Derived (set by parseInstallOutput).
	K3sVersion     string `json:"-"`
	KubeconfigYAML string `json:"-"`
}

// IsError reports whether the envelope is the error-shape document.
func (e *installEnvelope) IsError() bool { return e.ErrorMsg != "" }

// AsError builds a descriptive error from an error-shape envelope.
func (e *installEnvelope) AsError(exit int, stderr []byte) error {
	return fmt.Errorf("baremetal: install failed in section %s: %s (exit=%d, stderr=%q)",
		e.ErrorSection, e.ErrorMsg, exit, string(stderr))
}

// parseInstallOutput decodes the JSON envelope clusterboxnode prints.
// On success it lifts k3s_version and kubeconfig_yaml out of the k3s
// section so the caller can use them without re-walking the tree.
//
// stdout may contain non-JSON preamble lines (e.g. progress messages
// from sub-tools); we look for the first '{' and decode from there.
func parseInstallOutput(stdout []byte) (*installEnvelope, error) {
	idx := indexOf(stdout, '{')
	if idx < 0 {
		return nil, fmt.Errorf("no JSON document in install output (got %d bytes)", len(stdout))
	}
	var env installEnvelope
	if err := json.Unmarshal(stdout[idx:], &env); err != nil {
		return nil, fmt.Errorf("decode install JSON: %w", err)
	}
	if env.ErrorMsg != "" {
		return &env, nil
	}
	if env.Sections == nil {
		return nil, fmt.Errorf("install output missing sections key")
	}
	k3s, ok := env.Sections["k3s"]
	if !ok {
		return nil, fmt.Errorf("install output missing k3s section")
	}
	if v, _ := k3s["k3s_version"].(string); v != "" {
		env.K3sVersion = v
	}
	if v, _ := k3s["kubeconfig_yaml"].(string); v != "" {
		env.KubeconfigYAML = v
	}
	if env.KubeconfigYAML == "" {
		return nil, fmt.Errorf("install output missing kubeconfig_yaml in k3s section")
	}
	return &env, nil
}

// rewriteKubeconfigServer replaces every cluster.server URL of form
// https://127.0.0.1:6443 (or 0.0.0.0/localhost) with https://host:6443
// so the kubeconfig is usable from outside the target.
//
// We parse and re-marshal as a generic node tree to avoid pulling in
// client-go just to rewrite a single field.
func rewriteKubeconfigServer(in, host string) (string, error) {
	if in == "" {
		return "", errors.New("empty kubeconfig")
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(in), &n); err != nil {
		return "", fmt.Errorf("parse kubeconfig: %w", err)
	}
	rewriteServerURLs(&n, host)
	out, err := yaml.Marshal(&n)
	if err != nil {
		return "", fmt.Errorf("marshal kubeconfig: %w", err)
	}
	return string(out), nil
}

// rewriteServerURLs descends n and overwrites every "server" string
// value whose URL points at a loopback / unspecified address with the
// public host.
func rewriteServerURLs(n *yaml.Node, host string) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			v := n.Content[i+1]
			if k.Value == "server" && v.Kind == yaml.ScalarNode {
				if isLoopbackServerURL(v.Value) {
					v.Value = "https://" + net.JoinHostPort(host, "6443")
				}
				continue
			}
			rewriteServerURLs(v, host)
		}
		return
	}
	for _, c := range n.Content {
		rewriteServerURLs(c, host)
	}
}

// isLoopbackServerURL reports whether v looks like a kubeconfig server
// URL pointing at the local host.
func isLoopbackServerURL(v string) bool {
	for _, prefix := range []string{
		"https://127.0.0.1:6443",
		"https://0.0.0.0:6443",
		"https://localhost:6443",
	} {
		if strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return false
}

// writeKubeconfig writes data to path with mode 0600. If path already
// exists we overwrite and emit a warning to out so the operator knows a
// previous kubeconfig was replaced.
func writeKubeconfig(path, data string, out io.Writer) error {
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(out, "warning: overwriting existing kubeconfig at %s\n", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("baremetal: mkdir kubeconfig dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		return fmt.Errorf("baremetal: write kubeconfig: %w", err)
	}
	return nil
}

// mapArch maps `uname -m` output to the linux arch token agentbundle
// understands.
func mapArch(unameOut string) (string, error) {
	v := strings.TrimSpace(unameOut)
	switch v {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("baremetal: unsupported arch %q (want x86_64/amd64 or aarch64/arm64)", v)
	}
}

// shortSHA returns the first 12 hex chars of sha256(b). 12 chars is
// plenty to disambiguate /tmp paths and keeps the path readable.
func shortSHA(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:12]
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

// indexOf returns the first index of c in b, or -1 if absent.
func indexOf(b []byte, c byte) int {
	for i := 0; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// Compile-time check: *Provider satisfies provision.Provider.
var _ provision.Provider = (*Provider)(nil)
