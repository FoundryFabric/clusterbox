// Package qemu implements a local provision.Provider that boots an Ubuntu
// 22.04 VM via QEMU to exercise the full provisioning flow (cloud-init,
// k3sup, k3s) without a cloud account.
//
// This is a dev/test tool, not production infrastructure.
package qemu

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/registry"
)

// Name is the canonical --provider value for this provider.
const Name = "qemu"

// Deps groups the injectable dependencies for the QEMU provider.
// Tests replace fields; nil fields fall back to production defaults.
type Deps struct {
	// Out is the destination for human-readable progress lines.
	// When nil the provider writes to os.Stderr.
	Out io.Writer

	// SSHKeyPath is the path to the SSH private key (and .pub file).
	// Defaults to ~/.ssh/id_ed25519.
	SSHKeyPath string

	// KubeconfigPath is the destination the provider writes the kubeconfig to.
	// When empty the provider derives ~/.kube/<clusterName>.yaml.
	KubeconfigPath string

	// StateDir is the directory for per-cluster QEMU state files.
	// When empty the provider uses ~/.clusterbox/qemu/<clusterName>/.
	StateDir string

	// CacheDir is the directory for shared disk image cache.
	// When empty the provider uses ~/.clusterbox/cache/qemu/.
	CacheDir string

	// Bootstrap is the function that runs k3sup against the VM.
	// Injectable for tests; production code uses bootstrap.Bootstrap.
	Bootstrap func(ctx context.Context, cfg bootstrap.K3sConfig) error
}

// Provider is the QEMU implementation of provision.Provider.
type Provider struct {
	deps Deps
}

// New constructs a QEMU Provider with the given dependencies.
func New(deps Deps) *Provider {
	return &Provider{deps: deps}
}

// Name returns the canonical provider identifier ("qemu").
func (p *Provider) Name() string { return Name }

// Provision boots a fresh Ubuntu 22.04 VM via QEMU and installs k3s.
//
// Steps:
//  1. Resolve state dir (~/.clusterbox/qemu/<clusterName>/)
//  2. Download base Ubuntu 22.04 cloud image if not cached.
//  3. Create overlay QCOW2 disk (base stays clean).
//  4. Generate cloud-init seed ISO.
//  5. Pick a free TCP port for SSH forwarding.
//  6. Save port to <state-dir>/ssh.port.
//  7. Launch QEMU as a background orphan process.
//  8. Save QEMU PID to <state-dir>/vm.pid.
//  9. Poll SSH until ready (10 min timeout).
//  10. Run k3sup bootstrap.
//  11. Return ProvisionResult.
func (p *Provider) Provision(ctx context.Context, cfg provision.ClusterConfig) (provision.ProvisionResult, error) {
	out := p.out()
	clusterName := cfg.ClusterName

	// Step 1: resolve directories.
	stateDir, err := p.stateDir(clusterName)
	if err != nil {
		return provision.ProvisionResult{}, err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: mkdir state dir: %w", err)
	}

	cacheDir, err := p.cacheDir()
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	kubeconfigPath, err := p.kubeconfigPath(clusterName)
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	sshKeyPath, err := p.sshKeyPath()
	if err != nil {
		return provision.ProvisionResult{}, err
	}
	sshPubKeyPath := sshKeyPath + ".pub"

	// Step 2: download base image.
	_, _ = fmt.Fprintln(out, "[1/7] Downloading base Ubuntu 22.04 cloud image (if not cached)...")
	baseImage, err := EnsureBaseImage(ctx, cacheDir, out)
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	// Step 3: create overlay QCOW2 disk.
	_, _ = fmt.Fprintln(out, "[2/7] Creating overlay QCOW2 disk...")
	diskPath := filepath.Join(stateDir, "disk.qcow2")
	if err := createOverlayDisk(baseImage, diskPath); err != nil {
		return provision.ProvisionResult{}, err
	}

	// Step 4: generate cloud-init seed ISO.
	_, _ = fmt.Fprintln(out, "[3/7] Generating cloud-init seed ISO...")
	seedPath := filepath.Join(stateDir, "seed.iso")
	sshPubKey, err := os.ReadFile(sshPubKeyPath)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: read SSH public key %s: %w", sshPubKeyPath, err)
	}
	ciDir, err := os.MkdirTemp("", "qemu-cloudinit-*")
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: create cloud-init temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(ciDir) }()

	if err := WriteCloudInitFiles(ciDir, clusterName, strings.TrimSpace(string(sshPubKey))); err != nil {
		return provision.ProvisionResult{}, err
	}
	if err := MakeSeedISO(ciDir, seedPath); err != nil {
		return provision.ProvisionResult{}, err
	}

	// Step 5: pick a free TCP port for SSH.
	_, _ = fmt.Fprintln(out, "[4/7] Selecting free SSH forwarding port...")
	sshPort, err := findFreePort(2200)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: find free port: %w", err)
	}

	// Step 6: save port.
	portFile := filepath.Join(stateDir, "ssh.port")
	if err := os.WriteFile(portFile, []byte(strconv.Itoa(sshPort)), 0o600); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: write ssh.port: %w", err)
	}

	// Step 7: launch QEMU as orphan.
	_, _ = fmt.Fprintf(out, "[5/7] Launching QEMU (SSH forwarded to localhost:%d)...\n", sshPort)
	logPath := filepath.Join(stateDir, "qemu.log")
	pid, err := launchQEMU(diskPath, seedPath, logPath, sshPort)
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	// Step 8: save PID.
	pidFile := filepath.Join(stateDir, "vm.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: write vm.pid: %w", err)
	}

	// Step 9: poll SSH until ready.
	_, _ = fmt.Fprintf(out, "[6/7] Waiting for VM SSH on localhost:%d (up to 10 min)...\n", sshPort)
	if err := waitForSSH(ctx, sshPort, 10*time.Minute, out); err != nil {
		return provision.ProvisionResult{}, err
	}

	// Step 10: run k3sup bootstrap.
	// k3sup is called directly (not via bootstrap.Bootstrap) so we can pass
	// --ssh-port. See runBootstrap for details.
	_, _ = fmt.Fprintln(out, "[7/7] Running k3sup bootstrap...")
	if err := p.runBootstrap(ctx, sshPort, sshKeyPath, kubeconfigPath); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: bootstrap: %w", err)
	}

	now := time.Now().UTC()
	return provision.ProvisionResult{
		KubeconfigPath: kubeconfigPath,
		Nodes: []registry.Node{
			{
				ClusterName: clusterName,
				Hostname:    clusterName,
				Role:        "control-plane",
				JoinedAt:    now,
			},
		},
	}, nil
}

