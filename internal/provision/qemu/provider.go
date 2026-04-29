// Package qemu implements a local provision.Provider that boots an Ubuntu
// 22.04 VM via QEMU to exercise the full provisioning flow (cloud-init,
// k3sup, k3s) without a cloud account.
//
// This is a dev/test tool, not production infrastructure.
package qemu

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/foundryfabric/clusterbox/internal/agentbundle"
	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/provision/nodeinstall"
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

	// Bootstrap is called instead of the built-in clusterboxnode bootstrap when
	// set. Tests inject a no-op here to skip real SSH calls.
	// Returns the node-token so Provision can persist it for worker joins.
	Bootstrap func(ctx context.Context, sshPort, k3sPort int, sshKeyPath, kubeconfigPath string) (string, error)

	// AgentBundleForArch returns the embedded clusterboxnode binary bytes for
	// the given linux arch. Defaults to agentbundle.ForArch.
	AgentBundleForArch func(arch string) ([]byte, error)
}

// Provider is the QEMU implementation of provision.Provider.
type Provider struct {
	deps Deps
	// mu serializes the cluster-state read-modify-write inside AddNode so
	// concurrent workers don't race on NextWorkerIdx / cluster.json.
	mu sync.Mutex
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

	// Kill any VM left over from a previous failed run before touching ports.
	killVMByPIDFile(filepath.Join(stateDir, "vm.pid"))
	// Remove stale CP disk so k3s is always freshly installed with the current
	// TLS SANs. Without this, a re-run reuses the old disk, k3s reports "already
	// installed", and the cert is missing the 10.0.2.2 SAN that workers need.
	_ = os.Remove(filepath.Join(stateDir, "disk.qcow2"))

	// On failure: kill the newly launched VM (if any) and remove partial state.
	// Log tail is printed so the error is visible even after the terminal scrolls.
	var launchedPID int
	provisionOK := false
	defer func() {
		if !provisionOK {
			if launchedPID > 0 {
				killPID(launchedPID)
			}
			if logPath := filepath.Join(stateDir, "qemu.log"); fileExists(logPath) {
				_, _ = fmt.Fprintln(out, "\n--- VM log (last 20 lines) ---")
				printLogTail(logPath, 20, out)
				_, _ = fmt.Fprintln(out, "--- end VM log ---")
			}
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

	// Build the control-plane spec for cloud-init write_files and runBootstrap.
	// 10.0.2.2 is the QEMU user-net gateway as seen from inside any VM (SLIRP).
	// Workers connect to https://10.0.2.2:<cpK3sPort>, which SLIRP routes to
	// 127.0.0.1:<cpK3sPort> on the host — the CP's host-side k3s port forward.
	cpSpec := &config.Spec{
		Hostname: "cp",
		K3s: &config.K3sSpec{
			Enabled: true,
			Role:    "server-init",
			Version: bootstrap.DefaultK3sVersion,
			NodeIP:  "10.100.0.1",
			TLSSANs: []string{"127.0.0.1", "10.0.2.2"},
		},
	}
	cpSpecYAML, err := yaml.Marshal(cpSpec)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: marshal spec: %w", err)
	}
	cpConfigB64 := base64.StdEncoding.EncodeToString(cpSpecYAML)

	// Control-plane is always nodeIdx=0, cluster IP 10.100.0.1/24.
	const cpClusterIP = "10.100.0.1/24"
	if err := WriteCloudInitFiles(ciDir, clusterName, strings.TrimSpace(string(sshPubKey)), cpConfigB64, 0, cpClusterIP); err != nil {
		return provision.ProvisionResult{}, err
	}
	if err := MakeSeedISO(ciDir, seedPath); err != nil {
		return provision.ProvisionResult{}, err
	}

	// Step 5: pick free TCP ports for SSH and the k3s API.
	// Residual TOCTOU: there is a window between findFreePort returning and QEMU
	// binding where another process could claim the port. Provision is typically
	// run once per cluster, so the race is extremely unlikely in practice; the
	// mutex in AddNode closes the same gap for concurrent worker adds.
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
	launchedPID = pid

	// Step 8: save PID.
	pidFile := filepath.Join(stateDir, "vm.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: write vm.pid: %w", err)
	}

	// Give QEMU 600 ms to start and attempt port binding, then check the log
	// for hostfwd failures (e.g. port already in use). Without this check the
	// cluster boots without a working k3s API port forward and workers silently
	// fail to join.
	time.Sleep(600 * time.Millisecond)
	if err := checkQEMULogForErrors(logPath); err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: %w", err)
	}

	// Step 9+10: stream VM log for the entire boot+install sequence so the
	// operator can see what is happening without a separate terminal.
	streamCtx, stopStream := context.WithCancel(ctx)
	go streamLog(streamCtx, logPath, out)
	defer stopStream()

	_, _ = fmt.Fprintf(out, "[6/7] Waiting for VM SSH on localhost:%d (up to 10 min)...\n", sshPort)
	if err := waitForSSH(ctx, sshPort, 10*time.Minute, sshKeyPath, out); err != nil {
		return provision.ProvisionResult{}, err
	}

	// Step 10: install k3s on the control-plane via clusterboxnode.
	_, _ = fmt.Fprintln(out, "[7/7] Installing k3s on control-plane via clusterboxnode...")
	nodeToken, err := p.runBootstrap(ctx, sshPort, k3sPort, sshKeyPath, kubeconfigPath)
	if err != nil {
		return provision.ProvisionResult{}, fmt.Errorf("qemu: bootstrap: %w", err)
	}

	// Step 11: persist cluster state for future add-node calls.
	cs := &clusterState{
		McastPort:     mcastPort,
		CPSSHPort:     sshPort,
		CPK3sPort:     k3sPort,
		CPClusterIP:   "10.100.0.1",
		NextWorkerIdx: 1,
		NodeToken:     nodeToken,
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
//
// Concurrency: multiple AddNode calls may run in parallel (e.g. `up --nodes N`).
// A single mutex section keeps concurrent workers correct: it atomically
// claims the worker index, increments NextWorkerIdx, and pre-selects a free
// SSH port via OS-assigned random binding. Port selection inside the mutex
// prevents concurrent workers from racing to bind the same port. All slow I/O
// (disk creation, cloud-init, ISO generation, QEMU launch) runs after the
// mutex releases, using the correctly-allocated index and port from the mutex.
func (p *Provider) AddNode(ctx context.Context, clusterName string) (nodeName string, err error) {
	out := p.out()

	stateDir, err := p.stateDir(clusterName)
	if err != nil {
		return "", err
	}

	// Mutex: atomically claim a worker index and pre-select a free SSH port.
	// Serialising port selection prevents concurrent workers from picking the
	// same port. pickFreePort uses OS-assigned random binding (:0) to avoid
	// sequential scanning races.
	var workerSSHPort int
	p.mu.Lock()
	cs, err := loadClusterState(stateDir)
	if err != nil {
		p.mu.Unlock()
		return "", err
	}
	workerIdx := cs.NextWorkerIdx
	cs.NextWorkerIdx++
	workerSSHPort, err = pickFreePort()
	if err != nil {
		p.mu.Unlock()
		return "", fmt.Errorf("qemu: pick SSH port for worker: %w", err)
	}
	if err := saveClusterState(stateDir, cs); err != nil {
		p.mu.Unlock()
		return "", err
	}
	p.mu.Unlock()

	workerName := fmt.Sprintf("%s-worker-%d", clusterName, workerIdx)
	workerClusterIP := fmt.Sprintf("10.100.0.%d/24", workerIdx+1)
	workerClusterIPBare := fmt.Sprintf("10.100.0.%d", workerIdx+1)

	_, _ = fmt.Fprintf(out, "qemu: adding worker %q (cluster IP %s)...\n", workerName, workerClusterIPBare)

	// Step 2: resolve cache dir and download base image (idempotent).
	cacheDir, err := p.cacheDir()
	if err != nil {
		return "", err
	}
	baseImage, err := EnsureBaseImage(ctx, cacheDir, out)
	if err != nil {
		return "", err
	}

	// Step 3: create worker state dir.
	workerDir := filepath.Join(stateDir, "workers", workerName)
	if err := os.MkdirAll(workerDir, 0o700); err != nil {
		return "", fmt.Errorf("qemu: mkdir worker dir: %w", err)
	}

	// On failure: kill the worker VM (if launched) and clean up the worker
	// state directory so a re-run doesn't trip over stale state.
	var workerPID int
	workerOK := false
	defer func() {
		if !workerOK {
			if workerPID > 0 {
				killPIDGracefully(workerPID)
			}
			_ = os.RemoveAll(workerDir)
		}
	}()

	// Step 4: create overlay disk (slow; uses correctly-allocated workerIdx).
	diskPath := filepath.Join(workerDir, "disk.qcow2")
	if err := createOverlayDisk(baseImage, diskPath); err != nil {
		return "", err
	}

	// Step 5: generate cloud-init seed for worker.
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

	workerMinSpec := &config.Spec{Hostname: workerName}
	workerMinYAML, err := yaml.Marshal(workerMinSpec)
	if err != nil {
		return "", fmt.Errorf("qemu: marshal worker spec: %w", err)
	}
	workerConfigB64 := base64.StdEncoding.EncodeToString(workerMinYAML)
	if err := WriteCloudInitFiles(ciDir, workerName, strings.TrimSpace(string(sshPubKey)), workerConfigB64, workerIdx, workerClusterIP); err != nil {
		return "", err
	}
	seedPath := filepath.Join(workerDir, "seed.iso")
	if err := MakeSeedISO(ciDir, seedPath); err != nil {
		return "", err
	}

	portFile := filepath.Join(workerDir, "ssh.port")
	if err := os.WriteFile(portFile, []byte(strconv.Itoa(workerSSHPort)), 0o600); err != nil {
		return "", fmt.Errorf("qemu: write worker ssh.port: %w", err)
	}

	logPath := filepath.Join(workerDir, "qemu.log")
	_, _ = fmt.Fprintf(out, "qemu: launching worker VM (SSH forwarded to localhost:%d)...\n", workerSSHPort)
	workerPID, err = launchQEMU(diskPath, seedPath, logPath, workerSSHPort, 0, workerIdx, cs.McastPort)
	if err != nil {
		return "", err
	}

	// Give QEMU 600 ms to bind the port, then check the log for hostfwd failures.
	time.Sleep(600 * time.Millisecond)
	if err := checkQEMULogForErrors(logPath); err != nil {
		return "", fmt.Errorf("qemu worker: %w", err)
	}

	// Step 7: save worker PID (outside mutex; no concurrent access to workerDir).
	pidFile := filepath.Join(workerDir, "vm.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(workerPID)), 0o600); err != nil {
		return "", fmt.Errorf("qemu: write worker vm.pid: %w", err)
	}

	// Step 8: wait for worker SSH.
	_, _ = fmt.Fprintf(out, "qemu: waiting for worker SSH on localhost:%d (up to 10 min)...\n", workerSSHPort)
	if err := waitForSSH(ctx, workerSSHPort, 10*time.Minute, sshKeyPath, out); err != nil {
		return "", err
	}

	// Step 9: get node-token (from persisted state, or fall back to SSH).
	token := cs.NodeToken
	if token == "" {
		_, _ = fmt.Fprintln(out, "qemu: fetching node-token from control-plane (fallback)...")
		token, err = nodeinstall.SSHRun(ctx, vmSSH(cs.CPSSHPort, sshKeyPath), "sudo cat /var/lib/rancher/k3s/server/node-token")
		if err != nil {
			return "", fmt.Errorf("qemu: get node-token: %w", err)
		}
		token = strings.TrimSpace(token)
	}

	// Step 10: install k3s agent on worker via clusterboxnode.
	_, _ = fmt.Fprintf(out, "qemu: installing k3s agent on worker %q via clusterboxnode...\n", workerName)
	if err := p.runAgentBootstrap(ctx, workerSSHPort, sshKeyPath, workerClusterIPBare, token, cs.CPK3sPort); err != nil {
		return "", fmt.Errorf("qemu: agent bootstrap: %w", err)
	}

	workerOK = true
	_, _ = fmt.Fprintf(out, "qemu: worker %q joined cluster %q\n", workerName, clusterName)
	return workerName, nil
}

// RemoveNode kills the worker VM for nodeName and removes its state directory.
// It is called by the cmd layer after kubectl drain + delete have completed.
//
// The function is idempotent: if the PID file is absent or the process is
// already gone it returns nil rather than an error.
func (p *Provider) RemoveNode(_ context.Context, clusterName, nodeName string) error {
	stateDir, err := p.stateDir(clusterName)
	if err != nil {
		return err
	}
	workerDir := filepath.Join(stateDir, "workers", nodeName)

	pidFile := filepath.Join(workerDir, "vm.pid")
	if pidBytes, err := os.ReadFile(pidFile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes))); err == nil {
			killPIDGracefully(pid)
		}
	}

	if err := os.RemoveAll(workerDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("qemu: remove worker dir: %w", err)
	}
	return nil
}

