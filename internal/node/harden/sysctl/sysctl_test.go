package sysctl

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
	files map[string][]byte
}

func newFakeFS() *fakeFS { return &fakeFS{files: map[string][]byte{}} }

func (f *fakeFS) seed(path string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = append([]byte(nil), data...)
}

type fakeFileInfo struct{}

func (fakeFileInfo) Name() string       { return "" }
func (fakeFileInfo) Size() int64        { return 0 }
func (fakeFileInfo) Mode() fs.FileMode  { return 0o644 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() any           { return nil }

func (f *fakeFS) Stat(p string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[p]; ok {
		return fakeFileInfo{}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
}

func (f *fakeFS) ReadFile(p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.files[p]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	return append([]byte(nil), d...), nil
}

func (f *fakeFS) WriteFile(p string, d []byte, _ fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = append([]byte(nil), d...)
	return nil
}

func (f *fakeFS) MkdirAll(p string, _ fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[p]; !ok {
		f.files[p] = nil
	}
	return nil
}

type fakeRunner struct {
	mu    sync.Mutex
	calls []fakeCall
	errs  map[string]error
}

type fakeCall struct {
	name string
	args []string
}

func newFakeRunner() *fakeRunner { return &fakeRunner{errs: map[string]error{}} }

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, fakeCall{name: name, args: args})
	err := r.errs[name]
	r.mu.Unlock()
	return nil, err
}

func (r *fakeRunner) callsFor(name string) []fakeCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []fakeCall
	for _, c := range r.calls {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

func argsContain(args []string, needle ...string) bool {
	for i := 0; i+len(needle) <= len(args); i++ {
		match := true
		for j, n := range needle {
			if args[i+j] != n {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func enabledSpec() *config.Spec {
	return &config.Spec{Harden: &config.HardenSpec{Enabled: true, User: "ops"}}
}

func TestApply_Disabled(t *testing.T) {
	sec := &Section{Runner: newFakeRunner(), FS: newFakeFS()}
	res, err := sec.Apply(context.Background(), &config.Spec{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied || res.Reason != "disabled" {
		t.Errorf("res = %+v, want applied=false reason=disabled", res)
	}
}

func TestApply_WritesConfFile(t *testing.T) {
	runner := newFakeRunner()
	sec := &Section{Runner: runner, FS: newFakeFS()}

	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied = false, want true")
	}
	if res.Extra["conf_written"] != true {
		t.Errorf("conf_written = %v, want true", res.Extra["conf_written"])
	}
}

func TestApply_ConfContentContainsExpectedKeys(t *testing.T) {
	fsys := newFakeFS()
	sec := &Section{Runner: newFakeRunner(), FS: fsys}
	if _, err := sec.Apply(context.Background(), enabledSpec()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	data, err := fsys.ReadFile(ConfPath)
	if err != nil {
		t.Fatalf("conf not written: %v", err)
	}
	for _, key := range []string{
		"net.ipv4.conf.all.rp_filter",
		"net.ipv4.tcp_syncookies",
		"kernel.dmesg_restrict",
		"kernel.kptr_restrict",
		"fs.suid_dumpable",
	} {
		if !strings.Contains(string(data), key) {
			t.Errorf("conf missing key %q", key)
		}
	}
}

func TestApply_SkipsWriteWhenConfUnchanged(t *testing.T) {
	fsys := newFakeFS()
	fsys.seed(ConfPath, confPayload)
	runner := newFakeRunner()

	sec := &Section{Runner: runner, FS: fsys}
	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Extra["conf_written"] != false {
		t.Errorf("conf_written = %v, want false (file already up to date)", res.Extra["conf_written"])
	}
}

func TestApply_AlwaysRunsSysctl(t *testing.T) {
	// Even when the conf file is already up to date, sysctl --system must run.
	fsys := newFakeFS()
	fsys.seed(ConfPath, confPayload)
	runner := newFakeRunner()

	sec := &Section{Runner: runner, FS: fsys}
	if _, err := sec.Apply(context.Background(), enabledSpec()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	calls := runner.callsFor("sysctl")
	var sawSystem bool
	for _, c := range calls {
		if argsContain(c.args, "--system") {
			sawSystem = true
		}
	}
	if !sawSystem {
		t.Errorf("sysctl --system not called")
	}
}

func TestApply_SysctlErrorSurfaces(t *testing.T) {
	runner := newFakeRunner()
	runner.errs["sysctl"] = errors.New("permission denied")

	sec := &Section{Runner: runner, FS: newFakeFS()}
	_, err := sec.Apply(context.Background(), enabledSpec())
	if err == nil {
		t.Fatal("expected error from sysctl")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error %q missing cause", err)
	}
}

func TestRemove_NoOp(t *testing.T) {
	sec := &Section{Runner: newFakeRunner(), FS: newFakeFS()}
	res, err := sec.Remove(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if res.Applied {
		t.Errorf("Applied = true, want false")
	}
}
