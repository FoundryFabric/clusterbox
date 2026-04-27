// Package qemu implements a local provision.Provider that boots an Ubuntu
// 22.04 VM via QEMU to exercise the full provisioning flow (cloud-init,
// k3sup, k3s) without a cloud account.
//
// This is a dev/test tool, not production infrastructure.
package qemu

import (
	"bytes"
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

	// Bootstrap is called instead of the built-in SSH bootstrap when set.
	// Tests inject a no-op here to skip real SSH/k3s calls.
	Bootstrap func(ctx context.Context, sshPort, k3sPort int, sshKeyPath, kubeconfigPath string) error
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
//  11. Write cluster.json for future add-node calls.
//  12. Return ProvisionResult.
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
	// If any later step fails, remove the partial state dir so the next run
	// starts clean instead of hitting "file already exists" style errors.
	provisionOK := false
	defer func() {
		if !provisionOK {
			_ = os.RemoveAll(stateDir)
		}
	}()

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

	// Control-plane is always nodeIdx=0, cluster IP 10.100.0.1/24.
	const cpClusterIP = "10.100.0.1/24"
	if err := WriteCloudInitFiles(ciDir, clusterName, strings.TrimSpace(string(sshPubKey)), 0, cpClusterIP); err != nil {
		return provision.ProvisionResult{}, err
	}
	if err := MakeSeedISO(ciDir, seedPath); err != nil {
		return provision.ProvisionResult{}, err
	}

	// Step 5: pick free TCP ports for SSH and the k3s API.
	_, _ = fmt.Fprintln(out, "[4/7] Selecting free forwarding ports...")
	sshPort, err := findFreePort(2200)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: find free SSH port: %w", err)
	}
	k3sPort, err := findFreePort(16443)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: find free k3s API port: %w", err)
	}

	// Pick a free UDP port for the multicast cluster network.
	mcastPort, err := findFreeUDPPort(55500)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: find free UDP port: %w", err)
	}

	// Step 6: save ports.
	portFile := filepath.Join(stateDir, "ssh.port")
	if err := os.WriteFile(portFile, []byte(strconv.Itoa(sshPort)), 0o600); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: write ssh.port: %w", err)
	}

	// Step 7: launch QEMU as orphan.
	_, _ = fmt.Fprintf(out, "[5/7] Launching QEMU (SSH→localhost:%d, k3s API→localhost:%d)...\n", sshPort, k3sPort)
	logPath := filepath.Join(stateDir, "qemu.log")
	pid, err := launchQEMU(diskPath, seedPath, logPath, sshPort, k3sPort, 0, mcastPort)
	if err != nil {
		return provision.ProvisionResult{}, err
	}

	// Step 8: save PID.
	pidFile := filepath.Join(stateDir, "vm.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: write vm.pid: %w", err)
	}

	// Step 9: poll SSH until ready; stream QEMU console log alongside.
	_, _ = fmt.Fprintf(out, "[6/7] Waiting for VM SSH on localhost:%d (up to 10 min)...\n", sshPort)
	streamCtx, stopStream := context.WithCancel(ctx)
	go streamLog(streamCtx, logPath, out)
	sshErr := waitForSSH(ctx, sshPort, 10*time.Minute, sshKeyPath, out)
	stopStream()
	if sshErr != nil {
		return provision.ProvisionResult{}, sshErr
	}

	// Step 10: install k3s on the control-plane via SSH (no external tools needed).
	_, _ = fmt.Fprintln(out, "[7/7] Installing k3s on control-plane...")
	if err := p.runBootstrap(ctx, sshPort, k3sPort, sshKeyPath, kubeconfigPath); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: bootstrap: %w", err)
	}

	// Step 11: persist cluster state for future add-node calls.
	cs := &clusterState{
		McastPort:     mcastPort,
		CPSSHPort:     sshPort,
		CPK3sPort:     k3sPort,
		CPClusterIP:   "10.100.0.1",
		NextWorkerIdx: 1,
	}
	if err := saveClusterState(stateDir, cs); err != nil {
		return provision.ProvisionResult{}, err
	}

	provisionOK = true
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