// Destroy stops the QEMU VM and removes the state directory.
func (p *Provider) Destroy(_ context.Context, cluster registry.Cluster) error {
	stateDir, err := p.stateDir(cluster.Name)
	if err != nil {
		return err
	}

	// Kill all worker VMs first, with SIGTERM → wait → SIGKILL grace so QEMU
	// can flush write-back caches before the disk image is closed.
	workersDir := filepath.Join(stateDir, "workers")
	if entries, err := os.ReadDir(workersDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			workerDir := filepath.Join(workersDir, entry.Name())
			workerPIDFile := filepath.Join(workerDir, "vm.pid")
			if pidBytes, err := os.ReadFile(workerPIDFile); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes))); err == nil {
					killPIDGracefully(pid)
				}
			} else {
				// PID file missing (e.g. Provision failed mid-way); fall back to
				// killing by hostfwd port so orphaned QEMU processes don't leak.
				if portBytes, err := os.ReadFile(filepath.Join(workerDir, "ssh.port")); err == nil {
					if port, err := strconv.Atoi(strings.TrimSpace(string(portBytes))); err == nil {
						killQEMUByHostFwdPort(port)
					}
				}
			}
		}
	}

	// Read control-plane PID and kill the process.
	pidFile := filepath.Join(stateDir, "vm.pid")
	pidBytes, err := os.ReadFile(pidFile)
	if err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes))); err == nil {
			killPIDGracefully(pid)
		}
	} else {
		// PID file missing; fall back to killing by SSH port forward.
		if portBytes, err := os.ReadFile(filepath.Join(stateDir, "ssh.port")); err == nil {
			if port, err := strconv.Atoi(strings.TrimSpace(string(portBytes))); err == nil {
				killQEMUByHostFwdPort(port)
			}
		}
	}

	// Remove kubeconfig file (best-effort).
	if kubeconfigPath, err := p.kubeconfigPath(cluster.Name); err == nil {
		_ = os.Remove(kubeconfigPath)
	}

	// Remove state directory entirely.
	if err := os.RemoveAll(stateDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("qemu: remove state dir: %w", err)
	}
	return nil
}

