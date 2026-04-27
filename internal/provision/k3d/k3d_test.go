package k3d

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/registry"
)

// stubRunner records calls and returns pre-configured (output, error) pairs.
type stubRunner struct {
	calls   []stubCall
	results []stubResult
}

type stubCall struct {
	name string
	args []string
}

type stubResult struct {
	output []byte
	err    error
}

func (s *stubRunner) next() stubResult {
	if len(s.results) == 0 {
		return stubResult{}
	}
	r := s.results[0]
	s.results = s.results[1:]
	return r
}

func (s *stubRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.calls = append(s.calls, stubCall{name: name, args: args})
	r := s.next()
	return r.output, r.err
}

func ok(output string) stubResult { return stubResult{output: []byte(output)} }
func fail(msg string) stubResult  { return stubResult{err: errors.New(msg)} }
func outErr(output, msg string) stubResult {
	return stubResult{output: []byte(output), err: errors.New(msg)}
}

// newDeps returns a Deps with K3dBin pre-set so tests skip binary
// resolution and the stubRunner handles all Run calls.
func newDeps(extra Deps) Deps {
	if extra.K3dBin == "" {
		extra.K3dBin = "k3d"
	}
	if extra.Out == nil {
		extra.Out = &bytes.Buffer{}
	}
	return extra
}

// ---- Provision tests -------------------------------------------------------