// AddNode provisions a worker VM and joins it to an existing k3s cluster.
// It returns the worker node name on success.
func (p *Provider) AddNode(ctx context.Context, clusterName string) (nodeName string, err error) {
	out := p.out()

	// Step 1: load cluster state.
	stateDir, err := p.stateDir(clusterName)
	if err != nil {
		return "", err
	}
	cs, err := loadClusterState(stateDir)
	if err != nil {
		return "", err
	}

	// Step 2: allocate worker index, increment and save immediately.
	workerIdx := cs.NextWorkerIdx
	cs.NextWorkerIdx++
	if err := saveClusterState(stateDir, cs); err != nil {
		return "", err
	}

	workerName := fmt.Sprintf("%s-worker-%d", clusterName, workerIdx)
	workerClusterIP := fmt.Sprintf("10.100.0.%d/24", workerIdx+1)
	workerClusterIPBare := fmt.Sprintf("10.100.0.%d", workerIdx+1)

	_, _ = fmt.Fprintf(out, "qemu: adding worker %q (cluster IP %s)...\n", workerName, workerClusterIPBare)

	// Step 3: resolve cache dir and download base image (idempotent).
	cacheDir, err := p.cacheDir()
	if err != nil {
		return "", err
	}
	baseImage, err := EnsureBaseImage(ctx, cacheDir, out)
	if err != nil {
		return "", err
	}

	// Step 4: create worker state dir.
	workerDir := filepath.Join(stateDir, "workers", workerName)
	if err := os.MkdirAll(workerDir, 0o700); err != nil {
		return "", fmt.Errorf("qemu: mkdir worker dir: %w", err)
	}

	// Step 5: create overlay disk in worker state dir.
	diskPath := filepath.Join(workerDir, "disk.qcow2")
	if err := createOverlayDisk(baseImage, diskPath); err != nil {
		return "", err
	}

	// Step 6: generate cloud-init seed for worker.
	sshKeyPath, err := p.sshKeyPath()
	if err != nil {
		return "", err
	}
	sshPubKeyPath := sshKeyPath + ".pub"
	sshPubKey, err := os.ReadFile(sshPubKeyPath)
	if err != nil {
		return "", fmt.Errorf("qemu: read SSH public key %s: %w", sshPubKeyPath, err)
	}

	ciDir, err := os.MkdirTemp("", "qemu-worker-cloudinit-*")
	if err != nil {
		return "", fmt.Errorf("qemu: create cloud-init temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(ciDir) }()

	if err := WriteCloudInitFiles(ciDir, workerName, strings.TrimSpace(string(sshPubKey)), workerIdx, workerClusterIP); err != nil {
		return "", err
	}
	seedPath := filepath.Join(workerDir, "seed.iso")
	if err := MakeSeedISO(ciDir, seedPath); err != nil {
		return "", err
	}

	// Step 7: find free SSH port for worker.
	workerSSHPort, err := findFreePort(2300)
	if err != nil {
		return "", fmt.Errorf("qemu: find free SSH port for worker: %w", err)
	}
	portFile := filepath.Join(workerDir, "ssh.port")
	if err := os.WriteFile(portFile, []byte(strconv.Itoa(workerSSHPort)), 0o600); err != nil {
		return "", fmt.Errorf("qemu: write worker ssh.port: %w", err)
	}

	// Step 8: launch QEMU for worker (same multicast port as cluster; no k3s API exposure).
	_, _ = fmt.Fprintf(out, "qemu: launching worker VM (SSH forwarded to localhost:%d)...\n", workerSSHPort)
	logPath := filepath.Join(workerDir, "qemu.log")
	pid, err := launchQEMU(diskPath, seedPath, logPath, workerSSHPort, 0, workerIdx, cs.McastPort)
	if err != nil {
		return "", err
	}

	// Step 9: save worker PID.
	pidFile := filepath.Join(workerDir, "vm.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		return "", fmt.Errorf("qemu: write worker vm.pid: %w", err)
	}

	// Step 10: wait for worker SSH.
	_, _ = fmt.Fprintf(out, "qemu: waiting for worker SSH on localhost:%d (up to 10 min)...\n", workerSSHPort)
	if err := waitForSSH(ctx, workerSSHPort, 10*time.Minute, sshKeyPath, out); err != nil {
		return "", err
	}

	// Step 11: get node-token from control-plane.
	_, _ = fmt.Fprintln(out, "qemu: fetching node-token from control-plane...")
	token, err := sshRun(ctx, cs.CPSSHPort, sshKeyPath, "sudo cat /var/lib/rancher/k3s/server/node-token")
	if err != nil {
		return "", fmt.Errorf("qemu: get node-token: %w", err)
	}
	token = strings.TrimSpace(token)

	// Step 12: install k3s agent on worker.
	_, _ = fmt.Fprintf(out, "qemu: installing k3s agent on worker %q...\n", workerName)
	installCmd := fmt.Sprintf(
		"curl -sfL https://get.k3s.io | K3S_URL=https://10.100.0.1:6443 K3S_TOKEN=%s INSTALL_K3S_EXEC=\"agent --node-ip %s\" sh -",
		token, workerClusterIPBare,
	)
	if _, err := sshRun(ctx, workerSSHPort, sshKeyPath, installCmd); err != nil {
		return "", fmt.Errorf("qemu: install k3s agent: %w", err)
	}

	_, _ = fmt.Fprintf(out, "qemu: worker %q joined cluster %q\n", workerName, clusterName)
	return workerName, nil
}

// Destroy stops the QEMU VM and removes the state directory.
func (p *Provider) Destroy(_ context.Context, cluster registry.Cluster) error {
	stateDir, err := p.stateDir(cluster.Name)
	if err != nil {
		return err
	}

	// Kill all worker VMs first.
	workersDir := filepath.Join(stateDir, "workers")
	if entries, err := os.ReadDir(workersDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			workerPIDFile := filepath.Join(workersDir, entry.Name(), "vm.pid")
			if pidBytes, err := os.ReadFile(workerPIDFile); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes))); err == nil {
					if proc, err := os.FindProcess(pid); err == nil {
						_ = proc.Kill()
					}
				}
			}
		}
	}

	// Read control-plane PID and kill the process.
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