// Reconcile checks whether the QEMU control-plane VM is still running and
// surveys any worker VMs in the workers/ subdirectory.
func (p *Provider) Reconcile(_ context.Context, clusterName string) (provision.ReconcileSummary, error) {
	stateDir, err := p.stateDir(clusterName)
	if err != nil {
		return provision.ReconcileSummary{}, nil //nolint:nilerr
	}

	var summary provision.ReconcileSummary

	// Control-plane.
	if vmAlive(filepath.Join(stateDir, "vm.pid")) {
		summary.Existing++
	} else {
		summary.MarkedDestroyed++
	}

	// Workers: walk the workers/ subdirectory and check each PID file.
	workersDir := filepath.Join(stateDir, "workers")
	entries, err := os.ReadDir(workersDir)
	if err != nil && !os.IsNotExist(err) {
		return summary, fmt.Errorf("qemu reconcile: read workers dir: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pidFile := filepath.Join(workersDir, entry.Name(), "vm.pid")
		if vmAlive(pidFile) {
			summary.Existing++
		} else {
			summary.MarkedDestroyed++
		}
	}

	return summary, nil
}

// vmAlive returns true when the PID recorded in pidFile corresponds to a
// running process. It returns false if the file is absent, unparseable, or
// the process has already exited.
func vmAlive(pidFile string) bool {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal(0) checks if the process is alive without sending a real signal.
	return proc.Signal(syscall.Signal(0)) == nil
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

// vmSSH returns the nodeinstall.SSHConfig for connecting to a VM whose SSH
// port is forwarded to localhost:<sshPort>.
func vmSSH(sshPort int, sshKeyPath string) nodeinstall.SSHConfig {
	return nodeinstall.SSHConfig{
		Host:    "127.0.0.1",
		Port:    sshPort,
		User:    "ubuntu",
		KeyPath: sshKeyPath,
	}
}

// runBootstrap installs k3s on the control-plane VM via clusterboxnode and
// writes the kubeconfig to kubeconfigPath.
//
// Returns the node-token so Provision can persist it in clusterState for
// worker joins.
func (p *Provider) runBootstrap(ctx context.Context, sshPort, k3sPort int, sshKeyPath, kubeconfigPath string) (string, error) {
	if p.deps.Bootstrap != nil {
		return p.deps.Bootstrap(ctx, sshPort, k3sPort, sshKeyPath, kubeconfigPath)
	}

	out := p.out()
	cfg := vmSSH(sshPort, sshKeyPath)

	loader := p.deps.AgentBundleForArch
	if loader == nil {
		loader = agentbundle.ForArch
	}

	spec := &config.Spec{
		Hostname: "cp",
		K3s: &config.K3sSpec{
			Enabled: true,
			Role:    "server-init",
			Version: bootstrap.DefaultK3sVersion,
			NodeIP:  "10.100.0.1",
			TLSSANs: []string{"127.0.0.1", "10.0.2.2"},
		},
	}
	specYAML, err := yaml.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("qemu: marshal spec: %w", err)
	}

	result, err := nodeinstall.RunNodeAgent(ctx, cfg, specYAML, loader, out)
	if err != nil {
		return "", err
	}
	if result.KubeconfigYAML == "" {
		return "", fmt.Errorf("qemu: install output missing kubeconfig_yaml")
	}

	kubeconfig, err := rewriteQEMUKubeconfig(result.KubeconfigYAML, k3sPort)
	if err != nil {
		return "", fmt.Errorf("qemu: rewrite kubeconfig server: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(kubeconfigPath), 0o700); err != nil {
		return "", fmt.Errorf("qemu: mkdir kubeconfig dir: %w", err)
	}
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfig), 0o600); err != nil {
		return "", fmt.Errorf("qemu: write kubeconfig: %w", err)
	}
	return result.NodeToken, nil
}

