package k3s

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/node/config"
)

// fakeFS is a minimal FS implementation backed by an in-memory map. It is
// safe for concurrent access because waitForFile may race a goroutine
// "creating" the file partway through the poll loop.
type fakeFS struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newFakeFS() *fakeFS { return &fakeFS{files: map[string][]byte{}} }

func (f *fakeFS) set(path string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = data
}

func (f *fakeFS) delete(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, path)
}

type fakeFileInfo struct {
	name string
	size int64
}

func (fi fakeFileInfo) Name() string       { return fi.name }
func (fi fakeFileInfo) Size() int64        { return fi.size }
func (fi fakeFileInfo) Mode() fs.FileMode  { return 0o644 }
func (fi fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fi fakeFileInfo) IsDir() bool        { return false }
func (fi fakeFileInfo) Sys() any           { return nil }

func (f *fakeFS) Stat(path string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
	}
	return fakeFileInfo{name: path, size: int64(len(data))}, nil
}

func (f *fakeFS) ReadFile(path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

// fakeRunner records every call and returns programmable responses.
type fakeRunner struct {
	mu       sync.Mutex
	calls    []call
	shellOut []byte
	shellErr error
	// runResp keys on the first arg to Run (e.g. "systemctl").
	runResp map[string]runResp

	// onShell, when non-nil, fires after the installer call so tests can
	// simulate the kubeconfig appearing mid-install.
	onShell func()
}

type runResp struct {
	out []byte
	err error
}

type call struct {
	kind string
	name string
	args []string
	env  []string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{runResp: map[string]runResp{}}
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{kind: "run", name: name, args: args})
	// Full key (name + all args joined) takes priority over name-only so tests
	// can distinguish e.g. "systemctl is-active k3s" from "systemctl is-active k3s-agent".
	fullKey := strings.Join(append([]string{name}, args...), " ")
	resp, ok := f.runResp[fullKey]
	if !ok {
		resp, ok = f.runResp[name]
	}
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("no fake response for " + name)
	}
	return resp.out, resp.err
}

func (f *fakeRunner) RunShell(_ context.Context, env []string, _ string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{kind: "shell", env: env})
	cb := f.onShell
	f.mu.Unlock()
	if cb != nil {
		cb()
	}
	return f.shellOut, f.shellErr
}

func (f *fakeRunner) findCall(kind, name string) *call {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.calls {
		c := &f.calls[i]
		if c.kind == kind && (name == "" || c.name == name) {
			return c
		}
	}
	return nil
}

// helper: a Section preconfigured for fast polling so tests don't sleep for
// real seconds when waiting on a file.
func newTestSection(r Runner, fsys FS) *Section {
	return &Section{
		Runner:       r,
		FS:           fsys,
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
	}
}

func enabledSpec() *config.Spec {
	return &config.Spec{K3s: &config.K3sSpec{Enabled: true, Role: "server", Version: "v1.30.0+k3s1"}}
}

