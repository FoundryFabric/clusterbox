package cmd_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/apply"
	"github.com/foundryfabric/clusterbox/internal/secrets"
)

// UpRunner is the interface that the up command uses to call each step.
// The real implementation delegates to the actual packages; tests inject mocks.
//
// This file tests the step-ordering contract exported by the up command via
// RunUp, which accepts an UpDeps bag of injectable dependencies.

// ---- mock command runner (shared between bootstrap and secrets) ----

type mockCmdCall struct {
	stdin []byte
	name  string
	args  []string
}

// mockCommandRunner satisfies both bootstrap.CommandRunner and secrets.CommandRunner.
type mockCommandRunner struct {
	mu       sync.Mutex
	calls    []mockCmdCall
	response func(name string, args []string) ([]byte, error)
}

func (m *mockCommandRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	m.calls = append(m.calls, mockCmdCall{name: name, args: args})
	m.mu.Unlock()
	if m.response != nil {
		return m.response(name, args)
	}
	// Default: k3sup succeeds; kubectl returns Ready; all others no-op.
	if name == "kubectl" && containsArg(args, "get") {
		return []byte("clusterbox   Ready   master   1m\n"), nil
	}
	return nil, nil
}

// mockApplyRunner satisfies apply.CommandRunner.
type mockApplyRunner struct {
	mu    sync.Mutex
	calls []mockCmdCall
	err   error
}

func (m *mockApplyRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	m.calls = append(m.calls, mockCmdCall{stdin: stdin, name: name, args: args})
	m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	if name == "jsonnet" {
		return []byte("apiVersion: v1\nkind: ConfigMap\n"), nil
	}
	return nil, nil
}

// ---- step-order tracking ----

// ---- unit tests ----

// TestUpStep5_GHCRSecretNotLogged verifies that the GHCR token is not included
// in any error message returned by CreateGHCRSecret.
func TestUpStep5_GHCRSecretNotLogged(t *testing.T) {
	const sensitiveToken = "ghcr-super-secret-token-xyz"

	runner := &mockCommandRunner{
		response: func(name string, args []string) ([]byte, error) {
			if containsArg(args, "create") {
				return nil, errors.New("exit status 1")
			}
			return nil, nil
		},
	}

	err := secrets.CreateGHCRSecret(context.Background(), runner, "/tmp/kube.yaml", sensitiveToken, "user")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), sensitiveToken) {
		t.Errorf("error must not contain the GHCR token, got: %v", err)
	}
}

// TestUpStep6_ApplyRendersAllJsonnetFiles verifies that each .jsonnet file in
// the manifest directory is rendered by jsonnet and the output piped to kubectl.
func TestUpStep6_ApplyRendersAllJsonnetFiles(t *testing.T) {
	runner := &mockApplyRunner{}

	dir := t.TempDir()
	manifestNames := []string{"fdb-operator.jsonnet", "otel-collector.jsonnet", "traefik.jsonnet"}
	for _, n := range manifestNames {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write %q: %v", n, err)
		}
	}

	if err := apply.ApplyManifestsWithRunner(context.Background(), "/tmp/kube.yaml", dir, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 3 jsonnet + 3 kubectl = 6 total calls.
	if len(runner.calls) != 6 {
		t.Errorf("expected 6 calls, got %d", len(runner.calls))
	}

	// Collect jsonnet-rendered file names (last arg is the path; earlier args are flags like -J).
	var rendered []string
	for _, c := range runner.calls {
		if c.name == "jsonnet" && len(c.args) > 0 {
			rendered = append(rendered, filepath.Base(c.args[len(c.args)-1]))
		}
	}

	for _, wantFile := range manifestNames {
		found := false
		for _, r := range rendered {
			if r == wantFile {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("manifest %q was not rendered by jsonnet", wantFile)
		}
	}
}

// TestUpStep6_ApplyIdempotent verifies that calling ApplyManifests twice does
// not produce an error (idempotency via kubectl apply).
func TestUpStep6_ApplyIdempotent(t *testing.T) {
	runner := &mockApplyRunner{}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "traefik.jsonnet"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := apply.ApplyManifestsWithRunner(context.Background(), "/tmp/kube.yaml", dir, runner); err != nil {
			t.Fatalf("run %d: unexpected error: %v", i+1, err)
		}
	}
}

// ---- helpers ----

func containsArg(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}
