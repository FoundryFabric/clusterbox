package sshd

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
	modes map[string]fs.FileMode
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		files: map[string][]byte{},
		modes: map[string]fs.FileMode{},
	}
}

type fakeFileInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (fi fakeFileInfo) Name() string       { return fi.name }
func (fi fakeFileInfo) Size() int64        { return fi.size }
func (fi fakeFileInfo) Mode() fs.FileMode  { return fi.mode }
func (fi fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fi fakeFileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi fakeFileInfo) Sys() any           { return nil }

func (f *fakeFS) Stat(path string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
	}
	return fakeFileInfo{name: path, size: int64(len(data)), mode: f.modes[path]}, nil
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

func (f *fakeFS) WriteFile(path string, data []byte, mode fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	f.files[path] = cp
	f.modes[path] = mode
	return nil
}

func (f *fakeFS) MkdirAll(path string, mode fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[path]; !ok {
		f.files[path] = nil
	}
	f.modes[path] = mode | fs.ModeDir
	return nil
}

func (f *fakeFS) Remove(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, path)
	delete(f.modes, path)
	return nil
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

func (f *fakeRunner) findCall(name string) []*call {
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

func enabledSpec() *config.Spec {
	return &config.Spec{Harden: &config.HardenSpec{
		Enabled:   true,
		User:      "ops",
		SSHPubKey: "ssh-ed25519 AAAA test@example",
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

func TestApply_WritesDropInAndReloads(t *testing.T) {
	runner := newFakeRunner()
	fsys := newFakeFS()
	sec := &Section{Runner: runner, FS: fsys}

	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied = false, want true")
	}
	if res.Extra["reloaded"] != true {
		t.Errorf("reloaded = %v, want true", res.Extra["reloaded"])
	}
	got, _ := fsys.ReadFile(DropInPath)
	for _, want := range []string{
		"PermitRootLogin no",
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"PubkeyAuthentication yes",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("drop-in missing %q\ngot: %s", want, got)
		}
	}
	if calls := runner.findCall("sshd"); len(calls) == 0 {
		t.Errorf("sshd -t was not run")
	} else if calls[0].args[0] != "-t" {
		t.Errorf("sshd args = %v, want [-t]", calls[0].args)
	}
	if calls := runner.findCall("systemctl"); len(calls) == 0 {
		t.Errorf("systemctl reload ssh was not run")
	}
}

func TestApply_IdempotentWhenAlreadyConfigured(t *testing.T) {
	runner := newFakeRunner()
	fsys := newFakeFS()
	// Pre-populate with the exact embedded payload.
	if err := fsys.WriteFile(DropInPath, dropInPayload, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	sec := &Section{Runner: runner, FS: fsys}
	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied || res.Reason != "already configured" {
		t.Errorf("res = %+v, want applied=true reason='already configured'", res)
	}
	if res.Extra["reloaded"] != false {
		t.Errorf("reloaded = %v, want false", res.Extra["reloaded"])
	}
	if calls := runner.findCall("systemctl"); len(calls) > 0 {
		t.Errorf("systemctl should NOT run when drop-in already matches")
	}
	if calls := runner.findCall("sshd"); len(calls) > 0 {
		t.Errorf("sshd -t should NOT run when drop-in already matches")
	}
}

func TestApply_ValidationFailureRollsBack(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["sshd"] = runResp{err: errors.New("Bad configuration")}
	fsys := newFakeFS()

	sec := &Section{Runner: runner, FS: fsys}
	_, err := sec.Apply(context.Background(), enabledSpec())
	if err == nil {
		t.Fatal("expected validation error")
	}
	// Drop-in must not be left on disk after a validation failure.
	if _, err := fsys.ReadFile(DropInPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("drop-in should have been removed on validation failure, got err=%v", err)
	}
	// systemctl should not have run after sshd -t failed.
	if calls := runner.findCall("systemctl"); len(calls) > 0 {
		t.Errorf("systemctl should not run after sshd -t fails")
	}
}

func TestApply_ValidationFailureRestoresPrev(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["sshd"] = runResp{err: errors.New("Bad configuration")}
	fsys := newFakeFS()
	prev := []byte("# previous content\nPermitRootLogin yes\n")
	if err := fsys.WriteFile(DropInPath, prev, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	sec := &Section{Runner: runner, FS: fsys}
	_, err := sec.Apply(context.Background(), enabledSpec())
	if err == nil {
		t.Fatal("expected validation error")
	}
	got, _ := fsys.ReadFile(DropInPath)
	if string(got) != string(prev) {
		t.Errorf("rollback failed: got %q, want %q", got, prev)
	}
}

func TestApply_ReloadFailureSurfaces(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["systemctl"] = runResp{err: errors.New("unit not found")}
	fsys := newFakeFS()

	sec := &Section{Runner: runner, FS: fsys}
	_, err := sec.Apply(context.Background(), enabledSpec())
	if err == nil {
		t.Fatal("expected reload error")
	}
	if !strings.Contains(err.Error(), "unit not found") {
		t.Errorf("error %q should mention reload failure", err)
	}
}

func TestRemove_NoOp(t *testing.T) {
	sec := &Section{Runner: newFakeRunner(), FS: newFakeFS()}
	res, err := sec.Remove(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if res.Applied {
		t.Errorf("Applied = true, want false (Remove is a no-op for v1)")
	}
}