// runAgentBootstrap installs k3s in agent mode on a worker VM via clusterboxnode.
//
// Workers join via the QEMU user-net gateway: 10.0.2.2:<cpK3sPort> inside the
// VM is routed by SLIRP to 127.0.0.1:<cpK3sPort> on the host, which QEMU
// forwards to the CP VM's port 6443. This avoids the multicast socket
// network (net1) which does not carry traffic between VMs on macOS.
func (p *Provider) runAgentBootstrap(ctx context.Context, sshPort int, sshKeyPath, nodeIP, token string, cpK3sPort int) error {
	out := p.out()
	cfg := vmSSH(sshPort, sshKeyPath)

	if debugEnabled() {
		_, _ = fmt.Fprintf(out, "[debug] probing CP reachability at 10.0.2.2:%d...\n", cpK3sPort)
		probe, _ := nodeinstall.SSHRun(ctx, cfg, fmt.Sprintf("curl -sk --max-time 5 https://10.0.2.2:%d/version 2>&1 || echo UNREACHABLE", cpK3sPort))
		_, _ = fmt.Fprintf(out, "[debug] CP probe: %s\n", strings.TrimSpace(probe))
	}

	loader := p.deps.AgentBundleForArch
	if loader == nil {
		loader = agentbundle.ForArch
	}

	spec := &config.Spec{
		K3s: &config.K3sSpec{
			Enabled:    true,
			Role:       "agent",
			Version:    bootstrap.DefaultK3sVersion,
			NodeIP:     nodeIP,
			ServerURL:  fmt.Sprintf("https://10.0.2.2:%d", cpK3sPort),
			Token:      token,
			NodeLabels: []string{"node.kubernetes.io/worker=true"},
		},
	}
	specYAML, err := yaml.Marshal(spec)
	if err != nil {
		return fmt.Errorf("qemu: marshal spec: %w", err)
	}

	if _, err := nodeinstall.RunNodeAgent(ctx, cfg, specYAML, loader, out); err != nil {
		return err
	}
	return nil
}