// runBootstrap installs k3s on the control-plane VM via SSH and writes the
// kubeconfig to kubeconfigPath. No external tools (k3sup, helm, etc.) are
// required — only standard SSH access to the VM.
//
// The k3s server is configured with:
//   - --node-ip 10.100.0.1  so workers can reach it on the cluster network
//   - --tls-san 127.0.0.1   so the cert is valid for the host-side port-forward
//
// The kubeconfig is fetched via SSH and the server URL is rewritten from the
// VM-local address to localhost:<k3sPort> so kubectl works from the host.
func (p *Provider) runBootstrap(ctx context.Context, sshPort, k3sPort int, sshKeyPath, kubeconfigPath string) error {
	// Injectable override for tests.
	if p.deps.Bootstrap != nil {
		return p.deps.Bootstrap(ctx, sshPort, k3sPort, sshKeyPath, kubeconfigPath)
	}

	// Step 1: install k3s server on the VM.
	installCmd := `curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="server --node-ip 10.100.0.1 --tls-san 127.0.0.1" sh -`
	if _, err := sshRun(ctx, sshPort, sshKeyPath, installCmd); err != nil {
		return fmt.Errorf("k3s install: %w", err)
	}

	// Step 2: fetch the kubeconfig from the VM.
	raw, err := sshRun(ctx, sshPort, sshKeyPath, "sudo cat /etc/rancher/k3s/k3s.yaml")
	if err != nil {
		return fmt.Errorf("fetch kubeconfig: %w", err)
	}

	// Step 3: rewrite the server URL to use the host-side port-forward.
	kubeconfig := strings.ReplaceAll(raw, "https://127.0.0.1:6443", fmt.Sprintf("https://127.0.0.1:%d", k3sPort))

	// Step 4: write to local path.
	if err := os.MkdirAll(filepath.Dir(kubeconfigPath), 0o700); err != nil {
		return fmt.Errorf("mkdir kubeconfig dir: %w", err)
	}
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfig), 0o600); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}
	return nil
}

