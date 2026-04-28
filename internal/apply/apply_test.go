package apply_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/apply"
)

// ---- mock runner ----

// call records a single invocation of CommandRunner.Run.
type call struct {
	stdin []byte
	name  string
	args  []string
}

// mockRunner is a configurable CommandRunner for unit tests.
type mockRunner struct {
	calls    []call
	response func(name string, args []string) ([]byte, error)
}

func (m *mockRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	m.calls = append(m.calls, call{stdin: stdin, name: name, args: args})
	if m.response != nil {
		return m.response(name, args)
	}
	// Default: jsonnet returns some fake YAML, kubectl succeeds.
	if name == "jsonnet" {
		return []byte("apiVersion: v1\nkind: ConfigMap\n"), nil
	}
	return nil, nil
}

// ---- helpers ----

// writeTempManifests creates a temporary directory with the given .jsonnet
// file names (empty content) and returns the directory path.
func writeTempManifests(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write temp manifest %q: %v", n, err)
		}
	}
	return dir
}

// ---- tests ----

// TestApplyManifests_RendersAndAppliesEachFile verifies that for N jsonnet files
// in the directory there are exactly N jsonnet invocations followed by N kubectl
// apply invocations, each paired correctly.
func TestApplyManifests_RendersAndAppliesEachFile(t *testing.T) {
	runner := &mockRunner{}

	dir := writeTempManifests(t, "fdb-operator.jsonnet", "otel-collector.jsonnet", "traefik.jsonnet")

	if err := apply.ApplyManifestsWithRunner(context.Background(), "/tmp/kube.yaml", dir, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expect 6 calls: 3 jsonnet + 3 kubectl (paired in order).
	if len(runner.calls) != 6 {
		t.Fatalf("expected 6 runner calls, got %d: %v", len(runner.calls), runner.calls)
	}

	jsonnetCalls := 0
	kubectlCalls := 0
	for _, c := range runner.calls {
		switch c.name {
		case "jsonnet":
			jsonnetCalls++
		case "kubectl":
			kubectlCalls++
		}
	}
	if jsonnetCalls != 3 {
		t.Errorf("expected 3 jsonnet calls, got %d", jsonnetCalls)
	}
	if kubectlCalls != 3 {
		t.Errorf("expected 3 kubectl calls, got %d", kubectlCalls)
	}
}

// TestApplyManifests_LexicographicOrder verifies that files are processed in
// sorted order, so apply is deterministic.
func TestApplyManifests_LexicographicOrder(t *testing.T) {
	runner := &mockRunner{}

	dir := writeTempManifests(t, "traefik.jsonnet", "fdb-operator.jsonnet", "otel-collector.jsonnet")

	if err := apply.ApplyManifestsWithRunner(context.Background(), "/tmp/kube.yaml", dir, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Collect only jsonnet calls (last arg is the file path; earlier args are flags like -J).
	var jsonnetFiles []string
	for _, c := range runner.calls {
		if c.name == "jsonnet" && len(c.args) > 0 {
			jsonnetFiles = append(jsonnetFiles, filepath.Base(c.args[len(c.args)-1]))
		}
	}

	want := []string{"fdb-operator.jsonnet", "otel-collector.jsonnet", "traefik.jsonnet"}
	if len(jsonnetFiles) != len(want) {
		t.Fatalf("expected %d jsonnet files, got %d: %v", len(want), len(jsonnetFiles), jsonnetFiles)
	}
	for i, w := range want {
		if jsonnetFiles[i] != w {
			t.Errorf("file[%d]: want %q, got %q", i, w, jsonnetFiles[i])
		}
	}
}

// TestApplyManifests_KubeconfigPassedToKubectl verifies that the kubeconfig
// path is forwarded to every kubectl invocation.
func TestApplyManifests_KubeconfigPassedToKubectl(t *testing.T) {
	const kubeconfigPath = "/home/ops/.kube/clusterbox.yaml"
	runner := &mockRunner{}

	dir := writeTempManifests(t, "fdb-operator.jsonnet")

	if err := apply.ApplyManifestsWithRunner(context.Background(), kubeconfigPath, dir, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, c := range runner.calls {
		if c.name != "kubectl" {
			continue
		}
		found := false
		for i, a := range c.args {
			if a == "--kubeconfig" && i+1 < len(c.args) && c.args[i+1] == kubeconfigPath {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("kubectl call missing --kubeconfig %q: args=%v", kubeconfigPath, c.args)
		}
	}
}

// TestApplyManifests_RenderedOutputPipedToKubectl verifies that the stdout of
// jsonnet is passed as stdin to the subsequent kubectl apply call.
func TestApplyManifests_RenderedOutputPipedToKubectl(t *testing.T) {
	const fakeManifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n"

	runner := &mockRunner{
		response: func(name string, _ []string) ([]byte, error) {
			if name == "jsonnet" {
				return []byte(fakeManifest), nil
			}
			return nil, nil
		},
	}

	dir := writeTempManifests(t, "fdb-operator.jsonnet")

	if err := apply.ApplyManifestsWithRunner(context.Background(), "/tmp/kube.yaml", dir, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the kubectl call and verify its stdin equals fakeManifest.
	for _, c := range runner.calls {
		if c.name == "kubectl" {
			if string(c.stdin) != fakeManifest {
				t.Errorf("kubectl stdin = %q; want %q", string(c.stdin), fakeManifest)
			}
			return
		}
	}
	t.Error("no kubectl call found")
}

// TestApplyManifests_ErrorOnJsonnetFailure verifies that an error from jsonnet
// halts processing and is propagated.
func TestApplyManifests_ErrorOnJsonnetFailure(t *testing.T) {
	runner := &mockRunner{
		response: func(name string, _ []string) ([]byte, error) {
			if name == "jsonnet" {
				return nil, context.DeadlineExceeded
			}
			return nil, nil
		},
	}

	dir := writeTempManifests(t, "fdb-operator.jsonnet")

	err := apply.ApplyManifestsWithRunner(context.Background(), "/tmp/kube.yaml", dir, runner)
	if err == nil {
		t.Fatal("expected error from jsonnet failure, got nil")
	}
	if !strings.Contains(err.Error(), "render") {
		t.Errorf("error should mention render step, got: %v", err)
	}
}

// TestApplyManifests_ErrorOnKubectlFailure verifies that an error from kubectl
// is propagated.
func TestApplyManifests_ErrorOnKubectlFailure(t *testing.T) {
	runner := &mockRunner{
		response: func(name string, _ []string) ([]byte, error) {
			if name == "kubectl" {
				return nil, context.DeadlineExceeded
			}
			return []byte("apiVersion: v1\n"), nil
		},
	}

	dir := writeTempManifests(t, "fdb-operator.jsonnet")

	err := apply.ApplyManifestsWithRunner(context.Background(), "/tmp/kube.yaml", dir, runner)
	if err == nil {
		t.Fatal("expected error from kubectl failure, got nil")
	}
	if !strings.Contains(err.Error(), "kubectl apply") {
		t.Errorf("error should mention kubectl apply step, got: %v", err)
	}
}

// TestApplyManifests_EmptyDirectory verifies that an error is returned when no
// .jsonnet files are present in the directory.
func TestApplyManifests_EmptyDirectory(t *testing.T) {
	runner := &mockRunner{}
	dir := t.TempDir() // no files

	err := apply.ApplyManifestsWithRunner(context.Background(), "/tmp/kube.yaml", dir, runner)
	if err == nil {
		t.Fatal("expected error for empty directory, got nil")
	}
}

// TestApplyManifests_IgnoresNonJsonnetFiles verifies that non-.jsonnet files
// in the directory are silently skipped.
func TestApplyManifests_IgnoresNonJsonnetFiles(t *testing.T) {
	runner := &mockRunner{}

	dir := t.TempDir()
	// Write a mix of jsonnet and non-jsonnet files.
	for _, name := range []string{"fdb-operator.jsonnet", "jsonnetfile.json", "README.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}

	if err := apply.ApplyManifestsWithRunner(context.Background(), "/tmp/kube.yaml", dir, runner); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only 1 jsonnet + 1 kubectl call expected.
	if len(runner.calls) != 2 {
		t.Errorf("expected 2 runner calls (1 jsonnet + 1 kubectl), got %d", len(runner.calls))
	}
}