func TestApply_DisabledWhenSpecMissing(t *testing.T) {
	sec := newTestSection(newFakeRunner(), newFakeFS())
	res, err := sec.Apply(context.Background(), &config.Spec{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied || res.Reason != "disabled" {
		t.Errorf("res = %+v, want applied=false reason=disabled", res)
	}
}

func TestApply_DisabledWhenEnabledFalse(t *testing.T) {
	sec := newTestSection(newFakeRunner(), newFakeFS())
	spec := &config.Spec{K3s: &config.K3sSpec{Enabled: false}}
	res, err := sec.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied || res.Reason != "disabled" {
		t.Errorf("res = %+v, want applied=false reason=disabled", res)
	}
}

func TestApply_AgentRole(t *testing.T) {
	runner := newFakeRunner()
	// alreadyInstalled: binary absent, k3s service inactive → run installer.
	runner.runResp["systemctl is-active k3s"] = runResp{err: errors.New("exit 3")}
	// waitForAgent: k3s-agent joins immediately.
	runner.runResp["systemctl is-active k3s-agent"] = runResp{out: []byte("active\n")}
	fsys := newFakeFS()

	spec := &config.Spec{K3s: &config.K3sSpec{
		Enabled:   true,
		Role:      "agent",
		Version:   "v1.30.0+k3s1",
		ServerURL: "https://10.100.0.1:6443",
		Token:     "secret",
	}}
	sec := newTestSection(runner, fsys)
	res, err := sec.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied = false, want true")
	}
	if res.Extra["role"] != "worker" {
		t.Errorf("role = %v, want worker", res.Extra["role"])
	}

	c := runner.findCall("shell", "")
	if c == nil {
		t.Fatal("installer not called for agent")
	}
	envMap := make(map[string]string)
	for _, e := range c.env {
		if parts := strings.SplitN(e, "=", 2); len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}
	if envMap["K3S_URL"] != "https://10.100.0.1:6443" {
		t.Errorf("K3S_URL = %q, want https://10.100.0.1:6443", envMap["K3S_URL"])
	}
	if envMap["K3S_TOKEN"] != "secret" {
		t.Errorf("K3S_TOKEN = %q, want secret", envMap["K3S_TOKEN"])
	}
}

func TestApply_AgentJoinTimeout(t *testing.T) {
	runner := newFakeRunner()
	// alreadyInstalled: fresh node.
	runner.runResp["systemctl is-active k3s"] = runResp{err: errors.New("exit 3")}
	// waitForAgent: k3s-agent never becomes active.
	runner.runResp["systemctl is-active k3s-agent"] = runResp{out: []byte("activating\n"), err: errors.New("exit 3")}
	// Diagnostic command responses (needed so fakeRunner doesn't error on them).
	runner.runResp["systemctl"] = runResp{out: []byte("● k3s-agent.service - failed\n")}
	runner.runResp["journalctl"] = runResp{out: []byte("k3s-agent[123]: connection refused\n")}
	runner.runResp["ip"] = runResp{out: []byte("lo: inet 127.0.0.1\n")}
	runner.runResp["curl"] = runResp{err: errors.New("connection refused")}
	fsys := newFakeFS()

	spec := &config.Spec{K3s: &config.K3sSpec{
		Enabled:   true,
		Role:      "agent",
		Version:   "v1.30.0+k3s1",
		ServerURL: "https://10.0.2.2:16443",
		Token:     "secret",
	}}
	sec := newTestSection(runner, fsys) // PollTimeout: 100ms — fast timeout
	_, err := sec.Apply(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error when k3s-agent fails to join")
	}
	if !strings.Contains(err.Error(), "k3s-agent") {
		t.Errorf("error %q should mention k3s-agent", err)
	}
}

func TestApply_FreshInstallEmitsFullPayload(t *testing.T) {
	runner := newFakeRunner()
	// systemctl is-active should report inactive so the fresh-install path
	// is exercised.
	runner.runResp["systemctl"] = runResp{out: []byte("inactive\n"), err: errors.New("exit 3")}
	fsys := newFakeFS()

	// Simulate the installer creating the kubeconfig+token mid-call.
	runner.onShell = func() {
		fsys.set(K3sBinary, []byte("k3s-bin"))
		fsys.set(KubeconfigPath, []byte("apiVersion: v1\nkind: Config\n"))
		fsys.set(NodeTokenPath, []byte("K10::server:secret\n"))
	}

	sec := newTestSection(runner, fsys)
	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied = false, want true")
	}
	if res.Extra["role"] != "control-plane" {
		t.Errorf("role = %v, want control-plane", res.Extra["role"])
	}
	if res.Extra["k3s_version"] != "v1.30.0+k3s1" {
		t.Errorf("k3s_version = %v", res.Extra["k3s_version"])
	}
	if !strings.Contains(res.Extra["kubeconfig_yaml"].(string), "apiVersion") {
		t.Errorf("kubeconfig_yaml = %v", res.Extra["kubeconfig_yaml"])
	}
	if res.Extra["node_token"] != "K10::server:secret" {
		t.Errorf("node_token = %v (should have trailing newline trimmed)", res.Extra["node_token"])
	}
	if res.Extra["server_url"] != DefaultServerURL {
		t.Errorf("server_url = %v", res.Extra["server_url"])
	}

	// Verify the installer was invoked with INSTALL_K3S_VERSION set.
	c := runner.findCall("shell", "")
	if c == nil {
		t.Fatal("installer shell call not made")
	}
	var sawVersion bool
	for _, e := range c.env {
		if e == "INSTALL_K3S_VERSION=v1.30.0+k3s1" {
			sawVersion = true
		}
	}
	if !sawVersion {
		t.Errorf("INSTALL_K3S_VERSION env not set, env=%v", c.env)
	}
}

