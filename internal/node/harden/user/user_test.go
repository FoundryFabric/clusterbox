package user

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

// fakeFS is an in-memory FS implementation. It tracks file contents,
// modes, and recorded chown/chmod calls so tests can assert that the
// security-sensitive metadata was set correctly.
type fakeFS struct {
	mu     sync.Mutex
	files  map[string][]byte
	modes  map[string]fs.FileMode
	owners map[string][2]int

	users map[string]struct {
		home     string
		uid, gid int
	}
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		files:  map[string][]byte{},
		modes:  map[string]fs.FileMode{},
		owners: map[string][2]int{},
		users: map[string]struct {
			home     string
			uid, gid int
		}{},
	}
}

func (f *fakeFS) addUser(name, home string, uid, gid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[name] = struct {
		home     string
		uid, gid int
	}{home, uid, gid}
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

func (f *fakeFS) MkdirAll(path string, mode fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[path]; !ok {
		f.files[path] = nil
	}
	f.modes[path] = mode | fs.ModeDir
	return nil
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

func (f *fakeFS) Chown(path string, uid, gid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owners[path] = [2]int{uid, gid}
	return nil
}

func (f *fakeFS) Chmod(path string, mode fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modes[path] = mode
	return nil
}

func (f *fakeFS) LookupUserHome(name string) (string, int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[name]
	if !ok {
		return "", 0, 0, errors.New("user not found")
	}
	return u.home, u.uid, u.gid, nil
}

// fakeRunner records every Run invocation and returns programmable
// responses keyed by command name.
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

func (f *fakeRunner) findCall(name string) *call {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.calls {
		if f.calls[i].name == name {
			return &f.calls[i]
		}
	}
	return nil
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

func TestApply_RequiresSSHKey(t *testing.T) {
	sec := &Section{Runner: newFakeRunner(), FS: newFakeFS()}
	spec := &config.Spec{Harden: &config.HardenSpec{Enabled: true, User: "ops"}}
	_, err := sec.Apply(context.Background(), spec)
	if err == nil {
		t.Fatal("expected missing-key error")
	}
}

func TestApply_CreatesUserAndKey(t *testing.T) {
	runner := newFakeRunner()
	fsys := newFakeFS()
	// User does not exist yet — useradd should run, then we register it
	// in the fake password DB so the subsequent home-dir lookup works.
	runner.runResp["useradd"] = runResp{}
	// Capture the args via a side effect.
	origRun := runner
	wrapped := &captureRunner{inner: origRun, fs: fsys}

	sec := &Section{Runner: wrapped, FS: fsys}
	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied = false, want true")
	}
	if res.Extra["user_created"] != true {
		t.Errorf("user_created = %v, want true", res.Extra["user_created"])
	}
	if res.Extra["key_added"] != true {
		t.Errorf("key_added = %v, want true", res.Extra["key_added"])
	}

	if c := origRun.findCall("useradd"); c == nil {
		t.Errorf("useradd was not called, calls=%v", origRun.calls)
	}
	if c := origRun.findCall("usermod"); c == nil {
		t.Errorf("usermod was not called")
	} else if !sliceContains(c.args, "sudo") {
		t.Errorf("usermod missing 'sudo' group, args=%v", c.args)
	}

	keyPath := "/home/ops/.ssh/authorized_keys"
	data, err := fsys.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("authorized_keys missing: %v", err)
	}
	if !strings.Contains(string(data), "ssh-ed25519 AAAA test@example") {
		t.Errorf("key not written: %q", data)
	}
	if mode := fsys.modes[keyPath]; mode != 0o600 {
		t.Errorf("authorized_keys mode = %o, want 0600", mode)
	}
	if owner := fsys.owners[keyPath]; owner != [2]int{4242, 4242} {
		t.Errorf("authorized_keys owner = %v, want {4242,4242}", owner)
	}
}

func TestApply_IdempotentWhenUserExists(t *testing.T) {
	runner := newFakeRunner()
	fsys := newFakeFS()
	fsys.addUser("ops", "/home/ops", 1000, 1000)

	sec := &Section{Runner: runner, FS: fsys}
	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Extra["user_created"] != false {
		t.Errorf("user_created = %v, want false", res.Extra["user_created"])
	}
	if c := runner.findCall("useradd"); c != nil {
		t.Errorf("useradd should not run when user already exists")
	}
	// usermod should still run (cheap, idempotent).
	if c := runner.findCall("usermod"); c == nil {
		t.Errorf("usermod should still be invoked for sudo membership")
	}
}

func TestApply_KeyAlreadyPresentSkipsAppend(t *testing.T) {
	runner := newFakeRunner()
	fsys := newFakeFS()
	fsys.addUser("ops", "/home/ops", 1000, 1000)
	keyPath := "/home/ops/.ssh/authorized_keys"
	if err := fsys.WriteFile(keyPath, []byte("ssh-ed25519 AAAA test@example\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	sec := &Section{Runner: runner, FS: fsys}
	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Extra["key_added"] != false {
		t.Errorf("key_added = %v, want false (key was already present)", res.Extra["key_added"])
	}
	// Mode must be re-enforced even when the key was already present.
	if mode := fsys.modes[keyPath]; mode != 0o600 {
		t.Errorf("mode = %o, want 0600 (re-enforced)", mode)
	}
}

func TestApply_AppendsToExistingAuthorizedKeys(t *testing.T) {
	runner := newFakeRunner()
	fsys := newFakeFS()
	fsys.addUser("ops", "/home/ops", 1000, 1000)
	keyPath := "/home/ops/.ssh/authorized_keys"
	existing := []byte("ssh-rsa OLDKEY old@host")
	if err := fsys.WriteFile(keyPath, existing, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	sec := &Section{Runner: runner, FS: fsys}
	if _, err := sec.Apply(context.Background(), enabledSpec()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, _ := fsys.ReadFile(keyPath)
	if !strings.Contains(string(got), "OLDKEY") || !strings.Contains(string(got), "test@example") {
		t.Errorf("authorized_keys = %q, expected both keys preserved", got)
	}
}

func TestApply_DefaultUserNameWhenEmpty(t *testing.T) {
	runner := newFakeRunner()
	fsys := newFakeFS()
	wrapped := &captureRunner{inner: runner, fs: fsys}
	sec := &Section{Runner: wrapped, FS: fsys}
	spec := &config.Spec{Harden: &config.HardenSpec{
		Enabled:   true,
		SSHPubKey: "ssh-ed25519 AAAA test@example",
	}}
	res, err := sec.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Extra["user"] != DefaultUser {
		t.Errorf("user = %v, want %s", res.Extra["user"], DefaultUser)
	}
}

func TestApply_UseraddFailureSurfaces(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["useradd"] = runResp{err: errors.New("permission denied")}
	fsys := newFakeFS()
	sec := &Section{Runner: runner, FS: fsys}
	_, err := sec.Apply(context.Background(), enabledSpec())
	if err == nil {
		t.Fatal("expected useradd error")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error %q should mention underlying failure", err)
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

// captureRunner registers the user in the fake password DB the moment
// useradd is called, so subsequent home-dir lookups succeed. The
// production system has the same behaviour: useradd creates the entry
// in /etc/passwd before the call returns.
type captureRunner struct {
	inner *fakeRunner
	fs    *fakeFS
}

func (c *captureRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := c.inner.Run(ctx, name, args...)
	if name == "useradd" && err == nil {
		// Last positional arg is the username.
		username := args[len(args)-1]
		c.fs.addUser(username, "/home/"+username, 4242, 4242)
	}
	return out, err
}

func sliceContains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
