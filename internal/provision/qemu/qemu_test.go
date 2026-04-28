package qemu_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/provision/qemu"
	"github.com/foundryfabric/clusterbox/internal/registry"
)

// TestProviderName verifies the Name() method returns the expected constant.
func TestProviderName(t *testing.T) {
	p := qemu.New(qemu.Deps{})
	if got := p.Name(); got != "qemu" {
		t.Errorf("Name() = %q, want %q", got, "qemu")
	}
	if qemu.Name != "qemu" {
		t.Errorf("qemu.Name constant = %q, want %q", qemu.Name, "qemu")
	}
}

const (
	testConfigB64 = "aG9zdG5hbWU6IHRlc3QK" // base64("hostname: test\n")
	testSSHPubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI test-key"
)

// TestWriteCloudInitFiles verifies that user-data and meta-data are written
// correctly to the given directory when clusterIP is empty (single-node).
func TestWriteCloudInitFiles(t *testing.T) {
	dir := t.TempDir()
	clusterName := "test-cluster"

	if err := qemu.WriteCloudInitFiles(dir, clusterName, testSSHPubKey, testConfigB64, 0, ""); err != nil {
		t.Fatalf("WriteCloudInitFiles: %v", err)
	}

	// Verify user-data contains the SSH key and expected structure.
	userData, err := os.ReadFile(filepath.Join(dir, "user-data"))
	if err != nil {
		t.Fatalf("read user-data: %v", err)
	}
	if len(userData) == 0 {
		t.Error("user-data is empty")
	}
	userDataStr := string(userData)

	// Should start with cloud-config header.
	if !strings.HasPrefix(userDataStr, "#cloud-config\n") {
		t.Errorf("user-data does not start with #cloud-config, got prefix: %q", userDataStr[:min(20, len(userDataStr))])
	}
	// Should contain the SSH public key.
	if !strings.Contains(userDataStr, testSSHPubKey) {
		t.Errorf("user-data does not contain SSH public key")
	}
	// Should contain curl package.
	if !strings.Contains(userDataStr, "curl") {
		t.Error("user-data does not contain curl package")
	}
	// Should embed the spec YAML (base64) for cloud-init write_files.
	if !strings.Contains(userDataStr, testConfigB64) {
		t.Error("user-data does not contain config b64")
	}
	// The provider runs clusterboxnode via SSH; cloud-init must NOT call it.
	if strings.Contains(userDataStr, "clusterboxnode install") {
		t.Error("user-data must not call clusterboxnode install; provider does that via SSH")
	}

	// Verify meta-data contains instance-id and local-hostname.
	metaData, err := os.ReadFile(filepath.Join(dir, "meta-data"))
	if err != nil {
		t.Fatalf("read meta-data: %v", err)
	}
	metaDataStr := string(metaData)
	if !strings.Contains(metaDataStr, "instance-id: "+clusterName) {
		t.Errorf("meta-data missing instance-id, got: %q", metaDataStr)
	}
	if !strings.Contains(metaDataStr, "local-hostname: "+clusterName) {
		t.Errorf("meta-data missing local-hostname, got: %q", metaDataStr)
	}

	// No network-config should be written when clusterIP is empty.
	if _, err := os.Stat(filepath.Join(dir, "network-config")); !os.IsNotExist(err) {
		t.Error("expected no network-config when clusterIP is empty")
	}
}

