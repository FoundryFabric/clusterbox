package ufw

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

type fakeFS struct {
	mu    sync.Mutex
	files map[string]bool
}

func newFakeFS() *fakeFS { return &fakeFS{files: map[string]bool{}} }

func (f *fakeFS) addFile(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = true
}

type fakeFileInfo struct{ name string }

func (fi fakeFileInfo) Name() string       { return fi.name }
func (fi fakeFileInfo) Size() int64        { return 0 }
func (fi fakeFileInfo) Mode() fs.FileMode  { return 0o755 }
func (fi fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fi fakeFileInfo) IsDir() bool        { return false }
func (fi fakeFileInfo) Sys() any           { return nil }

func (f *fakeFS) Stat(path string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.files[path] {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
	}
	return fakeFileInfo{name: path}, nil
}

type fakeRunner struct {
	mu      sync.Mutex
	calls   []call
	runResp map[string]runResp
}

type runResp struct {
	out []byte
	err error
}

type call struct {
	name string
	args []string
	env  []string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{runResp: map[string]runResp{}}
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{name: name, args: args})
	resp, ok := f.runResp[name]
	f.mu.Unlock()
	if !ok {
		return nil, nil
	}
	return resp.out, resp.err
}

func (f *fakeRunner) RunEnv(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{name: name, args: args, env: env})
	resp, ok := f.runResp[name]
	f.mu.Unlock()
	if !ok {
		return nil, nil
	}
	return resp.out, resp.err
}

func (f *fakeRunner) callsFor(name string) []*call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*call
	for i := range f.calls {
		if f.calls[i].name == name {
			out = append(out, &f.calls[i])
		}
	}
	return out
}

// argsContain checks whether args contains needle as a contiguous
// subsequence — simpler than threading positional matches through every
// assertion.
func argsContain(args []string, needle ...string) bool {
	if len(needle) == 0 || len(args) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(args); i++ {
		ok := true
		for j := range needle {
			if args[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func enabledSpec(allowICMP bool) *config.Spec {
	return &config.Spec{Harden: &config.HardenSpec{
		Enabled:   true,
		User:      "ops",
		SSHPubKey: "ssh-ed25519 AAAA test@example",
		AllowICMP: allowICMP,
	}}
}

func TestApply_DisabledWhenSpecMissing(t *testing.T) {
	sec := &Section{Runner: newFakeRunner(), FS: newFakeFS()}
	res, err := sec.Apply(context.Background(), &config.Spec{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied || res.Reason != "disabled" {
		t.Errorf("res = %+v, want applied=false reason=disabled", res)
	}
}

func TestApply_FullRuleset(t *testing.T) {
	runner := newFakeRunner()
	fsys := newFakeFS()
	fsys.addFile(UfwBinary)

	sec := &Section{Runner: runner, FS: fsys}
	res, err := sec.Apply(context.Background(), enabledSpec(false))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied = false, want true")
	}
	if res.Extra["installed"] != false {
		t.Errorf("installed = %v, want false (binary already present)", res.Extra["installed"])
	}
	if res.Extra["ssh_from"] != TailscaleCGNAT {
		t.Errorf("ssh_from = %v, want %s", res.Extra["ssh_from"], TailscaleCGNAT)
	}

	ufwCalls := runner.callsFor("ufw")
	if len(ufwCalls) == 0 {
		t.Fatalf("no ufw calls were made, calls=%v", runner.calls)
	}

	// Required rules.
	wantRules := [][]string{
		{"default", "deny", "incoming"},
		{"default", "allow", "outgoing"},
		{"allow", "443/tcp"},
		{"allow", "41641/udp"},
		{"allow", "from", TailscaleCGNAT},
		{"--force", "enable"},
	}
	for _, want := range wantRules {
		var found bool
		for _, c := range ufwCalls {
			if argsContain(c.args, want...) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ufw call missing args sequence %v", want)
		}
	}

	// ICMP must NOT be allowed when AllowICMP is false.
	for _, c := range ufwCalls {
		if argsContain(c.args, "proto", "icmp") {
			t.Errorf("icmp should not be allowed when AllowICMP=false, got call args=%v", c.args)
		}
	}
}

func TestApply_AllowICMPAddsIcmpRule(t *testing.T) {
	runner := newFakeRunner()
	fsys := newFakeFS()
	fsys.addFile(UfwBinary)
	sec := &Section{Runner: runner, FS: fsys}

	if _, err := sec.Apply(context.Background(), enabledSpec(true)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var saw bool
	for _, c := range runner.callsFor("ufw") {
		if argsContain(c.args, "proto", "icmp") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected an icmp rule when AllowICMP=true")
	}
}

func TestApply_InstallsUfwWhenMissing(t *testing.T) {
	runner := newFakeRunner()
	fsys := newFakeFS() // ufw binary absent
	sec := &Section{Runner: runner, FS: fsys}

	res, err := sec.Apply(context.Background(), enabledSpec(false))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Extra["installed"] != true {
		t.Errorf("installed = %v, want true", res.Extra["installed"])
	}
	apt := runner.callsFor("apt-get")
	if len(apt) < 2 {
		t.Fatalf("expected apt-get update + install, got %d calls", len(apt))
	}
	// At least one apt-get invocation must include "install" + "ufw".
	var sawInstall bool
	for _, c := range apt {
		if argsContain(c.args, "install") && argsContain(c.args, "ufw") {
			sawInstall = true
			// DEBIAN_FRONTEND must be set to non-interactive.
			var sawEnv bool
			for _, e := range c.env {
				if strings.HasPrefix(e, "DEBIAN_FRONTEND=") && strings.Contains(e, "noninteractive") {
					sawEnv = true
				}
			}
			if !sawEnv {
				t.Errorf("apt-get install ufw missing DEBIAN_FRONTEND=noninteractive, env=%v", c.env)
			}
		}
	}
	if !sawInstall {
		t.Errorf("apt-get install ufw was not invoked")
	}
}

func TestApply_RuleFailureSurfaces(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["ufw"] = runResp{err: errors.New("ufw broke")}
	fsys := newFakeFS()
	fsys.addFile(UfwBinary)
	sec := &Section{Runner: runner, FS: fsys}

	_, err := sec.Apply(context.Background(), enabledSpec(false))
	if err == nil {
		t.Fatal("expected rule error")
	}
	if !strings.Contains(err.Error(), "ufw broke") {
		t.Errorf("error %q should mention underlying ufw failure", err)
	}
}

func TestRemove_NoOp(t *testing.T) {
	sec := &Section{Runner: newFakeRunner(), FS: newFakeFS()}
	res, err := sec.Remove(context.Background(), enabledSpec(false))
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if res.Applied {
		t.Errorf("Applied = true, want false (Remove is a no-op for v1)")
	}
}

func TestStatusActive(t *testing.T) {
	if !statusActive([]byte("Status: active\n")) {
		t.Error("statusActive should detect active status")
	}
	if statusActive([]byte("Status: inactive\n")) {
		t.Error("statusActive false-positive on inactive")
	}
}
