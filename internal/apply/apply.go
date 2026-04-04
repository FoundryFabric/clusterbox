// Package apply renders Jsonnet manifests and pipes them to kubectl apply.
package apply

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// CommandRunner executes external processes. Tests inject a mock; production
// code uses ExecRunner.
type CommandRunner interface {
	// Run executes name with args, optionally feeding stdin, and returns
	// combined stdout+stderr, or an error when the process exits non-zero.
	Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
}

// ExecRunner is the production CommandRunner that shells out via os/exec.
type ExecRunner struct{}

// Run implements CommandRunner using os/exec.
func (ExecRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w\noutput: %s", name, err, bytes.TrimSpace(out))
	}
	return out, nil
}

// ApplyManifests renders each .jsonnet file found in manifestDir using
// `jsonnet` and pipes the resulting JSON/YAML to `kubectl apply -f -`.
// Files are processed in lexicographic order for determinism.
// kubeconfig is passed to kubectl via --kubeconfig.
func ApplyManifests(ctx context.Context, kubeconfig, manifestDir string) error {
	return ApplyManifestsWithRunner(ctx, kubeconfig, manifestDir, ExecRunner{})
}

// ApplyManifestsWithRunner is the injectable variant used by tests.
func ApplyManifestsWithRunner(ctx context.Context, kubeconfig, manifestDir string, runner CommandRunner) error {
	entries, err := os.ReadDir(manifestDir)
	if err != nil {
		return fmt.Errorf("apply: read manifest dir %q: %w", manifestDir, err)
	}

	var jsonnetFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonnet") {
			jsonnetFiles = append(jsonnetFiles, filepath.Join(manifestDir, e.Name()))
		}
	}

	if len(jsonnetFiles) == 0 {
		return fmt.Errorf("apply: no .jsonnet files found in %q", manifestDir)
	}

	sort.Strings(jsonnetFiles)

	for _, path := range jsonnetFiles {
		rendered, err := runner.Run(ctx, nil, "jsonnet", path)
		if err != nil {
			return fmt.Errorf("apply: render %q: %w", filepath.Base(path), err)
		}

		if _, err := runner.Run(ctx, rendered, "kubectl",
			"--kubeconfig", kubeconfig,
			"apply", "-f", "-",
		); err != nil {
			return fmt.Errorf("apply: kubectl apply %q: %w", filepath.Base(path), err)
		}
	}

	return nil
}