// TestWriteCloudInitFilesWithClusterIP verifies that network-config is written
// when clusterIP is non-empty and contains the expected MAC addresses and IP.
func TestWriteCloudInitFilesWithClusterIP(t *testing.T) {
	dir := t.TempDir()
	clusterName := "my-cluster"

	// nodeIdx=0 → net0 MAC 52:54:00:01:00:00, net1 MAC 52:54:00:02:00:00
	if err := qemu.WriteCloudInitFiles(dir, clusterName, testSSHPubKey, testConfigB64, 0, "10.100.0.1/24"); err != nil {
		t.Fatalf("WriteCloudInitFiles: %v", err)
	}

	netCfg, err := os.ReadFile(filepath.Join(dir, "network-config"))
	if err != nil {
		t.Fatalf("read network-config: %v", err)
	}
	netCfgStr := string(netCfg)

	checks := []string{
		"version: 2",
		"52:54:00:01:00:00", // net0 MAC
		"52:54:00:02:00:00", // net1 MAC
		"10.100.0.1/24",
		"dhcp4: true",
	}
	for _, want := range checks {
		if !strings.Contains(netCfgStr, want) {
			t.Errorf("network-config missing %q\nfull content:\n%s", want, netCfgStr)
		}
	}

	// Worker node: nodeIdx=2 → net0 52:54:00:01:00:02, net1 52:54:00:02:00:02
	dir2 := t.TempDir()
	if err := qemu.WriteCloudInitFiles(dir2, clusterName+"-worker-2", testSSHPubKey, testConfigB64, 2, "10.100.0.3/24"); err != nil {
		t.Fatalf("WriteCloudInitFiles worker: %v", err)
	}
	netCfg2, err := os.ReadFile(filepath.Join(dir2, "network-config"))
	if err != nil {
		t.Fatalf("read worker network-config: %v", err)
	}
	netCfgStr2 := string(netCfg2)
	if !strings.Contains(netCfgStr2, "52:54:00:01:00:02") {
		t.Errorf("worker network-config missing net0 MAC 52:54:00:01:00:02\ngot:\n%s", netCfgStr2)
	}
	if !strings.Contains(netCfgStr2, "52:54:00:02:00:02") {
		t.Errorf("worker network-config missing net1 MAC 52:54:00:02:00:02\ngot:\n%s", netCfgStr2)
	}
	if !strings.Contains(netCfgStr2, "10.100.0.3/24") {
		t.Errorf("worker network-config missing IP 10.100.0.3/24\ngot:\n%s", netCfgStr2)
	}
}

// TestWriteCloudInitFilesErrorOnBadDir verifies that WriteCloudInitFiles returns
// an error when the directory does not exist.
func TestWriteCloudInitFilesErrorOnBadDir(t *testing.T) {
	err := qemu.WriteCloudInitFiles("/nonexistent/path/that/does/not/exist", "cluster", "key", testConfigB64, 0, "")
	if err == nil {
		t.Error("expected error writing to nonexistent dir, got nil")
	}
}

// TestMakeSeedISOErrorPath verifies that MakeSeedISO returns an error when no
// ISO tool is available (we test the error path via a fake PATH that has no tools).
func TestMakeSeedISOErrorPath(t *testing.T) {
	// Override PATH to an empty temp dir that has no executables.
	t.Setenv("PATH", t.TempDir())

	err := qemu.MakeSeedISO("/some/src", "/some/dst")
	if err == nil {
		t.Error("expected error when no ISO tool is available, got nil")
	}
}

// TestReconcileNoStateDir verifies that Reconcile returns MarkedDestroyed when
// no state dir or PID file exists.
func TestReconcileNoStateDir(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "nonexistent-cluster")
	p := qemu.New(qemu.Deps{
		StateDir: stateDir,
	})

	summary, err := p.Reconcile(context.Background(), "nonexistent-cluster")
	if err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}
	if summary.MarkedDestroyed != 1 {
		t.Errorf("Reconcile.MarkedDestroyed = %d, want 1", summary.MarkedDestroyed)
	}
	if summary.Existing != 0 {
		t.Errorf("Reconcile.Existing = %d, want 0", summary.Existing)
	}
}

// TestDestroyNoStateDir verifies that Destroy succeeds even when the state
// directory does not exist (best-effort, idempotent).
func TestDestroyNoStateDir(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "nonexistent-cluster")
	p := qemu.New(qemu.Deps{
		StateDir: stateDir,
	})

	if err := p.Destroy(context.Background(), registry.Cluster{Name: "test"}); err != nil {
		t.Errorf("Destroy on non-existent state dir: %v", err)
	}
}

// TestReconcileWithRunningPID verifies that Reconcile returns Existing=1 when
// the PID file contains the current process PID (which is definitely running).
func TestReconcileWithRunningPID(t *testing.T) {
	stateDir := t.TempDir()
	pidFile := filepath.Join(stateDir, "vm.pid")
	// Write the current process PID — guaranteed to be running.
	if err := os.WriteFile(pidFile, []byte(itoa(os.Getpid())), 0o600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	p := qemu.New(qemu.Deps{
		StateDir: stateDir,
	})

	summary, err := p.Reconcile(context.Background(), "test-cluster")
	if err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}
	if summary.Existing != 1 {
		t.Errorf("Reconcile.Existing = %d, want 1 (PID %d is running)", summary.Existing, os.Getpid())
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
