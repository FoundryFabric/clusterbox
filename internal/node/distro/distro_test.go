package distro

import (
	"context"
	"errors"
	"io/fs"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Fake implementations
// ---------------------------------------------------------------------------

type fakeFS struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newFakeFS() *fakeFS {
	return &fakeFS{files: map[string][]byte{}}
}

func (f *fakeFS) addFile(path string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = content
}

func (f *fakeFS) ReadFile(path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return data, nil
}

type call struct {
	env  []string
	name string
	args []string
}

type runResp struct {
	out []byte
	err error
}

type fakeRunner struct {
	mu      sync.Mutex
	calls   []call
	runResp map[string]runResp
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{runResp: map[string]runResp{}}
}

func (f *fakeRunner) key(name string, args []string) string {
	parts := append([]string{name}, args...)
	result := ""
	for i, p := range parts {
		if i > 0 {
			result += " "
		}
		result += p
	}
	return result
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{name: name, args: args})
	k := f.key(name, args)
	resp, ok := f.runResp[k]
	if !ok {
		resp = f.runResp[name]
	}
	f.mu.Unlock()
	return resp.out, resp.err
}

func (f *fakeRunner) RunEnv(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{env: env, name: name, args: args})
	k := f.key(name, args)
	resp, ok := f.runResp[k]
	if !ok {
		resp = f.runResp[name]
	}
	f.mu.Unlock()
	return resp.out, resp.err
}