// sshRun executes command on the VM reachable at 127.0.0.1:<port> via SSH.
// It returns the combined stdout as a string.
func sshRun(ctx context.Context, port int, sshKeyPath, command string) (string, error) {
	args := []string{
		"-p", strconv.Itoa(port),
		"-i", sshKeyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
		// Keep the session alive during long-running commands (e.g. k3s install).
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=20",
		"ubuntu@127.0.0.1",
		command,
	}
	cmd := exec.CommandContext(ctx, "ssh", args...) //nolint:gosec
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ssh run: %w", err)
	}
	return stdout.String(), nil
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
// nodeIdx is the sequential node number (0=control-plane, 1=first worker, …).
// k3sPort is the host-side TCP port forwarded to the VM's k3s API (6443);
// pass 0 for worker VMs (they don't expose the API to the host).
// mcastPort is the UDP multicast port shared by all nodes in the cluster.
func launchQEMU(diskPath, seedPath, logPath string, sshPort, k3sPort, nodeIdx, mcastPort int) (int, error) {
	qemuBin, args := buildQEMUArgs(diskPath, seedPath, sshPort, k3sPort, nodeIdx, mcastPort)

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
//
// Each VM gets two network interfaces:
//   - net0: user networking with SSH port-forward and (for control-plane) k3s API port-forward
//   - net1: multicast socket L2 network for VM-to-VM connectivity
//
// Deterministic MACs are derived from nodeIdx so cloud-init network-config
// can assign static IPs via MAC matching.
// k3sPort > 0 adds a hostfwd for the k3s API (control-plane only); workers pass 0.
func buildQEMUArgs(diskPath, seedPath string, sshPort, k3sPort, nodeIdx, mcastPort int) (string, []string) {
	arch := runtime.GOARCH

	net0MAC := fmt.Sprintf("52:54:00:01:00:%02x", nodeIdx)
	net1MAC := fmt.Sprintf("52:54:00:02:00:%02x", nodeIdx)

	net0Netdev := fmt.Sprintf("user,id=net0,hostfwd=tcp::%d-:22", sshPort)
	if k3sPort > 0 {
		net0Netdev += fmt.Sprintf(",hostfwd=tcp::%d-:6443", k3sPort)
	}
	net1Netdev := fmt.Sprintf("socket,id=net1,mcast=230.0.0.1:%d", mcastPort)

	commonNetArgs := []string{
		"-netdev", net0Netdev,
		"-device", fmt.Sprintf("virtio-net-pci,netdev=net0,mac=%s", net0MAC),
		"-netdev", net1Netdev,
		"-device", fmt.Sprintf("virtio-net-pci,netdev=net1,mac=%s", net1MAC),
	}

	// accel picks the hardware virtualization backend for the current OS.
	// -cpu host requires acceleration; without it QEMU rejects the model.
	accel := "tcg" // pure software fallback
	switch runtime.GOOS {
	case "darwin":
		accel = "hvf" // macOS Hypervisor.framework
	case "linux":
		accel = "kvm"
	}

	switch arch {
	case "arm64":
		qemuBin := "qemu-system-aarch64"
		args := []string{
			"-m", "2048",
			"-smp", "2",
			"-nographic",
			"-no-reboot",
			"-machine", "virt,accel=" + accel,
			"-cpu", "host",
			"-drive", "file=" + diskPath + ",format=qcow2,if=virtio",
			"-drive", "file=" + seedPath + ",format=raw,if=virtio",
		}
		args = append(args, commonNetArgs...)
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
			"-accel", accel,
			"-drive", "file=" + diskPath + ",format=qcow2,if=virtio",
			"-drive", "file=" + seedPath + ",format=raw,if=virtio",
		}
		args = append(args, commonNetArgs...)
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

// waitForSSH polls until a real SSH login succeeds or the timeout expires.
// Testing authentication (not just TCP) ensures cloud-init has finished
// writing authorized_keys before we proceed with provisioning commands.
func waitForSSH(ctx context.Context, port int, timeout time.Duration, sshKeyPath string, out io.Writer) error {
	deadline := time.Now().Add(timeout)
	ctx2, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		if _, err := sshRun(ctx2, port, sshKeyPath, "true"); err == nil {
			_, _ = fmt.Fprintf(out, "qemu: VM SSH is ready on 127.0.0.1:%d\n", port)
			return nil
		}
		_, _ = fmt.Fprintf(out, "qemu: waiting for VM SSH on 127.0.0.1:%d...\n", port)
		select {
		case <-ctx2.Done():
			return fmt.Errorf("qemu: timed out waiting for SSH on 127.0.0.1:%d after %s", port, timeout)
		case <-ticker.C:
		}
	}
}

// streamLog tails path and writes new lines to out until ctx is cancelled.
// Lines are prefixed with "[vm] " so they're visually distinct from clusterbox output.
func streamLog(ctx context.Context, path string, out io.Writer) {
	// Wait for the log file to appear (QEMU may not have created it yet).
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}

	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, 4096)
	var partial string
	for {
		n, _ := f.Read(buf)
		if n > 0 {
			chunk := partial + string(buf[:n])
			lines := strings.Split(chunk, "\n")
			// Last element may be incomplete; carry it forward.
			partial = lines[len(lines)-1]
			for _, line := range lines[:len(lines)-1] {
				if line != "" {
					_, _ = fmt.Fprintf(out, "[vm] %s\n", line)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Compile-time check: *Provider satisfies provision.Provider.
var _ provision.Provider = (*Provider)(nil)