func TestApply_IdempotentWhenBinaryPresent(t *testing.T) {
	runner := newFakeRunner()
	fsys := newFakeFS()
	// Pre-existing install: binary plus kubeconfig and token.
	fsys.set(K3sBinary, []byte("k3s-bin"))
	fsys.set(KubeconfigPath, []byte("apiVersion: v1\n"))
	fsys.set(NodeTokenPath, []byte("token\n"))

	sec := newTestSection(runner, fsys)
	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied || res.Reason != "already installed" {
		t.Errorf("res = %+v, want applied=true reason='already installed'", res)
	}
	// Crucially, the installer must NOT have run.
	if c := runner.findCall("shell", ""); c != nil {
		t.Errorf("installer should not run when k3s already present, got %+v", c)
	}
}

func TestApply_IdempotentWhenSystemdActive(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["systemctl"] = runResp{out: []byte("active\n"), err: nil}
	fsys := newFakeFS()
	fsys.set(KubeconfigPath, []byte("apiVersion: v1\n"))
	fsys.set(NodeTokenPath, []byte("token"))

	sec := newTestSection(runner, fsys)
	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied || res.Reason != "already installed" {
		t.Errorf("res = %+v, want applied=true reason='already installed'", res)
	}
	if c := runner.findCall("shell", ""); c != nil {
		t.Errorf("installer should not run when systemd reports active")
	}
}

func TestApply_KubeconfigTimeout(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["systemctl"] = runResp{err: errors.New("inactive")}
	fsys := newFakeFS()
	// Installer "succeeds" but never creates the kubeconfig.
	runner.onShell = func() {
		fsys.set(K3sBinary, []byte("k3s-bin"))
	}

	sec := &Section{
		Runner:       runner,
		FS:           fsys,
		PollInterval: time.Millisecond,
		PollTimeout:  10 * time.Millisecond,
	}
	_, err := sec.Apply(context.Background(), enabledSpec())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), KubeconfigPath) {
		t.Errorf("error %q should mention %s", err, KubeconfigPath)
	}
}

func TestApply_NodeTokenAppearsAfterKubeconfig(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["systemctl"] = runResp{err: errors.New("inactive")}
	fsys := newFakeFS()
	runner.onShell = func() {
		fsys.set(K3sBinary, []byte("k3s-bin"))
		fsys.set(KubeconfigPath, []byte("apiVersion: v1\n"))
		// node-token shows up a few polls later, simulating the install race.
		go func() {
			time.Sleep(5 * time.Millisecond)
			fsys.set(NodeTokenPath, []byte("racy-token\n"))
		}()
	}

	sec := &Section{
		Runner:       runner,
		FS:           fsys,
		PollInterval: time.Millisecond,
		PollTimeout:  500 * time.Millisecond,
	}
	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Extra["node_token"] != "racy-token" {
		t.Errorf("node_token = %v, want racy-token", res.Extra["node_token"])
	}
}

func TestApply_EmptyTokenIsRetried(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["systemctl"] = runResp{err: errors.New("inactive")}
	fsys := newFakeFS()
	runner.onShell = func() {
		fsys.set(K3sBinary, []byte("k3s-bin"))
		fsys.set(KubeconfigPath, []byte("apiVersion: v1\n"))
		// Token file present but empty initially — simulating the
		// installer creating the file before writing to it.
		fsys.set(NodeTokenPath, []byte(""))
		go func() {
			time.Sleep(5 * time.Millisecond)
			fsys.set(NodeTokenPath, []byte("eventually"))
		}()
	}
	sec := &Section{
		Runner:       runner,
		FS:           fsys,
		PollInterval: time.Millisecond,
		PollTimeout:  500 * time.Millisecond,
	}
	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Extra["node_token"] != "eventually" {
		t.Errorf("node_token = %v, want eventually", res.Extra["node_token"])
	}
}