func (f *fakeRunner) callsFor(name string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Detect tests
// ---------------------------------------------------------------------------

func TestDetect_Ubuntu(t *testing.T) {
	fsys := newFakeFS()
	fsys.addFile(osReleasePath, []byte("ID=ubuntu\nVERSION_ID=\"22.04\"\n"))

	d, err := Detect(context.Background(), newFakeRunner(), fsys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.ID() != "ubuntu" {
		t.Errorf("expected ubuntu, got %q", d.ID())
	}
}

func TestDetect_Flatcar(t *testing.T) {
	fsys := newFakeFS()
	fsys.addFile(osReleasePath, []byte("ID=flatcar\nNAME=\"Flatcar Container Linux\"\n"))

	d, err := Detect(context.Background(), newFakeRunner(), fsys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.ID() != "flatcar" {
		t.Errorf("expected flatcar, got %q", d.ID())
	}
}

func TestDetect_FileMissing_DefaultsToUbuntu(t *testing.T) {
	fsys := newFakeFS() // empty — no files

	d, err := Detect(context.Background(), newFakeRunner(), fsys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.ID() != "ubuntu" {
		t.Errorf("expected ubuntu default, got %q", d.ID())
	}
}

func TestDetect_UnrecognisedID_DefaultsToUbuntu(t *testing.T) {
	fsys := newFakeFS()
	fsys.addFile(osReleasePath, []byte("ID=alpine\n"))

	d, err := Detect(context.Background(), newFakeRunner(), fsys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.ID() != "ubuntu" {
		t.Errorf("expected ubuntu default for unrecognised ID, got %q", d.ID())
	}
}

func TestDetect_NoIDLine_DefaultsToUbuntu(t *testing.T) {
	fsys := newFakeFS()
	fsys.addFile(osReleasePath, []byte("NAME=\"Some Linux\"\nVERSION_ID=1\n"))

	d, err := Detect(context.Background(), newFakeRunner(), fsys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.ID() != "ubuntu" {
		t.Errorf("expected ubuntu default when no ID line, got %q", d.ID())
	}
}

func TestDetect_QuotedID(t *testing.T) {
	fsys := newFakeFS()
	fsys.addFile(osReleasePath, []byte(`ID="ubuntu"`+"\n"))

	d, err := Detect(context.Background(), newFakeRunner(), fsys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.ID() != "ubuntu" {
		t.Errorf("expected ubuntu with quoted ID, got %q", d.ID())
	}
}

// ---------------------------------------------------------------------------
// FromSpec tests
// ---------------------------------------------------------------------------

func TestFromSpec_Ubuntu(t *testing.T) {
	d, ok := FromSpec("ubuntu")
	if !ok {
		t.Fatal("expected ok=true for ubuntu")
	}
	if d.ID() != "ubuntu" {
		t.Errorf("expected ubuntu, got %q", d.ID())
	}
}

func TestFromSpec_Flatcar(t *testing.T) {
	d, ok := FromSpec("flatcar")
	if !ok {
		t.Fatal("expected ok=true for flatcar")
	}
	if d.ID() != "flatcar" {
		t.Errorf("expected flatcar, got %q", d.ID())
	}
}

func TestFromSpec_Empty(t *testing.T) {
	d, ok := FromSpec("")
	if ok {
		t.Error("expected ok=false for empty spec")
	}
	if d != nil {
		t.Errorf("expected nil distro for empty spec, got %v", d)
	}
}

func TestFromSpec_Unknown(t *testing.T) {
	d, ok := FromSpec("debian")
	if ok {
		t.Error("expected ok=false for unknown spec")
	}
	if d != nil {
		t.Errorf("expected nil distro for unknown spec, got %v", d)
	}
}

// ---------------------------------------------------------------------------
// Ubuntu.InstallPackage tests
// ---------------------------------------------------------------------------

func TestUbuntu_InstallPackage_CallsAptGet(t *testing.T) {
	runner := newFakeRunner()
	u := &Ubuntu{}

	err := u.InstallPackage(context.Background(), runner, "curl", "jq")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	aptCalls := runner.callsFor("apt-get")
	if len(aptCalls) != 2 {
		t.Fatalf("expected 2 apt-get calls (update + install), got %d", len(aptCalls))
	}

	// First call: apt-get update -qq
	updateCall := aptCalls[0]
	if updateCall.args[0] != "update" || updateCall.args[1] != "-qq" {
		t.Errorf("first apt-get call: expected [update -qq], got %v", updateCall.args)
	}
	if !hasEnv(updateCall.env, "DEBIAN_FRONTEND=noninteractive") {
		t.Errorf("update call missing DEBIAN_FRONTEND=noninteractive, env=%v", updateCall.env)
	}

	// Second call: apt-get install -y -qq curl jq
	installCall := aptCalls[1]
	if installCall.args[0] != "install" || installCall.args[1] != "-y" || installCall.args[2] != "-qq" {
		t.Errorf("second apt-get call: expected [install -y -qq ...], got %v", installCall.args)
	}
	if !contains(installCall.args, "curl") || !contains(installCall.args, "jq") {
		t.Errorf("install call missing packages, args=%v", installCall.args)
	}
	if !hasEnv(installCall.env, "DEBIAN_FRONTEND=noninteractive") {
		t.Errorf("install call missing DEBIAN_FRONTEND=noninteractive, env=%v", installCall.env)
	}
}

func TestUbuntu_InstallPackage_NoPkgs_NoOp(t *testing.T) {
	runner := newFakeRunner()
	u := &Ubuntu{}

	err := u.InstallPackage(context.Background(), runner /* no pkgs */)
	if err != nil {
		t.Fatalf("unexpected error for empty package list: %v", err)
	}

	if len(runner.callsFor("apt-get")) != 0 {
		t.Error("expected no apt-get calls for empty package list")
	}
}

func TestUbuntu_InstallPackage_UpdateError_Propagates(t *testing.T) {
	runner := newFakeRunner()
	runner.runResp["apt-get"] = runResp{err: errors.New("network error")}
	u := &Ubuntu{}

	err := u.InstallPackage(context.Background(), runner, "curl")
	if err == nil {
		t.Fatal("expected error when apt-get update fails")
	}
}

// ---------------------------------------------------------------------------
// Flatcar.InstallPackage tests
// ---------------------------------------------------------------------------

func TestFlatcar_InstallPackage_ReturnsErrNotSupported(t *testing.T) {
	runner := newFakeRunner()
	f := &Flatcar{}

	err := f.InstallPackage(context.Background(), runner, "curl")
	if !errors.Is(err, ErrNotSupported) {
		t.Errorf("expected ErrNotSupported, got %v", err)
	}
}

func TestFlatcar_ID(t *testing.T) {
	f := &Flatcar{}
	if f.ID() != "flatcar" {
		t.Errorf("expected flatcar, got %q", f.ID())
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func hasEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