func TestProvision_HappyPath(t *testing.T) {
	dir := t.TempDir()
	kubeconfigPath := filepath.Join(dir, "test.yaml")

	stub := &stubRunner{results: []stubResult{
		ok(""),                              // cluster create
		ok("apiVersion: v1\nclusters: []\n"), // kubeconfig get
	}}

	p := New(newDeps(Deps{
		KubeconfigPath: kubeconfigPath,
		Runner:         stub,
	}))

	res, err := p.Provision(context.Background(), provision.ClusterConfig{ClusterName: "local"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.KubeconfigPath != kubeconfigPath {
		t.Errorf("KubeconfigPath = %q; want %q", res.KubeconfigPath, kubeconfigPath)
	}
	if len(res.Nodes) != 1 {
		t.Fatalf("len(Nodes) = %d; want 1", len(res.Nodes))
	}
	if res.Nodes[0].Role != "control-plane" {
		t.Errorf("Node[0].Role = %q; want control-plane", res.Nodes[0].Role)
	}

	if _, err := os.Stat(kubeconfigPath); err != nil {
		t.Errorf("kubeconfig not written: %v", err)
	}

	if len(stub.calls) != 2 {
		t.Fatalf("len(calls) = %d; want 2 (create + kubeconfig get)", len(stub.calls))
	}
	if stub.calls[0].args[0] != "cluster" || stub.calls[0].args[1] != "create" {
		t.Errorf("call[0] = %v; want cluster create", stub.calls[0].args)
	}
	if stub.calls[1].args[0] != "kubeconfig" {
		t.Errorf("call[1] arg[0] = %q; want kubeconfig", stub.calls[1].args[0])
	}
}

func TestProvision_ClusterCreateFails(t *testing.T) {
	stub := &stubRunner{results: []stubResult{
		fail("k3d: some fatal error"),
	}}
	p := New(newDeps(Deps{Runner: stub}))
	_, err := p.Provision(context.Background(), provision.ClusterConfig{ClusterName: "local"})
	if err == nil {
		t.Fatal("expected error when cluster create fails")
	}
	if !strings.Contains(err.Error(), "k3d cluster create") {
		t.Errorf("error = %q; want to contain 'k3d cluster create'", err.Error())
	}
}

func TestProvision_ClusterAlreadyExists_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	kubeconfigPath := filepath.Join(dir, "test.yaml")

	stub := &stubRunner{results: []stubResult{
		outErr("ERRO[0000] Cluster already exists", "exit status 1"),
		ok("apiVersion: v1\n"),
	}}
	p := New(newDeps(Deps{KubeconfigPath: kubeconfigPath, Runner: stub}))
	_, err := p.Provision(context.Background(), provision.ClusterConfig{ClusterName: "local"})
	if err != nil {
		t.Fatalf("Provision should be idempotent when cluster already exists: %v", err)
	}
}

func TestProvision_MultiNode(t *testing.T) {
	dir := t.TempDir()
	kubeconfigPath := filepath.Join(dir, "test.yaml")

	stub := &stubRunner{results: []stubResult{
		ok(""),
		ok("apiVersion: v1\n"),
	}}
	p := New(newDeps(Deps{
		Nodes:          3,
		KubeconfigPath: kubeconfigPath,
		Runner:         stub,
	}))
	res, err := p.Provision(context.Background(), provision.ClusterConfig{ClusterName: "multi"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(res.Nodes) != 3 {
		t.Fatalf("len(Nodes) = %d; want 3", len(res.Nodes))
	}
	if res.Nodes[0].Role != "control-plane" {
		t.Errorf("Node[0].Role = %q; want control-plane", res.Nodes[0].Role)
	}
	if res.Nodes[1].Role != "worker" {
		t.Errorf("Node[1].Role = %q; want worker", res.Nodes[1].Role)
	}

	createCall := stub.calls[0]
	found := false
	for i, a := range createCall.args {
		if a == "--agents" && i+1 < len(createCall.args) && createCall.args[i+1] == "2" {
			found = true
		}
	}
	if !found {
		t.Errorf("cluster create args missing --agents 2: %v", createCall.args)
	}
}

func TestProvision_K3sVersion(t *testing.T) {
	dir := t.TempDir()

	stub := &stubRunner{results: []stubResult{
		ok(""),
		ok("apiVersion: v1\n"),
	}}
	p := New(newDeps(Deps{
		K3sVersion:     "v1.28.3-k3s1",
		KubeconfigPath: filepath.Join(dir, "test.yaml"),
		Runner:         stub,
	}))
	_, err := p.Provision(context.Background(), provision.ClusterConfig{ClusterName: "local"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	createCall := stub.calls[0]
	found := false
	for i, a := range createCall.args {
		if a == "--image" && i+1 < len(createCall.args) {
			if strings.Contains(createCall.args[i+1], "v1.28.3-k3s1") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("cluster create args missing --image with version: %v", createCall.args)
	}
}

// ---- Destroy tests ---------------------------------------------------------

func TestDestroy_HappyPath(t *testing.T) {
	dir := t.TempDir()
	kubeconfigPath := filepath.Join(dir, "test.yaml")
	_ = os.WriteFile(kubeconfigPath, []byte("dummy"), 0o600)

	stub := &stubRunner{results: []stubResult{ok("")}}
	p := New(newDeps(Deps{Runner: stub}))
	err := p.Destroy(context.Background(), registry.Cluster{
		Name:           "local",
		KubeconfigPath: kubeconfigPath,
	})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(kubeconfigPath); !os.IsNotExist(err) {
		t.Error("kubeconfig was not removed after Destroy")
	}
}

func TestDestroy_ClusterNotFound_IsIdempotent(t *testing.T) {
	stub := &stubRunner{results: []stubResult{
		outErr("No cluster found with that name", "exit status 1"),
	}}
	p := New(newDeps(Deps{Runner: stub}))
	err := p.Destroy(context.Background(), registry.Cluster{Name: "gone"})
	if err != nil {
		t.Fatalf("Destroy should be idempotent when cluster not found: %v", err)
	}
}

// ---- Reconcile tests -------------------------------------------------------

func TestReconcile_ClusterPresent(t *testing.T) {
	stub := &stubRunner{results: []stubResult{
		ok(`[{"name":"local"},{"name":"other"}]`),
	}}
	p := New(newDeps(Deps{Runner: stub}))
	sum, err := p.Reconcile(context.Background(), "local")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if sum.Existing != 1 {
		t.Errorf("Existing = %d; want 1", sum.Existing)
	}
	if len(sum.Unmanaged) != 1 || sum.Unmanaged[0] != "other" {
		t.Errorf("Unmanaged = %v; want [other]", sum.Unmanaged)
	}
	if sum.MarkedDestroyed != 0 {
		t.Errorf("MarkedDestroyed = %d; want 0", sum.MarkedDestroyed)
	}
}

func TestReconcile_ClusterAbsent(t *testing.T) {
	stub := &stubRunner{results: []stubResult{
		ok(`[{"name":"other"}]`),
	}}
	p := New(newDeps(Deps{Runner: stub}))
	sum, err := p.Reconcile(context.Background(), "local")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if sum.MarkedDestroyed != 1 {
		t.Errorf("MarkedDestroyed = %d; want 1", sum.MarkedDestroyed)
	}
}

func TestReconcile_EmptyList(t *testing.T) {
	stub := &stubRunner{results: []stubResult{ok(`[]`)}}
	p := New(newDeps(Deps{Runner: stub}))
	sum, err := p.Reconcile(context.Background(), "local")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if sum.MarkedDestroyed != 1 {
		t.Errorf("MarkedDestroyed = %d; want 1", sum.MarkedDestroyed)
	}
}
