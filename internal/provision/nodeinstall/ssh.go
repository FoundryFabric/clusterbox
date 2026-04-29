package nodeinstall

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// installTimeout bounds a single clusterboxnode install run.
// k3s download + install typically takes 1-3 minutes; 5 min gives headroom
// on slow links while still preventing an indefinite hang when the VM is stuck.
const installTimeout = 5 * time.Minute

// SSHConfig holds the connection parameters for SSH/SCP operations.
type SSHConfig struct {
	// Host is the hostname or IP address of the remote machine.
	Host string
	// Port is the SSH port. Defaults to 22 when zero.
	Port int
	// User is the SSH login user.
	User string
	// KeyPath is the path to the SSH private key.
	KeyPath string
}

func (c SSHConfig) portStr() string {
	if c.Port > 0 {
		return strconv.Itoa(c.Port)
	}
	return "22"
}

// sshFlags returns the common SSH flags shared by SSHRun and WaitForSSH.
//
// StrictHostKeyChecking=no is intentional: clusterbox provisions freshly-imaged
// VMs whose host keys are unknown at connection time. The VMs are reachable only
// via a private Tailscale overlay (Hetzner) or a host-local port forward (QEMU),
// so MITM attacks are not a realistic threat for this use case.
func sshFlags(cfg SSHConfig) []string {
	return []string{
		"-p", cfg.portStr(),
		"-i", cfg.KeyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=20",
	}
}

// SSHRun executes command on the remote host described by cfg and returns
// combined stdout as a string.
func SSHRun(ctx context.Context, cfg SSHConfig, command string) (string, error) {
	args := append(sshFlags(cfg), cfg.User+"@"+cfg.Host, command)
	cmd := exec.CommandContext(ctx, "ssh", args...) //nolint:gosec
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return "", fmt.Errorf("ssh run: %w\n%s", err, strings.TrimSpace(stderr.String()))
		}
		return "", fmt.Errorf("ssh run: %w", err)
	}
	return stdout.String(), nil
}

// SCPUploadBytes writes data to a temp file then SCPs it to remotePath on
// the host described by cfg.
func SCPUploadBytes(ctx context.Context, cfg SSHConfig, data []byte, remotePath string) error {
	tmp, err := os.CreateTemp("", "nodeinstall-upload-*")
	if err != nil {
		return fmt.Errorf("nodeinstall: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("nodeinstall: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("nodeinstall: close temp file: %w", err)
	}
	args := []string{
		"-P", cfg.portStr(),
		"-i", cfg.KeyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
		tmpPath,
		cfg.User + "@" + cfg.Host + ":" + remotePath,
	}
	cmd := exec.CommandContext(ctx, "scp", args...) //nolint:gosec
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nodeinstall: scp %s: %w (output: %s)", remotePath, err, out)
	}
	return nil
}

// ProbeArch SSHs into the host, runs `uname -m`, and returns the
// agentbundle arch token ("amd64" or "arm64").
func ProbeArch(ctx context.Context, cfg SSHConfig) (string, error) {
	out, err := SSHRun(ctx, cfg, "uname -m")
	if err != nil {
		return "", fmt.Errorf("nodeinstall: probe arch: %w", err)
	}
	arch, err := MapArch(out)
	if err != nil {
		return "", err
	}
	return arch, nil
}

// WaitForSSH polls until a real SSH login succeeds or the timeout
// expires. It tests authentication (not just TCP) so callers know
// cloud-init or key injection has completed before proceeding.
func WaitForSSH(ctx context.Context, cfg SSHConfig, timeout time.Duration, out io.Writer) error {
	deadline := time.Now().Add(timeout)
	ctx2, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		if _, err := SSHRun(ctx2, cfg, "true"); err == nil {
			_, _ = fmt.Fprintf(out, "nodeinstall: SSH ready on %s:%s\n", cfg.Host, cfg.portStr())
			return nil
		}
		_, _ = fmt.Fprintf(out, "nodeinstall: waiting for SSH on %s:%s...\n", cfg.Host, cfg.portStr())
		select {
		case <-ctx2.Done():
			return fmt.Errorf("nodeinstall: timed out waiting for SSH on %s:%s after %s",
				cfg.Host, cfg.portStr(), timeout)
		case <-ticker.C:
		}
	}
}

// RunNodeAgent probes the remote arch, selects the agent binary via loader,
// uploads it with specYAML, runs clusterboxnode install, and returns the parsed
// result. It consolidates the ProbeArch → load → RunAgent → ParseInstallOutput
// sequence that every SSH-based provider repeats for CP and worker installs.
func RunNodeAgent(ctx context.Context, cfg SSHConfig, specYAML []byte, loader func(string) ([]byte, error), out io.Writer) (*Result, error) {
	arch, err := ProbeArch(ctx, cfg)
	if err != nil {
		return nil, err
	}
	agentBytes, err := loader(arch)
	if err != nil {
		return nil, fmt.Errorf("nodeinstall: agent bundle for %s: %w", arch, err)
	}
	stdout, err := RunAgent(ctx, cfg, agentBytes, specYAML, out)
	if err != nil {
		return nil, err
	}
	result, err := ParseInstallOutput(stdout)
	if err != nil {
		return nil, fmt.Errorf("nodeinstall: parse install output: %w", err)
	}
	if result.IsError() {
		return nil, result.AsError(0, nil)
	}
	return result, nil
}

