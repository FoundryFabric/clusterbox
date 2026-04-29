package k3s

import (
	"context"
	"errors"
	"io/fs"
	"os"
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

func (f *fakeFS) WriteFile(path string, data []byte, _ os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	f.files[path] = cp
	return nil
}

func (f *fakeFS) MkdirAll(_ string, _ os.FileMode) error { return nil }

// fakeRunner records every call and returns programmable responses.
// defaultOK=true makes any unmapped Run call return nil,nil so tests only need
// to stub the calls they care about.
type fakeRunner struct {
	mu        sync.Mutex
	calls     []call
	defaultOK bool
	// runResp keys on "name arg0 arg1..." (full command) with fallback to "name".
	runResp map[string]runResp
}

type runResp struct {
	out []byte
	err error
}

type call struct {
	kind string
	name string
	args []string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{runResp: map[string]runResp{}}
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{kind: "run", name: name, args: args})
	fullKey := strings.Join(append([]string{name}, args...), " ")
	resp, ok := f.runResp[fullKey]
	if !ok {
		resp, ok = f.runResp[name]
	}
	defaultOK := f.defaultOK
	f.mu.Unlock()
	if !ok {
		if defaultOK {
			return nil, nil
		}
		return nil, errors.New("no fake response for " + fullKey)
	}
	return resp.out, resp.err
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

// newTestSection returns a Section preconfigured for fast polling with a
// no-op Downloader that writes a fake binary into fsys.
func newTestSection(r *fakeRunner, fsys *fakeFS) *Section {
	r.defaultOK = true
	return &Section{
		Runner:       r,
		FS:           fsys,
		PollInterval: time.Millisecond,
		PollTimeout:  100 * time.Millisecond,
		Arch:         "amd64",
		Downloader: func(_ context.Context, _, dest string, _ os.FileMode) error {
			fsys.set(dest, []byte("fake-k3s"))
			return nil
		},
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
	fsys := newFakeFS()
	// waitForAgent: kubelet.kubeconfig present → agent joined immediately.
	fsys.set(AgentKubeletKubeconfig, []byte("apiVersion: v1\n"))

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

	// Verify the env file was written with the correct URL and token.
	envData, err := fsys.ReadFile(AgentEnvPath)
	if err != nil {
		t.Fatalf("ReadFile AgentEnvPath: %v", err)
	}
	envStr := string(envData)
	if !strings.Contains(envStr, "K3S_URL=https://10.100.0.1:6443") {
		t.Errorf("env file missing K3S_URL, got: %s", envStr)
	}
	if !strings.Contains(envStr, "K3S_TOKEN=secret") {
		t.Errorf("env file missing K3S_TOKEN, got: %s", envStr)
	}
}

func TestApply_AgentJoinTimeout(t *testing.T) {
	runner := newFakeRunner()
	// alreadyInstalled: fresh node.
	runner.runResp["systemctl is-active k3s"] = runResp{err: errors.New("exit 3")}
	// Diagnostic command responses (collectAgentDiagnostics after Phase 1 timeout).
	runner.runResp["systemctl"] = runResp{out: []byte("● k3s-agent.service - failed\n")}
	runner.runResp["journalctl"] = runResp{out: []byte("k3s-agent[123]: connection refused\n")}
	runner.runResp["ip"] = runResp{out: []byte("lo: inet 127.0.0.1\n")}
	runner.runResp["curl"] = runResp{err: errors.New("connection refused")}
	fsys := newFakeFS()
	// AgentKubeletKubeconfig is never written → Phase 1 of waitForAgent times out.

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
		t.Fatal("expected error when k3s-agent fails to bootstrap")
	}
	if !strings.Contains(err.Error(), "k3s-agent") {
		t.Errorf("error %q should mention k3s-agent", err)
	}
}

func TestApply_AgentRegistrationTimeout(t *testing.T) {
	runner := newFakeRunner()
	// alreadyInstalled: fresh node.
	runner.runResp["systemctl is-active k3s"] = runResp{err: errors.New("exit 3")}
	// Phase 1 passes: kubelet.kubeconfig is pre-set in the FS.
	// Phase 2 fails: hostname returns a name but kubectl always errors → timeout.
	runner.runResp["hostname"] = runResp{out: []byte("test-worker\n")}
	runner.runResp["kubectl"] = runResp{err: errors.New(`nodes "test-worker" not found`)}
	// Diagnostic command responses.
	runner.runResp["systemctl"] = runResp{out: []byte("● k3s-agent.service\n")}
	runner.runResp["journalctl"] = runResp{out: []byte("kubelet: connection refused\n")}
	runner.runResp["ip"] = runResp{out: []byte("lo: inet 127.0.0.1\n")}
	runner.runResp["curl"] = runResp{err: errors.New("connection refused")}

	fsys := newFakeFS()
	fsys.set(AgentKubeletKubeconfig, []byte("apiVersion: v1\n")) // Phase 1 passes immediately

	spec := &config.Spec{K3s: &config.K3sSpec{
		Enabled:   true,
		Role:      "agent",
		Version:   "v1.30.0+k3s1",
		ServerURL: "https://10.0.2.2:16443",
		Token:     "secret",
	}}
	sec := newTestSection(runner, fsys) // PollTimeout: 100ms
	_, err := sec.Apply(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error when node registration times out")
	}
	if !strings.Contains(err.Error(), "did not register") {
		t.Errorf("error %q should mention registration", err)
	}
}

func TestApply_FreshInstallEmitsFullPayload(t *testing.T) {
	runner := newFakeRunner()
	// systemctl is-active should report inactive so the fresh-install path is exercised.
	runner.runResp["systemctl is-active k3s"] = runResp{err: errors.New("exit 3")}
	fsys := newFakeFS()

	var downloadedURL string
	sec := newTestSection(runner, fsys)
	sec.Downloader = func(_ context.Context, url, dest string, _ os.FileMode) error {
		downloadedURL = url
		fsys.set(dest, []byte("fake-k3s"))
		fsys.set(KubeconfigPath, []byte("apiVersion: v1\nkind: Config\n"))
		fsys.set(NodeTokenPath, []byte("K10::server:secret\n"))
		return nil
	}

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
	if !strings.Contains(downloadedURL, "v1.30.0%2Bk3s1") {
		t.Errorf("download URL %q should contain URL-encoded version", downloadedURL)
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
	// Downloader must NOT have run.
	if _, err := fsys.ReadFile(AgentServicePath); err == nil {
		t.Errorf("service file should not be written when k3s already present")
	}
}

func TestApply_IdempotentWhenSystemdActive(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["systemctl is-active k3s"] = runResp{out: []byte("active\n")}
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
}

func TestApply_KubeconfigTimeout(t *testing.T) {
	runner := newFakeRunner()
	runner.defaultOK = true
	runner.runResp["systemctl is-active k3s"] = runResp{err: errors.New("inactive")}
	fsys := newFakeFS()

	sec := &Section{
		Runner:       runner,
		FS:           fsys,
		PollInterval: time.Millisecond,
		PollTimeout:  10 * time.Millisecond,
		Arch:         "amd64",
		// Downloader writes the binary but not the kubeconfig.
		Downloader: func(_ context.Context, _, dest string, _ os.FileMode) error {
			fsys.set(dest, []byte("fake-k3s"))
			return nil
		},
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
	runner.defaultOK = true
	runner.runResp["systemctl is-active k3s"] = runResp{err: errors.New("inactive")}
	fsys := newFakeFS()

	sec := &Section{
		Runner:       runner,
		FS:           fsys,
		PollInterval: time.Millisecond,
		PollTimeout:  500 * time.Millisecond,
		Arch:         "amd64",
		Downloader: func(_ context.Context, _, dest string, _ os.FileMode) error {
			fsys.set(dest, []byte("fake-k3s"))
			fsys.set(KubeconfigPath, []byte("apiVersion: v1\n"))
			// node-token shows up a few polls later, simulating the install race.
			go func() {
				time.Sleep(5 * time.Millisecond)
				fsys.set(NodeTokenPath, []byte("racy-token\n"))
			}()
			return nil
		},
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
	runner.defaultOK = true
	runner.runResp["systemctl is-active k3s"] = runResp{err: errors.New("inactive")}
	fsys := newFakeFS()

	sec := &Section{
		Runner:       runner,
		FS:           fsys,
		PollInterval: time.Millisecond,
		PollTimeout:  500 * time.Millisecond,
		Arch:         "amd64",
		Downloader: func(_ context.Context, _, dest string, _ os.FileMode) error {
			fsys.set(dest, []byte("fake-k3s"))
			fsys.set(KubeconfigPath, []byte("apiVersion: v1\n"))
			// Token file present but empty initially.
			fsys.set(NodeTokenPath, []byte(""))
			go func() {
				time.Sleep(5 * time.Millisecond)
				fsys.set(NodeTokenPath, []byte("eventually"))
			}()
			return nil
		},
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
	runner.defaultOK = true
	runner.runResp["systemctl is-active k3s"] = runResp{err: errors.New("inactive")}
	fsys := newFakeFS()

	sec := &Section{
		Runner:       runner,
		FS:           fsys,
		PollInterval: 10 * time.Millisecond,
		PollTimeout:  10 * time.Second,
		Arch:         "amd64",
		// Downloader writes only the binary — kubeconfig never appears, forcing the wait loop.
		Downloader: func(_ context.Context, _, dest string, _ os.FileMode) error {
			fsys.set(dest, []byte("fake-k3s"))
			return nil
		},
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

func TestApply_DownloadFailureSurfaces(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["systemctl is-active k3s"] = runResp{err: errors.New("inactive")}
	fsys := newFakeFS()

	sec := newTestSection(runner, fsys)
	sec.Downloader = func(_ context.Context, _ string, _ string, _ os.FileMode) error {
		return errors.New("connection reset by peer")
	}
	_, err := sec.Apply(context.Background(), enabledSpec())
	if err == nil {
		t.Fatal("expected download error")
	}
	if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Errorf("error %q should mention download failure", err)
	}
}

func TestApply_ServerInitMapsToControlPlane(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["systemctl is-active k3s"] = runResp{err: errors.New("inactive")}
	fsys := newFakeFS()

	sec := newTestSection(runner, fsys)
	sec.Downloader = func(_ context.Context, _, dest string, _ os.FileMode) error {
		fsys.set(dest, []byte("fake-k3s"))
		fsys.set(KubeconfigPath, []byte("apiVersion: v1\n"))
		fsys.set(NodeTokenPath, []byte("t"))
		return nil
	}
	spec := &config.Spec{K3s: &config.K3sSpec{Enabled: true, Role: "server-init", Version: "v1"}}
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
	runner.runResp["systemctl is-active k3s"] = runResp{err: errors.New("inactive")}
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

func TestRemove_StopsAndCleansUp(t *testing.T) {
	runner := newFakeRunner()
	fsys := newFakeFS()
	fsys.set(K3sBinary, []byte("k3s-bin"))

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
	// Verify service was stopped.
	if c := runner.findCall("run", "systemctl"); c == nil || c.args[0] != "stop" {
		t.Errorf("expected systemctl stop call, calls=%v", runner.calls)
	}
}

// Avoid the unused import lint when fsys.delete is the only consumer in
// some test variants.
var _ = (*fakeFS).delete
