package harden

import (
	"context"
	"errors"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/node/harden/sshd"
	"github.com/foundryfabric/clusterbox/internal/node/harden/ufw"
	"github.com/foundryfabric/clusterbox/internal/node/harden/user"
)

// minimal fake plumbing — just enough for an end-to-end walk through
// all three subsystems. The per-subsystem unit tests already exercise
// each subsystem's edge cases; here we only verify aggregation.

type userFS struct {
	mu    sync.Mutex
	files map[string][]byte
	users map[string]struct{ uid, gid int }
	homes map[string]string
}

func newUserFS() *userFS {
	return &userFS{
		files: map[string][]byte{},
		users: map[string]struct{ uid, gid int }{},
		homes: map[string]string{},
	}
}

type fileInfo struct{ size int64 }

func (fileInfo) Name() string       { return "" }
func (fi fileInfo) Size() int64     { return fi.size }
func (fileInfo) Mode() fs.FileMode  { return 0o644 }
func (fileInfo) ModTime() time.Time { return time.Time{} }
func (fileInfo) IsDir() bool        { return false }
func (fileInfo) Sys() any           { return nil }

func (f *userFS) Stat(p string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.files[p]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
	return fileInfo{size: int64(len(d))}, nil
}
func (f *userFS) MkdirAll(p string, _ fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[p]; !ok {
		f.files[p] = nil
	}
	return nil
}
func (f *userFS) ReadFile(p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.files[p]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	out := make([]byte, len(d))
	copy(out, d)
	return out, nil
}
func (f *userFS) WriteFile(p string, d []byte, _ fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(d))
	copy(cp, d)
	f.files[p] = cp
	return nil
}
func (f *userFS) Chown(string, int, int) error    { return nil }
func (f *userFS) Chmod(string, fs.FileMode) error { return nil }
func (f *userFS) LookupUserHome(name string) (string, int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[name]
	if !ok {
		return "", 0, 0, errors.New("not found")
	}
	return f.homes[name], u.uid, u.gid, nil
}

type userRunner struct {
	fs *userFS
}

func (r *userRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name == "useradd" {
		uname := args[len(args)-1]
		r.fs.mu.Lock()
		r.fs.users[uname] = struct{ uid, gid int }{4242, 4242}
		r.fs.homes[uname] = "/home/" + uname
		r.fs.mu.Unlock()
	}
	return nil, nil
}

type sshdFS struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newSshdFS() *sshdFS { return &sshdFS{files: map[string][]byte{}} }

func (f *sshdFS) Stat(p string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.files[p]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
	return fileInfo{size: int64(len(d))}, nil
}
func (f *sshdFS) ReadFile(p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.files[p]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	return append([]byte(nil), d...), nil
}
func (f *sshdFS) WriteFile(p string, d []byte, _ fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = append([]byte(nil), d...)
	return nil
}
func (f *sshdFS) MkdirAll(p string, _ fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[p]; !ok {
		f.files[p] = nil
	}
	return nil
}
func (f *sshdFS) Remove(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, p)
	return nil
}

type sshdRunner struct{}

func (sshdRunner) Run(context.Context, string, ...string) ([]byte, error) { return nil, nil }

type ufwFS struct {
	mu    sync.Mutex
	files map[string]bool
}

func newUfwFS() *ufwFS { return &ufwFS{files: map[string]bool{ufw.UfwBinary: true}} }

func (f *ufwFS) Stat(p string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.files[p] {
		return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
	return fileInfo{}, nil
}

type ufwRunner struct{}

func (ufwRunner) Run(context.Context, string, ...string) ([]byte, error) { return nil, nil }
func (ufwRunner) RunEnv(context.Context, []string, string, ...string) ([]byte, error) {
	return nil, nil
}

func enabledSpec() *config.Spec {
	return &config.Spec{Harden: &config.HardenSpec{
		Enabled:   true,
		User:      "ops",
		SSHPubKey: "ssh-ed25519 AAAA test@example",
	}}
}

func TestApply_DisabledWhenSpecMissing(t *testing.T) {
	sec := &Section{}
	res, err := sec.Apply(context.Background(), &config.Spec{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied || res.Reason != "disabled" {
		t.Errorf("res = %+v, want applied=false reason=disabled", res)
	}
}

func TestApply_AggregatesSubsystemSteps(t *testing.T) {
	uFS := newUserFS()
	uRun := &userRunner{fs: uFS}
	sFS := newSshdFS()
	uwFS := newUfwFS()

	sec := &Section{
		User: user.Section{Runner: uRun, FS: uFS},
		SSHD: sshd.Section{Runner: sshdRunner{}, FS: sFS},
		UFW:  ufw.Section{Runner: ufwRunner{}, FS: uwFS},
	}
	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied = false, want true")
	}
	steps, ok := res.Extra["steps"].(map[string]interface{})
	if !ok {
		t.Fatalf("steps missing or wrong type: %v", res.Extra)
	}
	for _, k := range []string{"user_created", "sshd_locked_down", "ufw_enabled"} {
		v, present := steps[k]
		if !present {
			t.Errorf("steps missing %q", k)
		}
		if vb, _ := v.(bool); !vb {
			t.Errorf("steps[%q] = %v, want true", k, v)
		}
	}
}

func TestApply_StopsAtFirstSubsystemError(t *testing.T) {
	// Use a real user.Section with a runner that returns an error on
	// useradd — equivalent to a subsystem error and exercises the same
	// abort path the coordinator takes.
	uFS := newUserFS()
	uRun := &erroringUserRunner{}
	sec := &Section{
		User: user.Section{Runner: uRun, FS: uFS},
	}
	_, err := sec.Apply(context.Background(), enabledSpec())
	if err == nil {
		t.Fatal("expected user-subsystem error")
	}
}

type erroringUserRunner struct{}

func (erroringUserRunner) Run(_ context.Context, _ string, _ ...string) ([]byte, error) {
	return nil, errors.New("useradd blew up")
}

func TestRemove_NoOp(t *testing.T) {
	sec := &Section{}
	res, err := sec.Remove(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if res.Applied {
		t.Errorf("Applied = true, want false (Remove is a no-op for v1)")
	}
	steps, _ := res.Extra["steps"].(map[string]interface{})
	if len(steps) != 3 {
		t.Errorf("steps = %v, want 3 entries", steps)
	}
}
