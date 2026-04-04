package secrets_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/secrets"
)

// ---- DevResolver tests ----

const devSecretsJSON = `{
	"JWT_SECRET": "dev-jwt-secret",
	"DB_PASSWORD": "dev-db-password"
}`

// TestDevResolver_ReadsKeysFromJSON verifies that DevResolver correctly parses
// a valid JSON secrets file and returns all key-value pairs.
func TestDevResolver_ReadsKeysFromJSON(t *testing.T) {
	r := &secrets.DevResolver{
		ReadFileFn: func(name string) ([]byte, error) {
			return []byte(devSecretsJSON), nil
		},
	}

	got, err := r.Resolve(context.Background(), "app", "dev", "hetzner", "ash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]string{
		"JWT_SECRET":  "dev-jwt-secret",
		"DB_PASSWORD": "dev-db-password",
	}
	for k, wantV := range want {
		if gotV, ok := got[k]; !ok {
			t.Errorf("key %q missing from result", k)
		} else if gotV != wantV {
			t.Errorf("key %q: want %q, got %q", k, wantV, gotV)
		}
	}
}

// TestDevResolver_ErrorOnMissingFile verifies that DevResolver returns a
// descriptive error when the secrets file does not exist.
func TestDevResolver_ErrorOnMissingFile(t *testing.T) {
	r := &secrets.DevResolver{
		ReadFileFn: func(name string) ([]byte, error) {
			return nil, errors.New("no such file or directory")
		},
	}

	_, err := r.Resolve(context.Background(), "app", "dev", "hetzner", "ash")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "secrets: dev resolver") {
		t.Errorf("error should mention dev resolver, got: %v", err)
	}
}

// TestDevResolver_ErrorOnInvalidJSON verifies that DevResolver returns an error
// when the file contents are not valid JSON.
func TestDevResolver_ErrorOnInvalidJSON(t *testing.T) {
	r := &secrets.DevResolver{
		ReadFileFn: func(name string) ([]byte, error) {
			return []byte(`not json`), nil
		},
	}

	_, err := r.Resolve(context.Background(), "app", "dev", "hetzner", "ash")
	if err == nil {
		t.Fatal("expected error from invalid JSON, got nil")
	}
}

// ---- OPResolver tests ----

// mockOPRunner records calls and returns preconfigured outputs.
type mockOPRunner struct {
	calls    []opCall
	response func(name string, args []string) ([]byte, error)
}

type opCall struct {
	name string
	args []string
}

func (m *mockOPRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	m.calls = append(m.calls, opCall{name: name, args: args})
	if m.response != nil {
		return m.response(name, args)
	}
	return nil, nil
}

// TestOPResolver_ShellsOutWithCorrectPath verifies that OPResolver calls `op read`
// with the correct secret path for each key.
func TestOPResolver_ShellsOutWithCorrectPath(t *testing.T) {
	mock := &mockOPRunner{
		response: func(_ string, args []string) ([]byte, error) {
			// Return a fake value; caller should not log it.
			return []byte("secret-value\n"), nil
		},
	}

	r := secrets.NewOPResolverWithRunner([]string{"JWT_SECRET", "DB_PASSWORD"}, mock.run)

	got, err := r.Resolve(context.Background(), "foundryfabric", "prod", "hetzner", "ash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.calls) != 2 {
		t.Fatalf("expected 2 calls to op, got %d", len(mock.calls))
	}

	wantPaths := map[string]string{
		"JWT_SECRET":  "op://foundryfabric/prod/hetzner/ash/JWT_SECRET",
		"DB_PASSWORD": "op://foundryfabric/prod/hetzner/ash/DB_PASSWORD",
	}

	for _, c := range mock.calls {
		if c.name != "op" {
			t.Errorf("expected command %q, got %q", "op", c.name)
		}
		if len(c.args) < 2 || c.args[0] != "read" {
			t.Errorf("expected first arg %q, got %v", "read", c.args)
		}
		path := c.args[1]
		// Extract key name from the path (last segment).
		parts := strings.Split(path, "/")
		key := parts[len(parts)-1]
		if wantPath, ok := wantPaths[key]; !ok {
			t.Errorf("unexpected key in path: %q", path)
		} else if path != wantPath {
			t.Errorf("key %q: want path %q, got %q", key, wantPath, path)
		}
	}

	// Verify values have trailing newline stripped.
	for k, v := range got {
		if strings.Contains(v, "\n") {
			t.Errorf("key %q value should have trailing newline stripped, got %q", k, v)
		}
	}
}

