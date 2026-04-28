package qemu

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// userDataTemplate is the cloud-init user-data template.
//
// cloud-init writes the clusterboxnode spec to /etc/clusterboxnode.yaml on
// first boot. The provider then SSHes in, uploads the clusterboxnode binary,
// and runs it with --config /etc/clusterboxnode.yaml.
//
// Tailscale is disabled in the spec: QEMU VMs are accessed via SSH
// port-forwarding on localhost, so no Tailscale mesh is needed.
//
// Placeholders: sshPubKey, configB64 (base64-encoded spec YAML).
const userDataTemplate = `#cloud-config
users:
  - name: ubuntu
    ssh_authorized_keys:
      - %s
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
package_update: false
packages:
  - curl
write_files:
  - path: /etc/clusterboxnode.yaml
    encoding: b64
    content: %s
    permissions: '0644'
`

// networkConfigTemplate is the cloud-init network-config (v2) template.
// It assigns a static IP to the cluster interface (net1) using MAC matching,
// while leaving the user-net interface (net0) on DHCP.
//
// Placeholders: net0MAC, net1MAC, clusterIP (e.g. "10.100.0.1/24").
const networkConfigTemplate = `version: 2
ethernets:
  id-user:
    match:
      macaddress: "%s"
    dhcp4: true
  id-cluster:
    match:
      macaddress: "%s"
    addresses:
      - %s
`

// WriteCloudInitFiles writes user-data, meta-data, and (optionally)
// network-config files to dir.
//
//   - clusterName is used as the instance-id and local-hostname in meta-data.
//   - sshPubKey is injected into the ubuntu user's authorized_keys.
//   - configB64 is the base64-encoded clusterboxnode spec YAML written to
//     /etc/clusterboxnode.yaml by cloud-init at first boot.
//   - nodeIdx is the sequential node index (0=control-plane, 1=first worker…).
//     It is used to compute deterministic MACs for both network interfaces.
//   - clusterIP is the static IP to assign on the cluster network interface
//     (net1), e.g. "10.100.0.1/24". When empty, no network-config is written.
func WriteCloudInitFiles(dir, clusterName, sshPubKey, configB64 string, nodeIdx int, clusterIP string) error {
	userData := fmt.Sprintf(userDataTemplate, sshPubKey, configB64)
	if err := os.WriteFile(filepath.Join(dir, "user-data"), []byte(userData), 0o644); err != nil {
		return fmt.Errorf("qemu: write user-data: %w", err)
	}

	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", clusterName, clusterName)
	if err := os.WriteFile(filepath.Join(dir, "meta-data"), []byte(metaData), 0o644); err != nil {
		return fmt.Errorf("qemu: write meta-data: %w", err)
	}

	if clusterIP != "" {
		net0MAC := fmt.Sprintf("52:54:00:01:00:%02x", nodeIdx)
		net1MAC := fmt.Sprintf("52:54:00:02:00:%02x", nodeIdx)
		networkConfig := fmt.Sprintf(networkConfigTemplate, net0MAC, net1MAC, clusterIP)
		if err := os.WriteFile(filepath.Join(dir, "network-config"), []byte(networkConfig), 0o644); err != nil {
			return fmt.Errorf("qemu: write network-config: %w", err)
		}
	}

	return nil
}

// MakeSeedISO creates a cidata ISO at dst using files from srcDir.
// Tries genisoimage, then mkisofs, then hdiutil (macOS) in order.
// If a network-config file exists in srcDir it is automatically included.
// Any existing file at dst is removed first so reruns are idempotent.
func MakeSeedISO(srcDir, dst string) error {
	_ = os.Remove(dst)
	type isoTool struct {
		name string
		args func(srcDir, dst string) []string
	}
	tools := []isoTool{
		{
			name: "genisoimage",
			args: func(srcDir, dst string) []string {
				return []string{
					"-output", dst,
					"-volid", "cidata",
					"-joliet", "-rock",
					"-quiet",
					srcDir,
				}
			},
		},
		{
			name: "mkisofs",
			args: func(srcDir, dst string) []string {
				return []string{
					"-output", dst,
					"-volid", "cidata",
					"-joliet", "-rock",
					"-quiet",
					srcDir,
				}
			},
		},
		{
			name: "hdiutil",
			args: func(srcDir, dst string) []string {
				// Use -joliet -iso only (no -hfs). Adding -hfs wraps the image
				// in an Apple partition table, which confuses Linux's blkid and
				// prevents cloud-init from finding the cidata volume.
				return []string{
					"makehybrid",
					"-o", dst,
					"-joliet",
					"-iso",
					"-default-volume-name", "cidata",
					srcDir,
				}
			},
		},
	}

	var lastErr error
	for _, t := range tools {
		path, err := exec.LookPath(t.name)
		if err != nil {
			continue
		}
		cmd := exec.Command(path, t.args(srcDir, dst)...) //nolint:gosec
		if out, err := cmd.CombinedOutput(); err != nil {
			lastErr = fmt.Errorf("qemu: %s: %w (output: %s)", t.name, err, out)
			continue
		}
		return nil
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("qemu: no ISO tool found; install genisoimage, mkisofs, or hdiutil")
}
