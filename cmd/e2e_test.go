//go:build e2e

package cmd_test

// End-to-end lifecycle test for the QEMU provider.
//
// Runs the full cluster lifecycle:
//   1. Provision  → 1 node in Kubernetes (control plane)
//   2. AddNode    → 2 nodes in Kubernetes
//   3. remove-node → 1 node in Kubernetes
//   4. Destroy    → kubeconfig gone, cluster state wiped
//
// Requirements: qemu-system-x86_64, qemu-img, genisoimage or cloud-image-utils,
// and an SSH key at ~/.ssh/id_ed25519. Takes 20–30 minutes.
//
// Run with:
//
//	go test -v -tags e2e -run TestQEMULifecycleE2E -timeout 40m ./cmd/

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/cmd"
	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/provision/qemu"
	"github.com/foundryfabric/clusterbox/internal/registry"
)

func TestQEMULifecycleE2E(t *testing.T) {
	clusterName := fmt.Sprintf("e2e-%d", time.Now().Unix())

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	sshKeyPath := filepath.Join(home, ".ssh", "id_ed25519")

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()

	p := qemu.New(qemu.Deps{SSHKeyPath: sshKeyPath})

	// Always destroy on exit so a mid-test failure doesn't leave QEMU
	// processes or disk images behind.
	t.Cleanup(func() {
		t.Log("cleanup: destroying cluster")
		_ = p.Destroy(context.Background(), registry.Cluster{Name: clusterName})
	})

	// --- Step 1: Provision ---
	t.Logf("provisioning QEMU cluster %q...", clusterName)
	result, err := p.Provision(ctx, provision.ClusterConfig{ClusterName: clusterName})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	kubeconfigPath := result.KubeconfigPath
	t.Logf("cluster provisioned; kubeconfig: %s", kubeconfigPath)

	// --- Step 2: Validate 1 node (control plane only) ---
	nodes := getNodeNames(t, kubeconfigPath)
	if len(nodes) != 1 {
		t.Fatalf("after provision: got %d nodes, want 1; nodes: %v", len(nodes), nodes)
	}
	t.Logf("provision OK: 1 node (%v)", nodes)

	// --- Step 3: AddNode ---
	t.Log("adding worker node...")
	workerName, err := p.AddNode(ctx, clusterName)
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	t.Logf("worker added: %s", workerName)

	// --- Step 4: Validate 2 nodes ---
	nodes = getNodeNames(t, kubeconfigPath)
	if len(nodes) != 2 {
		t.Fatalf("after add-node: got %d nodes, want 2; nodes: %v", len(nodes), nodes)
	}
	if !containsStr(nodes, workerName) {
		t.Errorf("worker %q not found in node list: %v", workerName, nodes)
	}
	t.Logf("add-node OK: 2 nodes (%v)", nodes)

	// --- Step 5: remove-node (drain + kubectl delete + provider teardown) ---
	t.Logf("removing worker node %q...", workerName)
	if err := cmd.RunRemoveNodeWith(ctx, clusterName, []string{workerName}, bootstrap.ExecRunner{}); err != nil {
		t.Fatalf("remove-node: %v", err)
	}

	// --- Step 6: Validate back to 1 node ---
	nodes = getNodeNames(t, kubeconfigPath)
	if len(nodes) != 1 {
		t.Fatalf("after remove-node: got %d nodes, want 1; nodes: %v", len(nodes), nodes)
	}
	if containsStr(nodes, workerName) {
		t.Errorf("worker %q still present after remove-node: %v", workerName, nodes)
	}
	t.Logf("remove-node OK: 1 node (%v)", nodes)

	// --- Step 7: Destroy ---
	t.Log("destroying cluster...")
	if err := p.Destroy(ctx, registry.Cluster{Name: clusterName}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	// --- Step 8: Validate cluster is gone ---
	if _, err := os.Stat(kubeconfigPath); !os.IsNotExist(err) {
		t.Errorf("kubeconfig %s still exists after Destroy", kubeconfigPath)
	}
	t.Log("destroy OK: cluster gone")
}

// getNodeNames returns the names of all nodes visible via kubectl.
func getNodeNames(t *testing.T, kubeconfigPath string) []string {
	t.Helper()
	out, err := exec.Command("kubectl",
		"--kubeconfig", kubeconfigPath,
		"get", "nodes",
		"--no-headers",
		"-o", "custom-columns=NAME:.metadata.name",
	).Output()
	if err != nil {
		t.Fatalf("kubectl get nodes: %v", err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