func TestApply_ContextCancelled(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["systemctl"] = runResp{err: errors.New("inactive")}
	fsys := newFakeFS()
	runner.onShell = func() {
		fsys.set(K3sBinary, []byte("k3s-bin"))
		// Don't create the kubeconfig — force the wait loop.
	}
	sec := &Section{
		Runner:       runner,
		FS:           fsys,
		PollInterval: 10 * time.Millisecond,
		PollTimeout:  10 * time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := sec.Apply(ctx, enabledSpec())
	if err == nil {
		t.Fatal("expected context-cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %q should wrap context.Canceled", err)
	}
}

func TestApply_InstallerFailureSurfaces(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["systemctl"] = runResp{err: errors.New("inactive")}
	runner.shellErr = errors.New("curl exit 22")
	fsys := newFakeFS()

	sec := newTestSection(runner, fsys)
	_, err := sec.Apply(context.Background(), enabledSpec())
	if err == nil {
		t.Fatal("expected installer error")
	}
	if !strings.Contains(err.Error(), "curl exit 22") {
		t.Errorf("error %q should mention installer stderr", err)
	}
}

func TestApply_ServerInitMapsToControlPlane(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["systemctl"] = runResp{err: errors.New("inactive")}
	fsys := newFakeFS()
	runner.onShell = func() {
		fsys.set(K3sBinary, []byte("k3s-bin"))
		fsys.set(KubeconfigPath, []byte("apiVersion: v1\n"))
		fsys.set(NodeTokenPath, []byte("t"))
	}
	spec := &config.Spec{K3s: &config.K3sSpec{Enabled: true, Role: "server-init", Version: "v1"}}
	sec := newTestSection(runner, fsys)
	res, err := sec.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Extra["role"] != "control-plane" {
		t.Errorf("role = %v, want control-plane", res.Extra["role"])
	}
}

func TestRemove_NoOpWhenAbsent(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["systemctl"] = runResp{err: errors.New("inactive")}
	fsys := newFakeFS()
	sec := newTestSection(runner, fsys)
	res, err := sec.Remove(context.Background(), &config.Spec{})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if res.Applied {
		t.Errorf("Applied = true, want false (k3s was never installed)")
	}
	if res.Extra["k3s_was_present"] != false {
		t.Errorf("k3s_was_present = %v, want false", res.Extra["k3s_was_present"])
	}
	if res.Extra["removed"] != true {
		t.Errorf("removed = %v, want true", res.Extra["removed"])
	}
}

func TestRemove_RunsUninstallScript(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp[K3sUninstallSh] = runResp{out: []byte("uninstalled\n")}
	fsys := newFakeFS()
	fsys.set(K3sBinary, []byte("k3s-bin"))
	fsys.set(K3sUninstallSh, []byte("#!/bin/sh\n"))

	sec := newTestSection(runner, fsys)
	res, err := sec.Remove(context.Background(), &config.Spec{})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied = false, want true")
	}
	if res.Extra["removed"] != true || res.Extra["k3s_was_present"] != true {
		t.Errorf("Extra = %v", res.Extra)
	}
	if c := runner.findCall("run", K3sUninstallSh); c == nil {
		t.Errorf("uninstall script was not invoked, calls=%v", runner.calls)
	}
}

func TestRemove_MissingUninstallScriptIsAnError(t *testing.T) {
	runner := newFakeRunner()
	fsys := newFakeFS()
	// Binary present but no uninstall script — ambiguous state.
	fsys.set(K3sBinary, []byte("k3s-bin"))

	sec := newTestSection(runner, fsys)
	_, err := sec.Remove(context.Background(), &config.Spec{})
	if err == nil {
		t.Fatal("expected error when uninstall script is missing")
	}
	if !strings.Contains(err.Error(), K3sUninstallSh) {
		t.Errorf("error %q should mention %s", err, K3sUninstallSh)
	}
}

func TestRemove_UninstallScriptFailureSurfaces(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp[K3sUninstallSh] = runResp{err: errors.New("permission denied")}
	fsys := newFakeFS()
	fsys.set(K3sBinary, []byte("k3s-bin"))
	fsys.set(K3sUninstallSh, []byte("#!/bin/sh\n"))

	sec := newTestSection(runner, fsys)
	_, err := sec.Remove(context.Background(), &config.Spec{})
	if err == nil {
		t.Fatal("expected uninstall-script error")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error %q should mention underlying failure", err)
	}
}

// Avoid the unused import lint when fsys.delete is the only consumer in
// some test variants.
var _ = (*fakeFS).delete