// Destroy stops the QEMU VM and removes the state directory.
func (p *Provider) Destroy(_ context.Context, cluster registry.Cluster) error {
	stateDir, err := p.stateDir(cluster.Name)
	if err != nil {
		return err
	}

	// Read PID and kill the process.
	pidFile := filepath.Join(stateDir, "vm.pid")
	pidBytes, err := os.ReadFile(pidFile)
	if err == nil {
		pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
		if err == nil {
			if proc, err := os.FindProcess(pid); err == nil {
				// Best-effort kill; ignore "process already finished" errors.
				_ = proc.Kill()
			}
		}
	}

	// Remove state directory entirely.
	if err := os.RemoveAll(stateDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("qemu: remove state dir: %w", err)
	}
	return nil
}

// Reconcile checks whether the QEMU VM is still running.
func (p *Provider) Reconcile(_ context.Context, clusterName string) (provision.ReconcileSummary, error) {
	stateDir, err := p.stateDir(clusterName)
	if err != nil {
		return provision.ReconcileSummary{}, nil //nolint:nilerr
	}

	pidFile := filepath.Join(stateDir, "vm.pid")
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		// No PID file means no tracked VM.
		return provision.ReconcileSummary{MarkedDestroyed: 1}, nil
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		return provision.ReconcileSummary{MarkedDestroyed: 1}, nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return provision.ReconcileSummary{MarkedDestroyed: 1}, nil
	}

	// Signal(0) checks if process is alive without sending a real signal.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return provision.ReconcileSummary{MarkedDestroyed: 1}, nil
	}

	return provision.ReconcileSummary{Existing: 1}, nil
}

// ---- helpers ---------------------------------------------------------------

func (p *Provider) out() io.Writer {
	if p.deps.Out != nil {
		return p.deps.Out
	}
	return os.Stderr
}

