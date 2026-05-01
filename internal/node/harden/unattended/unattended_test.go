package unattended

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/node/distro"
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
	env  []string
}

func newFakeRunner() *fakeRunner { return &fakeRunner{errs: map[string]error{}} }

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, fakeCall{name: name, args: args})
	err := r.errs[name]
	r.mu.Unlock()
	return nil, err
}

func (r *fakeRunner) RunEnv(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, fakeCall{name: name, args: args, env: env})
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

func TestApply_BinaryAlreadyPresent(t *testing.T) {
	fsys := newFakeFS()
	fsys.seed("/usr/bin/unattended-upgrade", []byte("bin"))
	runner := newFakeRunner()

	sec := &Section{Runner: runner, FS: fsys}
	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied = false, want true")
	}
	if res.Extra["installed"] != false {
		t.Errorf("installed = %v, want false", res.Extra["installed"])
	}
	if len(runner.callsFor("apt-get")) != 0 {
		t.Errorf("apt-get called unexpectedly")
	}
}

func TestApply_InstallsWhenMissing(t *testing.T) {
	runner := newFakeRunner()
	sec := &Section{Runner: runner, FS: newFakeFS()}

	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Extra["installed"] != true {
		t.Errorf("installed = %v, want true", res.Extra["installed"])
	}
	var sawInstall bool
	for _, c := range runner.callsFor("apt-get") {
		if argsContain(c.args, "install") && argsContain(c.args, "unattended-upgrades") {
			sawInstall = true
			var sawEnv bool
			for _, e := range c.env {
				if strings.HasPrefix(e, "DEBIAN_FRONTEND=") {
					sawEnv = true
				}
			}
			if !sawEnv {
				t.Errorf("apt-get install missing DEBIAN_FRONTEND, env=%v", c.env)
			}
		}
	}
	if !sawInstall {
		t.Errorf("apt-get install unattended-upgrades not called")
	}
}

func TestApply_WritesBothConfigFiles(t *testing.T) {
	fsys := newFakeFS()
	fsys.seed("/usr/bin/unattended-upgrade", []byte("bin"))

	sec := &Section{Runner: newFakeRunner(), FS: fsys}
	if _, err := sec.Apply(context.Background(), enabledSpec()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	upgradesData, err := fsys.ReadFile(UpgradesPath)
	if err != nil {
		t.Fatalf("50unattended-upgrades not written: %v", err)
	}
	if !strings.Contains(string(upgradesData), "distro_codename") {
		t.Errorf("50unattended-upgrades missing expected content")
	}

	autoData, err := fsys.ReadFile(AutoPath)
	if err != nil {
		t.Fatalf("20auto-upgrades not written: %v", err)
	}
	if !strings.Contains(string(autoData), "Unattended-Upgrade") {
		t.Errorf("20auto-upgrades missing expected content")
	}
}

func TestApply_SkipsWriteWhenConfigUnchanged(t *testing.T) {
	fsys := newFakeFS()
	fsys.seed("/usr/bin/unattended-upgrade", []byte("bin"))
	fsys.seed(UpgradesPath, upgradesPayload)
	fsys.seed(AutoPath, autoPayload)

	sec := &Section{Runner: newFakeRunner(), FS: fsys}
	if _, err := sec.Apply(context.Background(), enabledSpec()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Both files should be unchanged.
	d1, _ := fsys.ReadFile(UpgradesPath)
	d2, _ := fsys.ReadFile(AutoPath)
	if string(d1) != string(upgradesPayload) {
		t.Errorf("50unattended-upgrades changed unexpectedly")
	}
	if string(d2) != string(autoPayload) {
		t.Errorf("20auto-upgrades changed unexpectedly")
	}
}

func TestApply_EnablesService(t *testing.T) {
	fsys := newFakeFS()
	fsys.seed("/usr/bin/unattended-upgrade", []byte("bin"))
	runner := newFakeRunner()

	sec := &Section{Runner: runner, FS: fsys}
	if _, err := sec.Apply(context.Background(), enabledSpec()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var sawEnable bool
	for _, c := range runner.callsFor("systemctl") {
		if argsContain(c.args, "enable", "unattended-upgrades") {
			sawEnable = true
		}
	}
	if !sawEnable {
		t.Errorf("systemctl enable unattended-upgrades not called")
	}
}

func TestApply_SystemctlErrorSurfaces(t *testing.T) {
	fsys := newFakeFS()
	fsys.seed("/usr/bin/unattended-upgrade", []byte("bin"))
	runner := newFakeRunner()
	runner.errs["systemctl"] = errors.New("masked unit")

	sec := &Section{Runner: runner, FS: fsys}
	_, err := sec.Apply(context.Background(), enabledSpec())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "masked unit") {
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

func TestApply_FlatcarDistro_Skipped(t *testing.T) {
	runner := newFakeRunner()
	sec := &Section{Runner: runner, FS: newFakeFS(), Distro: &distro.Flatcar{}}
	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Skipped {
		t.Errorf("Skipped = false, want true")
	}
	if res.SkipReason == "" {
		t.Errorf("SkipReason is empty, want non-empty")
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d command calls, want 0", len(runner.calls))
	}
}
