// Package k3s implements the k3s install/uninstall section of clusterboxnode.
//
// The package exposes two top-level entry points, Apply and Remove, that
// return a structured Result the section walker maps onto its
// install.SectionResult. All process and file system access flows through
// the Runner and FS interfaces so the implementation is fully unit-testable.
//
// The install path:
//
//   - Skip if k3s is already present on disk or active under systemd.
//   - Otherwise download the k3s binary directly from GitHub releases (with
//     exponential-backoff retry), write the systemd service + env files, and
//     start the unit via systemctl.
//   - For server roles: poll for /etc/rancher/k3s/k3s.yaml and the node-token.
//   - For agent role: two phases — (1) poll for AgentKubeletKubeconfig (TLS
//     bootstrap done, kubelet cert received), then (2) poll "kubectl get node"
//     until the node object appears in the API server.
package k3s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/foundryfabric/clusterbox/internal/node/config"
)

// Default filesystem locations used by the k3s installer.
//
// Exposed as package-level vars (not consts) so tests can override them when
// pointing at a fake filesystem rooted in a temp dir.
var (
	K3sBinary        = "/usr/local/bin/k3s"
	KubeconfigPath   = "/etc/rancher/k3s/k3s.yaml"
	NodeTokenPath    = "/var/lib/rancher/k3s/server/node-token"
	DefaultServerURL = "https://127.0.0.1:6443"

	// AgentKubeletKubeconfig is written by k3s-agent only after it has connected
	// to the control plane, received its node certificate, and registered.
	// It is the correct signal that an agent has joined the cluster.
	AgentKubeletKubeconfig = "/var/lib/rancher/k3s/agent/kubelet.kubeconfig"

	ServerServicePath = "/etc/systemd/system/k3s.service"
	AgentServicePath  = "/etc/systemd/system/k3s-agent.service"
	ServerEnvPath     = "/etc/systemd/system/k3s.service.env"
	AgentEnvPath      = "/etc/systemd/system/k3s-agent.service.env"

	// kubeconfigPollInterval and kubeconfigPollTimeout bound the wait for
	// /etc/rancher/k3s/k3s.yaml after the service starts.
	kubeconfigPollInterval = 500 * time.Millisecond
	kubeconfigPollTimeout  = 60 * time.Second

	// agentPollInterval and agentPollTimeout bound Phase 1 of waitForAgent:
	// waiting for AgentKubeletKubeconfig (TLS bootstrap complete, kubelet not
	// yet started).
	agentPollInterval = 5 * time.Second
	agentPollTimeout  = 3 * time.Minute

	// nodeRegPollTimeout bounds Phase 2 of waitForAgent: waiting for the node
	// object to appear in the API server after kubelet starts and registers.
	nodeRegPollTimeout = 2 * time.Minute
)

