package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// CommandRunner is the interface used to execute external commands. Tests inject
// a mock; production code uses ExecRunner.
type CommandRunner interface {
	// Run executes name with args and returns combined stdout+stderr, or an
	// error that wraps the combined output when the process exits non-zero.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner is the production CommandRunner that shells out via os/exec.
type ExecRunner struct{}

// Run implements CommandRunner using os/exec.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w\noutput: %s", name, err, bytes.TrimSpace(out))
	}
	return out, nil
}

// Bootstrap installs k3s on the node described by cfg using k3sup over Tailscale
// SSH, then polls kubectl until the node is Ready or the context deadline
// (default 60 s) expires.
func Bootstrap(ctx context.Context, cfg K3sConfig) error {
	return BootstrapWith(ctx, cfg, ExecRunner{})
}

// BootstrapWith is the injectable variant used by tests.
func BootstrapWith(ctx context.Context, cfg K3sConfig, runner CommandRunner) error {
	cfg = cfg.effective()

	if err := runK3sup(ctx, cfg, runner); err != nil {
		return err
	}

	return waitForNode(ctx, cfg, runner)
}

// runK3sup executes the k3sup install command.
func runK3sup(ctx context.Context, cfg K3sConfig, runner CommandRunner) error {
	args := []string{
		"install",
		"--ip", cfg.TailscaleIP,
		"--user", cfg.User,
		"--k3s-version", cfg.K3sVersion,
		"--local-path", cfg.KubeconfigPath,
		"--ssh-key", cfg.SSHKeyPath,
		"--context", "clusterbox",
	}

	if _, err := runner.Run(ctx, "k3sup", args...); err != nil {
		return fmt.Errorf("bootstrap: k3sup install: %w", err)
	}
	return nil
}

// waitForNode polls `kubectl get nodes` until at least one node shows "Ready"
// or ctx is cancelled. A fresh 60-second deadline is applied if ctx has no
// deadline of its own.
func waitForNode(ctx context.Context, cfg K3sConfig, runner CommandRunner) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("bootstrap: node not ready within timeout: %w; last error: %v", ctx.Err(), lastErr)
			}
			return fmt.Errorf("bootstrap: node not ready within timeout: %w", ctx.Err())
		case <-ticker.C:
			out, err := runner.Run(ctx, "kubectl",
				"--kubeconfig", cfg.KubeconfigPath,
				"get", "nodes",
			)
			if err != nil {
				lastErr = err
				continue
			}
			if bytes.Contains(out, []byte(" Ready")) {
				return nil
			}
			lastErr = fmt.Errorf("node not yet Ready: %s", bytes.TrimSpace(out))
		}
	}
}
