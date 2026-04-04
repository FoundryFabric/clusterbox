package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"time"
)

// JoinConfig holds all inputs required to join an existing node to a k3s cluster.
type JoinConfig struct {
	// NodeIP is the Tailscale IP of the new worker node being joined.
	NodeIP string

	// ServerIP is the Tailscale IP of the existing k3s control-plane node.
	ServerIP string

	// K3sVersion is the exact k3s release to install, e.g. "v1.32.3+k3s1".
	// Defaults to DefaultK3sVersion when empty.
	K3sVersion string

	// User is the SSH user on both the node and server. Defaults to "clusterbox".
	User string

	// KubeconfigPath is the local path to the kubeconfig used when waiting for
	// the node to become Ready.
	KubeconfigPath string

	// SSHKeyPath is the path to the SSH private key used by k3sup.
	SSHKeyPath string
}

// effective returns a copy of cfg with defaults applied.
func (cfg JoinConfig) effective() JoinConfig {
	out := cfg
	if out.K3sVersion == "" {
		out.K3sVersion = DefaultK3sVersion
	}
	if out.User == "" {
		out.User = "clusterbox"
	}
	return out
}

// Join runs k3sup join to add a new worker node to an existing k3s cluster,
// then polls kubectl until the node is Ready or the context deadline (default
// 60 s) expires. It uses the real os/exec runner.
func Join(ctx context.Context, cfg JoinConfig) error {
	return JoinWith(ctx, cfg, ExecRunner{})
}

// JoinWith is the injectable variant used by tests.
func JoinWith(ctx context.Context, cfg JoinConfig, runner CommandRunner) error {
	cfg = cfg.effective()

	if err := runK3supJoin(ctx, cfg, runner); err != nil {
		return err
	}

	return waitForJoinedNode(ctx, cfg, runner)
}

// runK3supJoin executes the k3sup join command.
func runK3supJoin(ctx context.Context, cfg JoinConfig, runner CommandRunner) error {
	args := []string{
		"join",
		"--ip", cfg.NodeIP,
		"--server-ip", cfg.ServerIP,
		"--user", cfg.User,
		"--k3s-version", cfg.K3sVersion,
		"--ssh-key", cfg.SSHKeyPath,
	}

	if _, err := runner.Run(ctx, "k3sup", args...); err != nil {
		return fmt.Errorf("bootstrap: k3sup join: %w", err)
	}
	return nil
}

// waitForJoinedNode polls `kubectl get nodes` until the new node (identified by
// cfg.NodeIP) shows "Ready", or ctx is cancelled. A fresh 60-second deadline
// is applied if ctx has no deadline of its own.
func waitForJoinedNode(ctx context.Context, cfg JoinConfig, runner CommandRunner) error {
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
				return fmt.Errorf("bootstrap: joined node not ready within timeout: %w; last error: %v", ctx.Err(), lastErr)
			}
			return fmt.Errorf("bootstrap: joined node not ready within timeout: %w", ctx.Err())
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
			lastErr = fmt.Errorf("joined node not yet Ready: %s", bytes.TrimSpace(out))
		}
	}
}