// Runner abstracts process execution so unit tests can inject a fake.
//
// Implementations must respect ctx cancellation and surface stderr in any
// returned error so failed commands remain debuggable.
type Runner interface {
	// Run executes name with args and returns combined stdout. Errors must
	// wrap any non-zero exit status with stderr context.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// FS abstracts filesystem access so tests do not require root-owned paths.
type FS interface {
	Stat(path string) (fs.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm os.FileMode) error
	MkdirAll(path string, perm os.FileMode) error
}

// Result is the structured payload returned by Apply / Remove. The section
// walker translates this onto its install.SectionResult shape; keeping the
// type here avoids an import cycle between the walker and this package.
type Result struct {
	// Applied mirrors install.SectionResult.Applied.
	Applied bool
	// Reason is a short human-readable explanation when Applied=false or
	// when work was skipped (e.g. "already installed").
	Reason string
	// Extra is flattened into the per-section JSON payload by the walker.
	Extra map[string]any
}

// Section bundles the dependencies (Runner, FS, clock, polling knobs) used
// by Apply and Remove.
//
// All fields have working zero-value defaults so production callers can
// construct a zero Section. Tests override Runner, FS, and Downloader to drive
// deterministic scenarios without real network or filesystem access.
type Section struct {
	Runner Runner
	FS     FS

	// Out receives human-readable progress lines during installation.
	// Defaults to io.Discard so test output is not polluted.
	// Production callers set this to the command's stdout so progress
	// is visible through the SSH stream.
	Out io.Writer

	// Now is injected so tests can deterministically observe the polling
	// timeout. Defaults to time.Now.
	Now func() time.Time

	// PollInterval and PollTimeout default to the package-level constants.
	PollInterval time.Duration
	PollTimeout  time.Duration

	// Arch is the target node architecture ("amd64", "arm64").
	// Defaults to runtime.GOARCH. Tests inject a fixed value.
	Arch string

	// Downloader downloads url to dest with perm. Defaults to httpDownloadWithRetry.
	// Tests inject a no-op that writes fake bytes to the fake FS.
	Downloader func(ctx context.Context, url, dest string, perm os.FileMode) error
}

func (s *Section) arch() string {
	if s.Arch != "" {
		return s.Arch
	}
	return runtime.GOARCH
}

func (s *Section) downloader() func(ctx context.Context, url, dest string, perm os.FileMode) error {
	if s.Downloader != nil {
		return s.Downloader
	}
	return httpDownloadWithRetry
}

// Apply installs k3s if it is not already present and returns a structured
// Result describing what happened.
//
// Behaviour matrix:
//
//   - spec.K3s nil or Enabled=false: Applied=false, Reason="disabled".
//   - role=agent: downloads and installs k3s, writes service files, starts
//     k3s-agent, then polls until the unit is active.
//   - role=server / server-init and k3s already present: Applied=true with
//     Reason="already installed" — kubeconfig and node-token are still read
//     so the parent control plane can pick them up.
//   - otherwise: download, install, start k3s server, poll for kubeconfig+token.
func (s *Section) Apply(ctx context.Context, spec *config.Spec) (Result, error) {
	k := specK3s(spec)
	if k == nil || !k.Enabled {
		return Result{Applied: false, Reason: "disabled"}, nil
	}

	runner, fsys := s.runner(), s.fsys()

	already, err := s.alreadyInstalled(ctx, runner, fsys)
	if err != nil {
		return Result{}, err
	}
	if !already {
		if err := s.installK3s(ctx, runner, fsys, k); err != nil {
			return Result{}, err
		}
	}

	if k.Role == "agent" {
		// Poll until k3s-agent connects to the control plane. installK3s starts
		// the service but returns before the agent has joined; polling here means
		// callers get a real join confirmation (or a clear failure with embedded
		// diagnostics) rather than a silent success that hides a misconfigured
		// token or unreachable server URL.
		if err := s.waitForAgent(ctx, runner, fsys); err != nil {
			s.collectAgentDiagnostics(runner, k.ServerURL)
			return Result{}, fmt.Errorf("k3s: %w", err)
		}
		res := Result{
			Applied: true,
			Extra: map[string]any{
				"role":        "worker",
				"k3s_version": k.Version,
			},
		}
		if already {
			res.Reason = "already installed"
		}
		return res, nil
	}

	kubeconfig, err := s.waitForFile(ctx, fsys, KubeconfigPath)
	if err != nil {
		return Result{}, fmt.Errorf("k3s: waiting for %s: %w", KubeconfigPath, err)
	}
	// node-token is written shortly after the kubeconfig but not always in
	// the same instant; keep polling on the same deadline budget.
	token, err := s.waitForFile(ctx, fsys, NodeTokenPath)
	if err != nil {
		return Result{}, fmt.Errorf("k3s: waiting for %s: %w", NodeTokenPath, err)
	}

	res := Result{
		Applied: true,
		Extra: map[string]any{
			"role":            mapRole(k.Role),
			"k3s_version":     k.Version,
			"kubeconfig_yaml": string(kubeconfig),
			"node_token":      trimTrailingNewline(string(token)),
			"server_url":      DefaultServerURL,
		},
	}
	if already {
		res.Reason = "already installed"
	}
	return res, nil
}

// Remove stops and removes k3s when it is present. Idempotent: returns
// removed=true whether or not k3s was installed.
func (s *Section) Remove(ctx context.Context, _ *config.Spec) (Result, error) {
	runner, fsys := s.runner(), s.fsys()

	present, err := s.alreadyInstalled(ctx, runner, fsys)
	if err != nil {
		return Result{}, err
	}
	if !present {
		return Result{
			Applied: false,
			Reason:  "k3s not installed",
			Extra:   map[string]any{"removed": true, "k3s_was_present": false},
		}, nil
	}

	for _, svc := range []string{"k3s.service", "k3s-agent.service"} {
		_, _ = runner.Run(ctx, "systemctl", "stop", svc)
		_, _ = runner.Run(ctx, "systemctl", "disable", svc)
	}
	for _, path := range []string{
		K3sBinary,
		"/usr/local/bin/kubectl",
		"/usr/local/bin/crictl",
		"/usr/local/bin/ctr",
		ServerServicePath, ServerEnvPath,
		AgentServicePath, AgentEnvPath,
	} {
		_, _ = runner.Run(ctx, "rm", "-f", path)
	}
	_, _ = runner.Run(ctx, "systemctl", "daemon-reload")

	return Result{
		Applied: true,
		Extra:   map[string]any{"removed": true, "k3s_was_present": true},
	}, nil
}

// alreadyInstalled returns true when either the k3s binary is on disk or the
// systemd unit reports active. The two checks together cover both freshly
// installed nodes and ones where the binary path was customised.
func (s *Section) alreadyInstalled(ctx context.Context, runner Runner, fsys FS) (bool, error) {
	if _, err := fsys.Stat(K3sBinary); err == nil {
		return true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("k3s: stat %s: %w", K3sBinary, err)
	}
	// systemctl is-active exits 0 when active, non-zero otherwise. Treat
	// any non-zero exit as "not active" rather than an error.
	if out, err := runner.Run(ctx, "systemctl", "is-active", "k3s"); err == nil {
		if strings.HasPrefix(strings.TrimSpace(string(out)), "active") {
			return true, nil
		}
	}
	return false, nil
}

// installK3s installs the k3s binary (from the embedded asset or by download),
// writes the systemd service and env files from spec, then enables and starts
// the unit. It does not wait for the service to become healthy — the caller
// polls via waitForAgent or waitForFile.
func (s *Section) installK3s(ctx context.Context, runner Runner, fsys FS, k *config.K3sSpec) error {
	embVersion := strings.TrimSpace(EmbeddedVersion)
	if embVersion != "" && k.Version == embVersion && len(EmbeddedBinary) > 0 {
		_, _ = fmt.Fprintf(s.out(), "k3s: installing embedded binary %s...\n", k.Version)
		if err := fsys.WriteFile(K3sBinary, EmbeddedBinary, 0o755); err != nil {
			return fmt.Errorf("k3s: write embedded binary: %w", err)
		}
	} else {
		arch := s.arch()
		url := k3sBinaryURL(k.Version, arch)
		_, _ = fmt.Fprintf(s.out(), "k3s: downloading %s (arch=%s)...\n", k.Version, arch)
		if err := s.downloader()(ctx, url, K3sBinary, 0o755); err != nil {
			return fmt.Errorf("k3s: download binary: %w", err)
		}
	}
	_, _ = fmt.Fprintln(s.out(), "k3s: binary installed")

	for _, link := range []string{"/usr/local/bin/kubectl", "/usr/local/bin/crictl", "/usr/local/bin/ctr"} {
		if _, err := runner.Run(ctx, "ln", "-sf", K3sBinary, link); err != nil {
			return fmt.Errorf("k3s: symlink %s: %w", link, err)
		}
	}

	var svcPath, envPath, svcName string
	var svcData, envData []byte
	switch k.Role {
	case "agent":
		svcPath, envPath, svcName = AgentServicePath, AgentEnvPath, "k3s-agent.service"
		svcData = agentServiceFile(k)
		envData = agentEnvFile(k)
	default:
		svcPath, envPath, svcName = ServerServicePath, ServerEnvPath, "k3s.service"
		svcData = serverServiceFile(k)
		envData = []byte("# k3s server environment\n")
	}
	if err := fsys.WriteFile(svcPath, svcData, 0o644); err != nil {
		return fmt.Errorf("k3s: write %s: %w", svcPath, err)
	}
	if err := fsys.WriteFile(envPath, envData, 0o600); err != nil {
		return fmt.Errorf("k3s: write %s: %w", envPath, err)
	}

	_, _ = fmt.Fprintf(s.out(), "k3s: starting %s...\n", svcName)
	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", svcName},
		{"start", svcName},
	} {
		if _, err := runner.Run(ctx, "systemctl", args...); err != nil {
			return fmt.Errorf("k3s: systemctl %s: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}

const serverServiceTmpl = `[Unit]
Description=Lightweight Kubernetes
Documentation=https://k3s.io
Wants=network-online.target
After=network-online.target

[Install]
WantedBy=multi-user.target

[Service]
Type=notify
EnvironmentFile=-/etc/systemd/system/k3s.service.env
KillMode=process
Delegate=yes
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
TimeoutStartSec=0
Restart=always
RestartSec=5s
ExecStartPre=-/sbin/modprobe br_netfilter
ExecStartPre=-/sbin/modprobe overlay
ExecStart=%s
`

const agentServiceTmpl = `[Unit]
Description=Lightweight Kubernetes Node
Documentation=https://k3s.io
Wants=network-online.target
After=network-online.target

[Install]
WantedBy=multi-user.target

[Service]
Type=notify
EnvironmentFile=-/etc/systemd/system/k3s-agent.service.env
KillMode=process
Delegate=yes
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
TimeoutStartSec=0
Restart=always
RestartSec=5s
ExecStartPre=-/sbin/modprobe br_netfilter
ExecStartPre=-/sbin/modprobe overlay
ExecStart=%s
`

func serverServiceFile(k *config.K3sSpec) []byte {
	args := []string{K3sBinary, "server"}
	if k.NodeIP != "" {
		args = append(args, "--node-ip", k.NodeIP)
	}
	for _, san := range k.TLSSANs {
		args = append(args, "--tls-san", san)
	}
	if k.FlannelIface != "" {
		args = append(args, "--flannel-iface", k.FlannelIface)
	}
	for _, addon := range k.DisableAddons {
		args = append(args, "--disable", addon)
	}
	return []byte(fmt.Sprintf(serverServiceTmpl, strings.Join(args, " ")))
}

func agentServiceFile(k *config.K3sSpec) []byte {
	args := []string{K3sBinary, "agent"}
	if k.NodeIP != "" {
		args = append(args, "--node-ip", k.NodeIP)
	}
	if k.FlannelIface != "" {
		args = append(args, "--flannel-iface", k.FlannelIface)
	}
	for _, label := range k.NodeLabels {
		args = append(args, "--node-label", label)
	}
	return []byte(fmt.Sprintf(agentServiceTmpl, strings.Join(args, " ")))
}

func agentEnvFile(k *config.K3sSpec) []byte {
	token := k.Token
	if k.TokenEnv != "" {
		token = os.Getenv(k.TokenEnv)
	}
	return []byte(fmt.Sprintf("K3S_URL=%s\nK3S_TOKEN=%s\n", k.ServerURL, token))
}

// waitForFile polls fsys for path until it exists with non-empty contents,
// the timeout is hit, or ctx is cancelled.
//
// The "non-empty" check matters for /var/lib/rancher/k3s/server/node-token:
// the installer creates the file slightly before writing the token, so a
// naive Stat-only loop occasionally races and reads zero bytes.
func (s *Section) waitForFile(ctx context.Context, fsys FS, path string) ([]byte, error) {
	interval := s.PollInterval
	if interval <= 0 {
		interval = kubeconfigPollInterval
	}
	timeout := s.PollTimeout
	if timeout <= 0 {
		timeout = kubeconfigPollTimeout
	}
	now := s.Now
	if now == nil {
		now = time.Now
	}
	deadline := now().Add(timeout)

	_, _ = fmt.Fprintf(s.out(), "k3s: waiting for %s (timeout %s)...\n", path, timeout)
	lastLog := now()

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
		data, err := fsys.ReadFile(path)
		if err == nil && len(data) > 0 {
			return data, nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if !now().Before(deadline) {
			if err == nil {
				return nil, fmt.Errorf("%s exists but is empty after %s", path, timeout)
			}
			return nil, fmt.Errorf("%s did not appear within %s", path, timeout)
		}
		if now().Sub(lastLog) >= 10*time.Second {
			lastLog = now()
			_, _ = fmt.Fprintf(s.out(), "k3s: still waiting for %s...\n", path)
		}
		timer.Reset(interval)
	}
}

// waitForAgent waits in two phases:
//
// Phase 1 — TLS bootstrap: polls for AgentKubeletKubeconfig. k3s-agent writes
// this file after receiving its signed client cert from the control plane.
// Kubelet has not yet started, so the node object does not yet exist in the API.
//
// Phase 2 — node registration: polls "kubectl get node <hostname>" using the
// kubelet kubeconfig until the node object appears in the API server. The kubelet
// cert (system:node:<hostname>) has GET permission on its own node via the
// default system:node ClusterRole.
func (s *Section) waitForAgent(ctx context.Context, runner Runner, fsys FS) error {
	interval := s.PollInterval
	if interval <= 0 {
		interval = agentPollInterval
	}
	timeout := s.PollTimeout
	if timeout <= 0 {
		timeout = agentPollTimeout
	}
	now := s.Now
	if now == nil {
		now = time.Now
	}
	deadline := now().Add(timeout)

	// ---- Phase 1: TLS bootstrap ----
	_, _ = fmt.Fprintf(s.out(), "k3s: waiting for agent TLS bootstrap (timeout %s)...\n", timeout)
	lastLog := now()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		if _, err := fsys.ReadFile(AgentKubeletKubeconfig); err == nil {
			break
		}
		if !now().Before(deadline) {
			return fmt.Errorf("k3s-agent did not bootstrap within %s", timeout)
		}
		if now().Sub(lastLog) >= 10*time.Second {
			lastLog = now()
			_, _ = fmt.Fprintln(s.out(), "k3s: still waiting for agent bootstrap...")
		}
		timer.Reset(interval)
	}
	_, _ = fmt.Fprintln(s.out(), "k3s: agent bootstrapped; waiting for node registration...")

	// ---- Phase 2: node registration ----
	regTimeout := s.PollTimeout
	if regTimeout <= 0 {
		regTimeout = nodeRegPollTimeout
	}
	regDeadline := now().Add(regTimeout)

	hostOut, _ := runner.Run(ctx, "hostname")
	nodeName := strings.TrimSpace(string(hostOut))

	lastLog = now()
	timer2 := time.NewTimer(0)
	defer timer2.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer2.C:
		}
		out, err := runner.Run(ctx, "kubectl",
			"--kubeconfig", AgentKubeletKubeconfig,
			"get", "node", nodeName,
			"--no-headers")
		if err == nil && (nodeName == "" || strings.Contains(string(out), nodeName)) {
			_, _ = fmt.Fprintln(s.out(), "k3s: agent joined cluster")
			return nil
		}
		if !now().Before(regDeadline) {
			return fmt.Errorf("k3s-agent node %q did not register within %s", nodeName, regTimeout)
		}
		if now().Sub(lastLog) >= 10*time.Second {
			lastLog = now()
			_, _ = fmt.Fprintf(s.out(), "k3s: still waiting for node %q to register...\n", nodeName)
		}
		timer2.Reset(interval)
	}
}

// collectAgentDiagnostics runs diagnostic commands after a failed agent join
// and prints results to s.Out(). Called from inside clusterboxnode (running as
// root via sudo), so journalctl and systemctl need no privilege escalation.
// serverURL is probed with curl so the operator can see whether the CP API is
// reachable at all.
func (s *Section) collectAgentDiagnostics(runner Runner, serverURL string) {
	_, _ = fmt.Fprintln(s.out(), "\n--- k3s-agent diagnostics ---")
	diagCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	type diagCmd struct {
		label string
		name  string
		args  []string
	}
	cmds := []diagCmd{
		{"ip addr", "ip", []string{"addr", "show"}},
		{"k3s-agent status", "systemctl", []string{"status", "k3s-agent.service", "--no-pager"}},
		{"k3s-agent journal", "journalctl", []string{"-n", "40", "-u", "k3s-agent.service", "--no-pager"}},
	}
	if serverURL != "" {
		cmds = append(cmds, diagCmd{
			label: "curl cp api",
			name:  "curl",
			args:  []string{"-sk", "--max-time", "5", serverURL + "/version"},
		})
	}
	for _, c := range cmds {
		out, _ := runner.Run(diagCtx, c.name, c.args...)
		_, _ = fmt.Fprintf(s.out(), "[diag: %s]\n%s\n", c.label, strings.TrimSpace(string(out)))
	}
	_, _ = fmt.Fprintln(s.out(), "--- end k3s-agent diagnostics ---")
}

func (s *Section) runner() Runner {
	if s.Runner != nil {
		return s.Runner
	}
	return execRunner{}
}

func (s *Section) fsys() FS {
	if s.FS != nil {
		return s.FS
	}
	return osFS{}
}

func (s *Section) out() io.Writer {
	if s.Out != nil {
		return s.Out
	}
	return io.Discard
}

func specK3s(spec *config.Spec) *config.K3sSpec {
	if spec == nil {
		return nil
	}
	return spec.K3s
}

// mapRole translates the k3s-native config role onto the public-facing label
// used in the T3 JSON contract. server and server-init are both control
// planes from the perspective of an external orchestrator.
func mapRole(role string) string {
	switch role {
	case "server", "server-init":
		return "control-plane"
	case "agent":
		return "worker"
	default:
		return role
	}
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// execRunner is the production [Runner] implementation backed by os/exec.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %v: %w: %s", name, args, err, string(out))
	}
	return out, nil
}

// osFS is the production [FS] implementation backed by the real filesystem.
type osFS struct{}

func (osFS) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }
func (osFS) ReadFile(path string) ([]byte, error)  { return os.ReadFile(path) }
func (osFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}
func (osFS) MkdirAll(path string, perm os.FileMode) error { return os.MkdirAll(path, perm) }