// CollectAgentDiagnostics SSHes into a worker node after a failed k3s agent
// join and dumps diagnostics to out. cpAPIURL is the URL the worker uses to
// reach the k3s API server (e.g. "https://10.0.2.2:16443" for QEMU or
// "https://10.0.1.2:6443" for Hetzner). Always runs on failure — it only
// fires when something has already gone wrong so the noise cost is zero on
// success.
func CollectAgentDiagnostics(ctx context.Context, cfg SSHConfig, cpAPIURL string, out io.Writer) {
	_, _ = fmt.Fprintln(out, "\n--- worker diagnostics ---")
	cmds := []struct {
		label string
		cmd   string
	}{
		{"ip addr", "ip addr show"},
		{"curl cp api", fmt.Sprintf("curl -sk --max-time 5 %s/version 2>&1 || echo UNREACHABLE", cpAPIURL)},
		{"k3s-agent env", "sudo cat /etc/systemd/system/k3s-agent.service.env 2>/dev/null || echo '(not found)'"},
		{"k3s-agent status", "systemctl status k3s-agent.service --no-pager 2>&1 || true"},
		{"k3s-agent journal", "sudo journalctl -n 40 -u k3s-agent.service --no-pager 2>&1 || true"},
	}
	diagCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, c := range cmds {
		_, _ = fmt.Fprintf(out, "\n[diag: %s]\n", c.label)
		result, err := SSHRun(diagCtx, cfg, c.cmd)
		if err != nil {
			_, _ = fmt.Fprintf(out, "(ssh error: %v)\n", err)
			continue
		}
		_, _ = fmt.Fprintln(out, result)
	}
	_, _ = fmt.Fprintln(out, "--- end worker diagnostics ---")
}

// ReadRemoteFile reads path on the remote host via SSH and returns its contents.
func ReadRemoteFile(ctx context.Context, cfg SSHConfig, path string) (string, error) {
	out, err := SSHRun(ctx, cfg, "sudo cat "+path)
	if err != nil {
		return "", fmt.Errorf("nodeinstall: read remote %s: %w", path, err)
	}
	return out, nil
}

// WaitForRemoteFile polls until path exists and has non-empty content on the
// remote host, the timeout elapses, or ctx is cancelled.
func WaitForRemoteFile(ctx context.Context, cfg SSHConfig, path string, timeout time.Duration, out io.Writer) (string, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		content, err := ReadRemoteFile(ctx, cfg, path)
		if err == nil && strings.TrimSpace(content) != "" {
			return content, nil
		}
		_, _ = fmt.Fprintf(out, "nodeinstall: waiting for %s on %s...\n", path, cfg.Host)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("nodeinstall: timed out waiting for %s after %s", path, timeout)
		}
	}
}

// RunAgent uploads agentBytes and specYAML to the remote host, runs
// `sudo clusterboxnode install --config <path>`, and returns the raw
// stdout (the clusterboxnode JSON envelope).
//
// SSH stdout is tee'd to out in real time so the caller sees progress lines
// (section starts, k3s installer progress, kubeconfig wait) as they happen.
// The install command is bounded by installTimeout (10 min) to prevent an
// indefinite hang when the VM or network is stuck.
//
// Best-effort cleanup of uploaded files is performed before returning.
func RunAgent(ctx context.Context, cfg SSHConfig, agentBytes, specYAML []byte, out io.Writer) ([]byte, error) {
	binPath := "/tmp/clusterboxnode-" + ShortSHA(agentBytes)
	cfgPath := "/tmp/clusterbox-node-" + ShortSHA(specYAML) + ".yaml"

	_, _ = fmt.Fprintf(out, "nodeinstall: uploading clusterboxnode binary...\n")
	if err := SCPUploadBytes(ctx, cfg, agentBytes, binPath); err != nil {
		return nil, err
	}
	if err := SCPUploadBytes(ctx, cfg, specYAML, cfgPath); err != nil {
		return nil, err
	}

	if _, err := SSHRun(ctx, cfg, "chmod +x "+binPath); err != nil {
		return nil, fmt.Errorf("nodeinstall: chmod clusterboxnode: %w", err)
	}

	_, _ = fmt.Fprintln(out, "nodeinstall: running clusterboxnode install (timeout 5m)...")
	installCmd := fmt.Sprintf("sudo %s install --config %s", binPath, cfgPath)

	// Apply a hard timeout to the install SSH command so a stuck VM or
	// failed k3s-agent join does not block the caller indefinitely.
	installCtx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()

	// Stream clusterboxnode stdout to out in real time (progress lines) while
	// also buffering it so ParseInstallOutput can find the final JSON envelope.
	var stdoutBuf bytes.Buffer
	args := append(sshFlags(cfg), cfg.User+"@"+cfg.Host, installCmd) //nolint:gocritic
	cmd := exec.CommandContext(installCtx, "ssh", args...)            //nolint:gosec
	cmd.Stdout = io.MultiWriter(&stdoutBuf, out)
	cmd.Stderr = out

	runErr := cmd.Run()
	// Best-effort cleanup regardless of install success/failure.
	_, _ = SSHRun(ctx, cfg, "rm -f "+binPath+" "+cfgPath)
	if runErr != nil {
		if installCtx.Err() != nil {
			return stdoutBuf.Bytes(), fmt.Errorf("nodeinstall: clusterboxnode install timed out after %s", installTimeout)
		}
		return stdoutBuf.Bytes(), fmt.Errorf("nodeinstall: clusterboxnode install: %w", runErr)
	}
	return stdoutBuf.Bytes(), nil
}
