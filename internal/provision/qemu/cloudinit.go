package qemu

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// userDataTemplate is the cloud-init user-data template. No Tailscale:
// the QEMU VM is accessed via SSH port-forwarding on localhost.
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
runcmd:
  - mkdir -p /data
`

// WriteCloudInitFiles writes user-data and meta-data files to dir.
// clusterName is used as the instance-id and local-hostname in meta-data.
// sshPubKey is injected into the ubuntu user's authorized_keys.
func WriteCloudInitFiles(dir, clusterName, sshPubKey string) error {
	userData := fmt.Sprintf(userDataTemplate, sshPubKey)
	if err := os.WriteFile(filepath.Join(dir, "user-data"), []byte(userData), 0o644); err != nil {
		return fmt.Errorf("qemu: write user-data: %w", err)
	}

	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", clusterName, clusterName)
	if err := os.WriteFile(filepath.Join(dir, "meta-data"), []byte(metaData), 0o644); err != nil {
		return fmt.Errorf("qemu: write meta-data: %w", err)
	}

	return nil
}

// MakeSeedISO creates a cidata ISO at dst using files from srcDir.
// Tries genisoimage, then mkisofs, then hdiutil (macOS) in order.
func MakeSeedISO(srcDir, dst string) error {
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
				return []string{
					"makehybrid",
					"-o", dst,
					"-hfs",
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