// checkQEMULogForErrors scans the QEMU log for fatal startup errors such as
// port-binding failures. Called shortly after launchQEMU so provisioning fails
// fast instead of waiting for the SSH timeout.
func checkQEMULogForErrors(logPath string) error {
	data, _ := os.ReadFile(logPath)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "Could not set up host forwarding rule") {
			return fmt.Errorf("port bind failed (another cluster may be using this port — run 'clusterbox destroy' first): %s", strings.TrimSpace(line))
		}
	}
	return nil
}

// debugEnabled reports whether CLUSTERBOX_DEBUG is set to a non-empty,
// non-"0" value. When true, providers emit verbose pre-install diagnostics.
func debugEnabled() bool {
	v := os.Getenv("CLUSTERBOX_DEBUG")
	return v != "" && v != "0"
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
// The process is NOT waited on — it outlives the CLI because the parent
// exiting does not kill children on Unix/macOS. We avoid Release() so that
// the PID remains usable for killPID on cleanup.
func launchQEMU(diskPath, seedPath, logPath string, sshPort, k3sPort, nodeIdx, mcastPort int) (int, error) {
	qemuBin, args := buildQEMUArgs(diskPath, seedPath, sshPort, k3sPort, nodeIdx, mcastPort)

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("qemu: open log file: %w", err)
	}

	cmd := exec.Command(qemuBin, args...) //nolint:gosec
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Ensure the child doesn't inherit our process group so it survives
	// when the CLI exits.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return 0, fmt.Errorf("qemu: start VM: %w", err)
	}
	// Close our copy of the log file handle; QEMU keeps its own.
	_ = logFile.Close()

	return cmd.Process.Pid, nil
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

