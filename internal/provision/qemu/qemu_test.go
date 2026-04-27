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

// TestWriteCloudInitFiles verifies that user-data and meta-data are written
// correctly to the given directory.
func TestWriteCloudInitFiles(t *testing.T) {
	dir := t.TempDir()
	clusterName := "test-cluster"
	sshPubKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI test-key"

	if err := qemu.WriteCloudInitFiles(dir, clusterName, sshPubKey); err != nil {
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
	if !strings.Contains(userDataStr, sshPubKey) {
		t.Errorf("user-data does not contain SSH public key %q", sshPubKey)
	}
	// Should contain curl package.
	if !strings.Contains(userDataStr, "curl") {
		t.Error("user-data does not contain curl package")
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
}

// TestWriteCloudInitFilesErrorOnBadDir verifies that WriteCloudInitFiles returns
// an error when the directory does not exist.
func TestWriteCloudInitFilesErrorOnBadDir(t *testing.T) {
	err := qemu.WriteCloudInitFiles("/nonexistent/path/that/does/not/exist", "cluster", "key")
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
