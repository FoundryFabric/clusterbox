package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// CommandRunner is the interface used to execute external commands. Tests inject
// a mock; production code uses ExecRunner.
type CommandRunner interface {
	// Run executes name with args and returns combined stdout+stderr, or an
	// error that wraps the combined output when the process exits non-zero.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner is the production CommandRunner that shells out via os/exec.
type ExecRunner struct{}

// Run implements CommandRunner using os/exec.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w\noutput: %s", name, err, bytes.TrimSpace(out))
	}
	return out, nil
}