// pickFreePort asks the OS for a random free TCP port by binding to :0 and
// reading back the assigned port. Used by AddNode inside the mutex so
// concurrent workers never select the same port.
func pickFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("pick free port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

// waitForSSH polls until a real SSH login succeeds or the timeout expires.
func waitForSSH(ctx context.Context, port int, timeout time.Duration, sshKeyPath string, out io.Writer) error {
	return nodeinstall.WaitForSSH(ctx, vmSSH(port, sshKeyPath), timeout, out)
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

// killPID sends SIGKILL to the given PID via the shell, which is more
// reliable than os.FindProcess+Kill on macOS after process orphaning.
func killPID(pid int) {
	_ = exec.Command("kill", "-9", strconv.Itoa(pid)).Run() //nolint:gosec
}

// killPIDGracefully sends SIGTERM and waits up to 5 s for the process to exit
// before falling back to SIGKILL. This gives QEMU a chance to flush its
// write-back cache before the disk image is forcibly detached.
func killPIDGracefully(pid int) {
	_ = exec.Command("kill", "-15", strconv.Itoa(pid)).Run() //nolint:gosec
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := exec.Command("kill", "-0", strconv.Itoa(pid)).Run(); err != nil { //nolint:gosec
			return // process exited
		}
		time.Sleep(200 * time.Millisecond)
	}
	killPID(pid)
}

// killQEMUByHostFwdPort finds a QEMU process whose command line includes a
// hostfwd mapping for port and kills it. Used as a fallback in Destroy when
// the vm.pid file is absent (e.g. Provision failed before writing it).
func killQEMUByHostFwdPort(port int) {
	if port <= 0 {
		return
	}
	pattern := fmt.Sprintf("hostfwd=tcp::%d-", port)
	out, err := exec.Command("pgrep", "-f", pattern).Output() //nolint:gosec
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && pid > 0 {
			killPIDGracefully(pid)
		}
	}
}

// killVMByPIDFile reads a vm.pid file and kills the process if it exists.
func killVMByPIDFile(pidFile string) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return
	}
	killPID(pid)
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// printLogTail writes the last n lines of a log file to out.
func printLogTail(path string, n int, out io.Writer) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, line := range lines {
		_, _ = fmt.Fprintln(out, line)
	}
}

// rewriteQEMUKubeconfig replaces every loopback server URL in the kubeconfig
// with https://127.0.0.1:<port> so the file remains usable via the host-side
// port forward rather than the VM's internal address. The replacement is done
// via YAML parsing rather than string replacement to avoid corrupting YAML
// structure (e.g. multi-context configs, comment lines).
func rewriteQEMUKubeconfig(in string, port int) (string, error) {
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(in), &n); err != nil {
		return "", fmt.Errorf("parse kubeconfig: %w", err)
	}
	rewriteLoopbackPort(&n, port)
	out, err := yaml.Marshal(&n)
	if err != nil {
		return "", fmt.Errorf("marshal kubeconfig: %w", err)
	}
	return string(out), nil
}

// rewriteLoopbackPort descends the YAML node tree and overwrites every
// "server" scalar whose URL uses a loopback address with the forwarded port.
func rewriteLoopbackPort(n *yaml.Node, port int) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			v := n.Content[i+1]
			if k.Value == "server" && v.Kind == yaml.ScalarNode {
				if strings.HasPrefix(v.Value, "https://127.0.0.1:") ||
					strings.HasPrefix(v.Value, "https://0.0.0.0:") {
					v.Value = fmt.Sprintf("https://127.0.0.1:%d", port)
				}
				continue
			}
			rewriteLoopbackPort(v, port)
		}
		return
	}
	for _, c := range n.Content {
		rewriteLoopbackPort(c, port)
	}
}

// Compile-time check: *Provider satisfies provision.Provider.
var _ provision.Provider = (*Provider)(nil)
