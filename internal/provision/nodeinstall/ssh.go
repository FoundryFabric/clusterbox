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

// RunAgent uploads agentBytes and specYAML to the remote host, runs
// `sudo clusterboxnode install --config <path>`, and returns the raw
// stdout (the clusterboxnode JSON envelope).
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

	_, _ = fmt.Fprintln(out, "nodeinstall: running clusterboxnode install (this may take a few minutes)...")
	installCmd := fmt.Sprintf("sudo %s install --config %s", binPath, cfgPath)
	stdout, err := SSHRun(ctx, cfg, installCmd)
	// Best-effort cleanup regardless of install success/failure.
	_, _ = SSHRun(ctx, cfg, "rm -f "+binPath+" "+cfgPath)
	if err != nil {
		return nil, fmt.Errorf("nodeinstall: clusterboxnode install: %w", err)
	}
	return []byte(stdout), nil
}
