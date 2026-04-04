package secrets

import (
	"context"
	"fmt"
)

// CreateGHCRSecret creates (or replaces) a k8s docker-registry Secret named
// "ghcr-credentials" in the "default" namespace using the supplied kubeconfig.
//
// The token and user values are passed directly to kubectl; they are never
// written to any log or error message.
//
// A CommandRunner is accepted so that tests can inject a mock without
// shelling out to a real cluster.
func CreateGHCRSecret(ctx context.Context, runner CommandRunner, kubeconfig, token, user string) error {
	// kubectl create secret docker-registry does not support --dry-run +
	// apply natively, so we delete any pre-existing secret first (ignore
	// "not found" errors) and then recreate it.
	_, _ = runner.Run(ctx, "kubectl",
		"--kubeconfig", kubeconfig,
		"delete", "secret", "ghcr-credentials",
		"--namespace", "default",
		"--ignore-not-found",
	)

	_, err := runner.Run(ctx, "kubectl",
		"--kubeconfig", kubeconfig,
		"create", "secret", "docker-registry", "ghcr-credentials",
		"--namespace", "default",
		"--docker-server", "ghcr.io",
		"--docker-username", user,
		"--docker-password", token,
	)
	if err != nil {
		// Do NOT include token or user in this message.
		return fmt.Errorf("secrets: create ghcr-credentials: kubectl exited non-zero")
	}

	return nil
}

// CommandRunner executes external processes. Tests inject a mock; production
// code uses the ExecCommandRunner.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecCommandRunner is the production CommandRunner that shells out via os/exec.
type ExecCommandRunner struct{}

// Run implements CommandRunner using os/exec.
func (ExecCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return execRun(ctx, name, args...)
}