// TestOPResolver_ErrorOnNonZeroExit verifies that OPResolver returns a
// descriptive error when `op` exits non-zero, and that the error message does
// NOT contain the secret path or value (to prevent credential leaking).
func TestOPResolver_ErrorOnNonZeroExit(t *testing.T) {
	mock := &mockOPRunner{
		response: func(_ string, _ []string) ([]byte, error) {
			return nil, errors.New("exit status 1")
		},
	}

	r := secrets.NewOPResolverWithRunner([]string{"JWT_SECRET"}, mock.run)

	_, err := r.Resolve(context.Background(), "foundryfabric", "prod", "hetzner", "ash")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	errStr := err.Error()
	if !strings.Contains(errStr, "secrets: op resolver") {
		t.Errorf("error should mention op resolver, got: %s", errStr)
	}
	if !strings.Contains(errStr, "JWT_SECRET") {
		t.Errorf("error should include the key name for diagnostics, got: %s", errStr)
	}
	// Ensure the raw secret path is not embedded in the error.
	if strings.Contains(errStr, "op://") {
		t.Errorf("error must not include secret path op://..., got: %s", errStr)
	}
}

// TestOPResolver_KeyNotLogged verifies that a successfully retrieved secret
// value is NOT present in any error returned for a different key. This guards
// against accidental co-mingling of values in error messages.
func TestOPResolver_KeyNotLogged(t *testing.T) {
	callCount := 0
	mock := &mockOPRunner{
		response: func(_ string, _ []string) ([]byte, error) {
			callCount++
			if callCount == 1 {
				return []byte("top-secret-value"), nil
			}
			// Second key fails.
			return nil, errors.New("exit status 1")
		},
	}

	r := secrets.NewOPResolverWithRunner([]string{"KEY1", "KEY2"}, mock.run)
	_, err := r.Resolve(context.Background(), "foundryfabric", "prod", "hetzner", "ash")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if strings.Contains(err.Error(), "top-secret-value") {
		t.Error("secret value from KEY1 must not appear in the error message for KEY2")
	}
}

// ---- CreateGHCRSecret tests ----

// mockCommandRunner records kubectl invocations.
type mockCommandRunner struct {
	calls    []cmdCall
	response func(name string, args []string) ([]byte, error)
}

type cmdCall struct {
	name string
	args []string
}

func (m *mockCommandRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	m.calls = append(m.calls, cmdCall{name: name, args: args})
	if m.response != nil {
		return m.response(name, args)
	}
	return nil, nil
}

// TestCreateGHCRSecret_InvokesKubectlCorrectly verifies that CreateGHCRSecret
// calls kubectl with the expected flags.
func TestCreateGHCRSecret_InvokesKubectlCorrectly(t *testing.T) {
	mock := &mockCommandRunner{}

	err := secrets.CreateGHCRSecret(context.Background(), mock, "/tmp/kube.yaml", "mytoken", "myuser")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expect at least 2 calls: delete + create.
	if len(mock.calls) < 2 {
		t.Fatalf("expected at least 2 kubectl calls, got %d", len(mock.calls))
	}

	// Last call should be the create.
	createCall := mock.calls[len(mock.calls)-1]
	if createCall.name != "kubectl" {
		t.Errorf("expected kubectl, got %q", createCall.name)
	}

	assertArg(t, createCall.args, "--kubeconfig", "/tmp/kube.yaml")
	assertArg(t, createCall.args, "--namespace", "default")
	assertArg(t, createCall.args, "--docker-server", "ghcr.io")
	assertArg(t, createCall.args, "--docker-username", "myuser")
	assertArg(t, createCall.args, "--docker-password", "mytoken")

	// The secret must be named "ghcr-credentials".
	if !containsArg(createCall.args, "ghcr-credentials") {
		t.Errorf("expected secret name ghcr-credentials in args: %v", createCall.args)
	}
}

// TestCreateGHCRSecret_ErrorOnKubectlFailure verifies that CreateGHCRSecret
// returns an error when kubectl exits non-zero and that the error does NOT
// include the token value.
func TestCreateGHCRSecret_ErrorOnKubectlFailure(t *testing.T) {
	mock := &mockCommandRunner{
		response: func(name string, args []string) ([]byte, error) {
			// delete call can succeed; create call fails.
			if containsArg(args, "create") {
				return nil, fmt.Errorf("exit status 1")
			}
			return nil, nil
		},
	}

	err := secrets.CreateGHCRSecret(context.Background(), mock, "/tmp/kube.yaml", "super-secret-token", "myuser")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if strings.Contains(err.Error(), "super-secret-token") {
		t.Error("token must not appear in the error message")
	}
}

// ---- helpers ----

func assertArg(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i, a := range args {
		if a == flag {
			if i+1 >= len(args) {
				t.Errorf("flag %q has no value", flag)
				return
			}
			if got := args[i+1]; got != value {
				t.Errorf("flag %q: want %q, got %q", flag, value, got)
			}
			return
		}
	}
	t.Errorf("flag %q not found in args: %v", flag, args)
}

func containsArg(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}
