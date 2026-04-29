// Package k3s implements the k3s install/uninstall section of clusterboxnode.
//
// The package exposes two top-level entry points, Apply and Remove, that
// return a structured Result the section walker maps onto its
// install.SectionResult. All process and file system access flows through
// the Runner and FS interfaces so the implementation is fully unit-testable.
//
// The install path is intentionally minimal:
//
//   - Skip if k3s is already present on disk or active under systemd.
//   - Otherwise pipe the official get.k3s.io installer to sh with
//     INSTALL_K3S_VERSION set from the spec.
//   - Wait up to 60s for /etc/rancher/k3s/k3s.yaml to appear, then read it
//     along with the server node-token.
//
// Worker (agent) joins are deliberately out of scope for the first release;
// requesting role=agent returns a clear placeholder error rather than a
// silent no-op so callers find out at install time rather than at first use.
package k3s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
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
	K3sUninstallSh   = "/usr/local/bin/k3s-uninstall.sh"
	KubeconfigPath   = "/etc/rancher/k3s/k3s.yaml"
	NodeTokenPath    = "/var/lib/rancher/k3s/server/node-token"
	DefaultServerURL = "https://127.0.0.1:6443"

	// InstallURL is the upstream installer endpoint piped to sh.
	InstallURL = "https://get.k3s.io"

	// kubeconfigPollInterval and kubeconfigPollTimeout bound the wait for
	// /etc/rancher/k3s/k3s.yaml after the installer returns.
	kubeconfigPollInterval = 500 * time.Millisecond
	kubeconfigPollTimeout  = 60 * time.Second
)

// Runner abstracts process execution so unit tests can inject a fake.
//
// Implementations must respect ctx cancellation and surface stderr in any
// returned error so failed shell pipelines remain debuggable.
type Runner interface {
	// Run executes name with args and returns combined stdout. Errors must
	// wrap any non-zero exit status with stderr context.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	// RunShell executes a shell pipeline (e.g. "curl ... | sh -") under
	// /bin/sh -c with the supplied environment overlay.
	RunShell(ctx context.Context, env []string, script string) ([]byte, error)
}

// FS abstracts filesystem reads so tests do not require root-owned paths.
type FS interface {
	Stat(path string) (fs.FileInfo, error)
	ReadFile(path string) ([]byte, error)
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
	Extra map[string]interface{}
}

// Section bundles the dependencies (Runner, FS, clock, polling knobs) used
// by Apply and Remove.
//
// All fields have working zero-value defaults so production callers can
// construct a zero Section. Tests override Runner and FS to drive
// deterministic scenarios.
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
}

// Apply installs k3s if it is not already present and returns a structured
// Result describing what happened.
//
// Behaviour matrix:
//
//   - spec.K3s nil or Enabled=false: Applied=false, Reason="disabled".
//   - role=agent: runs the agent installer (K3S_URL + K3S_TOKEN), returns
//     Applied=true without waiting for a kubeconfig (agents don't have one).
//   - role=server / server-init and k3s already present: Applied=true with
//     Reason="already installed" — kubeconfig and node-token are still read
//     so the parent control plane can pick them up.
//   - otherwise: run the installer, poll for kubeconfig+token, return
//     Applied=true.
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
		if err := s.runInstaller(ctx, runner, k); err != nil {
			return Result{}, err
		}
	}

	if k.Role == "agent" {
		res := Result{
			Applied: true,
			Extra: map[string]interface{}{
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
		Extra: map[string]interface{}{
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

// Remove runs the upstream uninstall script when k3s is present.
//
// The output payload always includes k3s_was_present so callers can tell the
// difference between "we cleaned it up" and "it was never here". Returning
// removed=true means the script ran without error or k3s was already absent
// — in both cases the post-condition (no k3s on the box) holds.
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
			Extra: map[string]interface{}{
				"removed":         true,
				"k3s_was_present": false,
			},
		}, nil
	}

	// The official installer drops k3s-uninstall.sh next to the k3s binary.
	// If it's missing (manual install, partial cleanup) we surface the error
	// rather than guessing.
	if _, err := fsys.Stat(K3sUninstallSh); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Result{}, fmt.Errorf("k3s: %s missing on a host that has k3s installed", K3sUninstallSh)
		}
		return Result{}, fmt.Errorf("k3s: stat %s: %w", K3sUninstallSh, err)
	}
	if _, err := runner.Run(ctx, K3sUninstallSh); err != nil {
		return Result{}, fmt.Errorf("k3s: %s: %w", K3sUninstallSh, err)
	}
	return Result{
		Applied: true,
		Extra: map[string]interface{}{
			"removed":         true,
			"k3s_was_present": true,
		},
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

// runInstaller pipes the official installer to /bin/sh, setting environment
// variables from k to configure role, node-ip, tls-san, and join credentials.
func (s *Section) runInstaller(ctx context.Context, runner Runner, k *config.K3sSpec) error {
	env := []string{"INSTALL_K3S_VERSION=" + k.Version}

	var execParts []string
	switch k.Role {
	case "agent":
		execParts = append(execParts, "agent")
		if k.NodeIP != "" {
			execParts = append(execParts, "--node-ip", k.NodeIP)
		}
		if k.FlannelIface != "" {
			execParts = append(execParts, "--flannel-iface", k.FlannelIface)
		}
		for _, label := range k.NodeLabels {
			execParts = append(execParts, "--node-label", label)
		}
		env = append(env, "K3S_URL="+k.ServerURL)
		token := k.Token
		if k.TokenEnv != "" {
			token = os.Getenv(k.TokenEnv)
		}
		env = append(env, "K3S_TOKEN="+token)
	default: // server, server-init
		if k.NodeIP != "" {
			execParts = append(execParts, "--node-ip", k.NodeIP)
		}
		for _, san := range k.TLSSANs {
			execParts = append(execParts, "--tls-san", san)
		}
		if k.FlannelIface != "" {
			execParts = append(execParts, "--flannel-iface", k.FlannelIface)
		}
	}
	if len(execParts) > 0 {
		env = append(env, "INSTALL_K3S_EXEC="+strings.Join(execParts, " "))
	}

	_, _ = fmt.Fprintf(s.out(), "k3s: running installer %s (role=%s, this may take several minutes)...\n", k.Version, k.Role)
	script := fmt.Sprintf("curl -sfL %s | sh -", InstallURL)
	if _, err := runner.RunShell(ctx, env, script); err != nil {
		return fmt.Errorf("k3s: installer: %w", err)
	}
	_, _ = fmt.Fprintf(s.out(), "k3s: installer complete\n")
	return nil
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

func (execRunner) RunShell(ctx context.Context, env []string, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("/bin/sh -c %q: %w: %s", script, err, string(out))
	}
	return out, nil
}

// osFS is the production [FS] implementation backed by the real filesystem.
type osFS struct{}

func (osFS) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }
func (osFS) ReadFile(path string) ([]byte, error)  { return os.ReadFile(path) }
