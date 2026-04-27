package qemu

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// clusterState holds the persistent per-cluster metadata written at
// Provision time and read by AddNode.
type clusterState struct {
	McastPort     int    `json:"mcast_port"`
	CPSSHPort     int    `json:"cp_ssh_port"`
	CPK3sPort     int    `json:"cp_k3s_port"`
	CPClusterIP   string `json:"cp_cluster_ip"`
	NextWorkerIdx int    `json:"next_worker_idx"`
	NodeToken     string `json:"node_token"`
}

// loadClusterState reads cluster.json from stateDir.
func loadClusterState(stateDir string) (*clusterState, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, "cluster.json"))
	if err != nil {
		return nil, fmt.Errorf("qemu: read cluster.json: %w", err)
	}
	var s clusterState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("qemu: parse cluster.json: %w", err)
	}
	return &s, nil
}

// saveClusterState writes cluster.json to stateDir atomically.
func saveClusterState(stateDir string, s *clusterState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("qemu: marshal cluster.json: %w", err)
	}
	dst := filepath.Join(stateDir, "cluster.json")
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("qemu: write cluster.json: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("qemu: rename cluster.json: %w", err)
	}
	return nil
}

// findFreeUDPPort finds a free UDP port on localhost starting at start.
func findFreeUDPPort(start int) (int, error) {
	for port := start; port < start+100; port++ {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		conn, err := net.ListenPacket("udp", addr)
		if err != nil {
			continue
		}
		_ = conn.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free UDP port found in range %d-%d", start, start+99)
}