func (p *Provider) stateDir(clusterName string) (string, error) {
	if p.deps.StateDir != "" {
		return p.deps.StateDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("qemu: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".clusterbox", "qemu", clusterName), nil
}

func (p *Provider) cacheDir() (string, error) {
	if p.deps.CacheDir != "" {
		return p.deps.CacheDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("qemu: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".clusterbox", "cache", "qemu"), nil
}

func (p *Provider) kubeconfigPath(clusterName string) (string, error) {
	if p.deps.KubeconfigPath != "" {
		return p.deps.KubeconfigPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("qemu: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".kube", clusterName+".yaml"), nil
}

func (p *Provider) sshKeyPath() (string, error) {
	if p.deps.SSHKeyPath != "" {
		return p.deps.SSHKeyPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("qemu: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".ssh", "id_ed25519"), nil
}

// runBootstrap installs k3s on the VM using k3sup via the SSH port forward.
//
// bootstrap.K3sConfig does not have an SSHPort field so we call k3sup
// directly here to pass --ssh-port. If Deps.Bootstrap is set (tests), that
// function is called instead and sshPort is ignored.
//
// TODO: Once bootstrap.K3sConfig gains an SSHPort field, replace this with
// a call to bootstrap.Bootstrap and remove the direct k3sup invocation.
func (p *Provider) runBootstrap(ctx context.Context, sshPort int, sshKeyPath, kubeconfigPath string) error {
	cfg := bootstrap.K3sConfig{
		TailscaleIP:    "127.0.0.1",
		User:           "ubuntu",
		SSHKeyPath:     sshKeyPath,
		KubeconfigPath: kubeconfigPath,
	}

	// Injectable override for tests.
	if p.deps.Bootstrap != nil {
		return p.deps.Bootstrap(ctx, cfg)
	}

	// Production: call k3sup directly so we can pass --ssh-port.
	args := []string{
		"install",
		"--ip", cfg.TailscaleIP,
		"--user", cfg.User,
		"--local-path", cfg.KubeconfigPath,
		"--ssh-key", cfg.SSHKeyPath,
		"--context", "clusterbox",
		"--ssh-port", strconv.Itoa(sshPort),
	}
	if cfg.K3sVersion != "" {
		args = append(args, "--k3s-version", cfg.K3sVersion)
	}

	cmd := exec.CommandContext(ctx, "k3sup", args...) //nolint:gosec
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("k3sup install: %w\noutput: %s", err, out)
	}
	return nil
}

// createOverlayDisk creates a QCOW2 overlay disk backed by baseImage.
// If disk.qcow2 already exists it is left in place (idempotent).
func createOverlayDisk(baseImage, diskPath string) error {
	if _, err := os.Stat(diskPath); err == nil {
		return nil // already exists
	}
	cmd := exec.Command("qemu-img", "create", //nolint:gosec
		"-f", "qcow2",
		"-b", baseImage,
		"-F", "qcow2",
		diskPath,
		"20G",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu: qemu-img create: %w (output: %s)", err, out)
	}
	return nil
}

// launchQEMU starts a QEMU process in the background and returns its PID.
// The process is orphaned (Start + Release) so it outlives the CLI.
func launchQEMU(diskPath, seedPath, logPath string, sshPort int) (int, error) {
	qemuBin, args := buildQEMUArgs(diskPath, seedPath, sshPort)

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("qemu: open log file: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(qemuBin, args...) //nolint:gosec
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("qemu: start VM: %w", err)
	}

	pid := cmd.Process.Pid
	// Detach: release so the child outlives this process.
	if err := cmd.Process.Release(); err != nil {
		return 0, fmt.Errorf("qemu: release VM process: %w", err)
	}

	return pid, nil
}

// buildQEMUArgs returns the QEMU binary name and argument list for the
// current host architecture.
func buildQEMUArgs(diskPath, seedPath string, sshPort int) (string, []string) {
	arch := runtime.GOARCH
	netdev := fmt.Sprintf("user,id=net0,hostfwd=tcp::%d-:22", sshPort)

	switch arch {
	case "arm64":
		qemuBin := "qemu-system-aarch64"
		args := []string{
			"-m", "2048",
			"-smp", "2",
			"-nographic",
			"-no-reboot",
			"-machine", "virt",
			"-cpu", "host",
			"-drive", "file=" + diskPath + ",format=qcow2,if=virtio",
			"-drive", "file=" + seedPath + ",format=raw,if=virtio",
			"-netdev", netdev,
			"-device", "virtio-net-pci,netdev=net0",
		}
		// Try the standard Homebrew BIOS path; fall back to omitting -bios.
		biosPath := "/opt/homebrew/share/qemu/edk2-aarch64-code.fd"
		if _, err := os.Stat(biosPath); err == nil {
			args = append(args, "-bios", biosPath)
		}
		return qemuBin, args

	default: // amd64
		qemuBin := "qemu-system-x86_64"
		args := []string{
			"-m", "2048",
			"-smp", "2",
			"-nographic",
			"-no-reboot",
			"-drive", "file=" + diskPath + ",format=qcow2,if=virtio",
			"-drive", "file=" + seedPath + ",format=raw,if=virtio",
			"-netdev", netdev,
			"-device", "virtio-net-pci,netdev=net0",
		}
		return qemuBin, args
	}
}

// findFreePort finds a free TCP port on localhost starting at start.
func findFreePort(start int) (int, error) {
	for port := start; port < start+100; port++ {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		_ = ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free port found in range %d-%d", start, start+99)
}

// waitForSSH polls the VM's SSH port until a connection succeeds or the
// timeout expires.
func waitForSSH(ctx context.Context, port int, timeout time.Duration, out io.Writer) error {
	deadline := time.Now().Add(timeout)
	ctx2, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Immediately try once before starting the ticker loop.
	if trySSH(addr) {
		return nil
	}

	for {
		select {
		case <-ctx2.Done():
			return fmt.Errorf("qemu: timed out waiting for SSH on %s after %s", addr, timeout)
		case <-ticker.C:
			_, _ = fmt.Fprintf(out, "qemu: waiting for VM SSH on %s...\n", addr)
			if trySSH(addr) {
				_, _ = fmt.Fprintf(out, "qemu: VM SSH is ready on %s\n", addr)
				return nil
			}
		}
	}
}

// trySSH attempts a TCP connection to addr to check if SSH is listening.
func trySSH(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Compile-time check: *Provider satisfies provision.Provider.
var _ provision.Provider = (*Provider)(nil)
