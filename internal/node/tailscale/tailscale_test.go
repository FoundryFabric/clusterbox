package tailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/node/config"
)

// ---------------------------------------------------------------------------
// fakeFS
// ---------------------------------------------------------------------------

type fakeFS struct {
	mu      sync.Mutex
	files   map[string][]byte
	removed map[string]bool
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		files:   map[string][]byte{},
		removed: map[string]bool{},
	}
}

func (f *fakeFS) seed(path string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = append([]byte(nil), data...)
}

type fakeFileInfo struct{ name string }

func (i fakeFileInfo) Name() string     { return i.name }
func (fakeFileInfo) Size() int64        { return 0 }
func (fakeFileInfo) Mode() fs.FileMode  { return 0o644 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() interface{}   { return nil }

func (f *fakeFS) Stat(p string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removed[p] {
		return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
	if _, ok := f.files[p]; ok {
		return fakeFileInfo{name: p}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
}

func (f *fakeFS) ReadFile(p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removed[p] {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
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
	delete(f.removed, p)
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

func (f *fakeFS) Remove(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed[p] = true
	return nil
}

// ---------------------------------------------------------------------------
// fakeRunner — supports per-call responses (stateful sequence per key)
// ---------------------------------------------------------------------------

type fakeCall struct {
	name string
	args []string
}

type fakeRunner struct {
	mu sync.Mutex
	// calls records every invocation in order.
	calls []fakeCall
	// resps maps "name arg0 arg1..." to a queue of responses.
	// Each call to that key pops the first response; the last response is
	// returned for all subsequent calls.
	resps map[string][]fakeResp
	// errs maps command name to a static error returned for every call.
	errs map[string]error
}

type fakeResp struct {
	out []byte
	err error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		resps: map[string][]fakeResp{},
		errs:  map[string]error{},
	}
}

// setResp adds a sequence of responses for the given full command string
// (e.g. "tailscale status --json").
func (r *fakeRunner) setResp(key string, resps ...fakeResp) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resps[key] = resps
}

func (r *fakeRunner) popResp(name string, args []string) fakeResp {
	key := name
	if len(args) > 0 {
		key = name + " " + strings.Join(args, " ")
	}
	if q, ok := r.resps[key]; ok && len(q) > 0 {
		resp := q[0]
		if len(q) > 1 {
			r.resps[key] = q[1:]
		}
		return resp
	}
	// Fall back to name-only key.
	if q, ok := r.resps[name]; ok && len(q) > 0 {
		resp := q[0]
		if len(q) > 1 {
			r.resps[name] = q[1:]
		}
		return resp
	}
	// Fall back to static error map.
	if err, ok := r.errs[name]; ok {
		return fakeResp{err: err}
	}
	return fakeResp{}
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	resp := r.popResp(name, args)
	r.calls = append(r.calls, fakeCall{name: name, args: args})
	r.mu.Unlock()
	return resp.out, resp.err
}

func (r *fakeRunner) RunEnv(_ context.Context, _ []string, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	resp := r.popResp(name, args)
	r.calls = append(r.calls, fakeCall{name: name, args: args})
	r.mu.Unlock()
	return resp.out, resp.err
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

// ---------------------------------------------------------------------------
// helpers for building status JSON
// ---------------------------------------------------------------------------

func statusJSON(backendState string, ips []string) []byte {
	st := tailscaleStatus{BackendState: backendState}
	st.Self.TailscaleIPs = ips
	b, _ := json.Marshal(st)
	return b
}

func notRunningJSON() []byte       { return statusJSON("Stopped", nil) }
func runningJSON(ip string) []byte { return statusJSON("Running", []string{ip}) }

// ---------------------------------------------------------------------------
// spec helpers
// ---------------------------------------------------------------------------

func enabledSpec() *config.Spec {
	return &config.Spec{
		Tailscale: &config.TailscaleSpec{
			Enabled: true,
			AuthKey: "tskey-test-1234",
		},
	}
}

func fullSpec() *config.Spec {
	return &config.Spec{
		Tailscale: &config.TailscaleSpec{
			Enabled:      true,
			AuthKey:      "tskey-test-1234",
			Hostname:     "my-node",
			AcceptRoutes: true,
			AcceptDNS:    true,
		},
	}
}

// ---------------------------------------------------------------------------
// instantPoll returns a Section with time controls that never sleep and
// advance time by pollInterval on each afterFunc call.
// ---------------------------------------------------------------------------

func instantSection(runner Runner, fsys FS) *Section {
	t := time.Now()
	mu := &sync.Mutex{}
	return &Section{
		Runner: runner,
		FS:     fsys,
		nowFunc: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return t
		},
		afterFunc: func(d time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			t = t.Add(d)
		},
	}
}

// timeoutSection advances time past the poll deadline immediately on first
// sleep call so the poll loop exits quickly.
func timeoutSection(runner Runner, fsys FS) *Section {
	start := time.Now()
	mu := &sync.Mutex{}
	calls := 0
	return &Section{
		Runner: runner,
		FS:     fsys,
		nowFunc: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			if calls > 0 {
				return start.Add(pollTimeout + time.Second)
			}
			return start
		},
		afterFunc: func(d time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			calls++
		},
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestApply_Disabled_NilSpec(t *testing.T) {
	sec := instantSection(newFakeRunner(), newFakeFS())
	res, err := sec.Apply(context.Background(), &config.Spec{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied || res.Reason != "disabled" {
		t.Errorf("res = %+v, want applied=false reason=disabled", res)
	}
}

func TestApply_Disabled_ExplicitFalse(t *testing.T) {
	spec := &config.Spec{Tailscale: &config.TailscaleSpec{Enabled: false}}
	runner := newFakeRunner()
	sec := instantSection(runner, newFakeFS())
	res, err := sec.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied || res.Reason != "disabled" {
		t.Errorf("res = %+v, want applied=false reason=disabled", res)
	}
	if len(runner.calls) != 0 {
		t.Errorf("expected no commands run; got %d call(s): %v", len(runner.calls), runner.calls)
	}
}

func TestApply_AlreadyRunning(t *testing.T) {
	runner := newFakeRunner()
	runner.setResp("tailscale status --json",
		fakeResp{out: runningJSON("100.64.0.1")},
	)
	sec := instantSection(runner, newFakeFS())
	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied || res.Reason != "already running" {
		t.Errorf("res = %+v, want applied=false reason='already running'", res)
	}
}

func TestApply_HappyPath_WithEmbeddedBinaries(t *testing.T) {
	// Simulate embedded binaries by patching package vars temporarily.
	origTS := EmbeddedTailscale
	origTSd := EmbeddedTailscaled
	EmbeddedTailscale = []byte("fake-tailscale-bin")
	EmbeddedTailscaled = []byte("fake-tailscaled-bin")
	defer func() {
		EmbeddedTailscale = origTS
		EmbeddedTailscaled = origTSd
	}()

	runner := newFakeRunner()
	// First call: idempotency check — not running yet.
	runner.setResp("tailscale status --json",
		fakeResp{out: notRunningJSON()},          // idempotency check
		fakeResp{out: runningJSON("100.64.0.2")}, // first poll
	)
	fsys := newFakeFS()
	sec := instantSection(runner, fsys)

	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied = false, want true")
	}
	if res.Extra["ip"] != "100.64.0.2" {
		t.Errorf("ip = %v, want 100.64.0.2", res.Extra["ip"])
	}

	// Binaries must have been written.
	d, err := fsys.ReadFile(tailscaledBin)
	if err != nil || string(d) != "fake-tailscaled-bin" {
		t.Errorf("tailscaled binary not written correctly: %v %q", err, string(d))
	}
	d, err = fsys.ReadFile(tailscaleBin)
	if err != nil || string(d) != "fake-tailscale-bin" {
		t.Errorf("tailscale binary not written correctly: %v %q", err, string(d))
	}

	// Service file must have been written.
	svcData, err := fsys.ReadFile(tailscaledUnit)
	if err != nil {
		t.Fatalf("service file not written: %v", err)
	}
	if !strings.Contains(string(svcData), "ExecStart=/usr/local/bin/tailscaled") {
		t.Errorf("service file missing ExecStart line")
	}

	// Verify systemctl calls in order: daemon-reload, enable, start.
	sysctlCalls := runner.callsFor("systemctl")
	wantOrder := [][]string{
		{"daemon-reload"},
		{"enable", "tailscaled"},
		{"start", "tailscaled"},
	}
	idx := 0
	for _, call := range sysctlCalls {
		if idx < len(wantOrder) && argsContain(call.args, wantOrder[idx]...) {
			idx++
		}
	}
	if idx != len(wantOrder) {
		t.Errorf("systemctl calls not in expected order; got %v, want order %v", sysctlCalls, wantOrder)
	}

	// tailscale up must have been called.
	tsCalls := runner.callsFor("tailscale")
	var sawUp bool
	for _, c := range tsCalls {
		if len(c.args) > 0 && c.args[0] == "up" {
			sawUp = true
			var sawAuthKey bool
			for _, a := range c.args {
				if strings.HasPrefix(a, "--authkey=") {
					sawAuthKey = true
				}
			}
			if !sawAuthKey {
				t.Errorf("tailscale up missing --authkey flag; args: %v", c.args)
			}
		}
	}
	if !sawUp {
		t.Errorf("tailscale up not called")
	}
}

func TestApply_HappyPath_NoEmbeddedBinaries(t *testing.T) {
	// EmbeddedTailscale/d are nil (stub build tag) — no binary writes expected.
	origTS := EmbeddedTailscale
	origTSd := EmbeddedTailscaled
	EmbeddedTailscale = nil
	EmbeddedTailscaled = nil
	defer func() {
		EmbeddedTailscale = origTS
		EmbeddedTailscaled = origTSd
	}()

	runner := newFakeRunner()
	runner.setResp("tailscale status --json",
		fakeResp{out: notRunningJSON()},
		fakeResp{out: runningJSON("100.64.0.3")},
	)
	fsys := newFakeFS()
	sec := instantSection(runner, fsys)

	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied = false, want true")
	}

	// Binaries must NOT have been written.
	if _, err := fsys.ReadFile(tailscaledBin); err == nil {
		t.Errorf("tailscaled binary written unexpectedly when no embedded binary")
	}
	if _, err := fsys.ReadFile(tailscaleBin); err == nil {
		t.Errorf("tailscale binary written unexpectedly when no embedded binary")
	}

	// But systemctl and tailscale up must still have run.
	if len(runner.callsFor("systemctl")) == 0 {
		t.Errorf("systemctl not called")
	}
	if len(runner.callsFor("tailscale")) == 0 {
		t.Errorf("tailscale not called")
	}
}

func TestApply_FullSpec_FlagsPassedThrough(t *testing.T) {
	origTS := EmbeddedTailscale
	origTSd := EmbeddedTailscaled
	EmbeddedTailscale = nil
	EmbeddedTailscaled = nil
	defer func() {
		EmbeddedTailscale = origTS
		EmbeddedTailscaled = origTSd
	}()

	runner := newFakeRunner()
	runner.setResp("tailscale status --json",
		fakeResp{out: notRunningJSON()},
		fakeResp{out: runningJSON("100.64.0.4")},
	)
	sec := instantSection(runner, newFakeFS())

	if _, err := sec.Apply(context.Background(), fullSpec()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	tsCalls := runner.callsFor("tailscale")
	for _, c := range tsCalls {
		if len(c.args) == 0 || c.args[0] != "up" {
			continue
		}
		args := c.args
		wantFlags := []string{"--accept-routes", "--accept-dns", "--hostname=my-node"}
		for _, flag := range wantFlags {
			found := false
			for _, a := range args {
				if a == flag {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("tailscale up missing flag %q; args: %v", flag, args)
			}
		}
		return
	}
	t.Errorf("tailscale up not called")
}

func TestApply_PollTimeout(t *testing.T) {
	origTS := EmbeddedTailscale
	origTSd := EmbeddedTailscaled
	EmbeddedTailscale = nil
	EmbeddedTailscaled = nil
	defer func() {
		EmbeddedTailscale = origTS
		EmbeddedTailscaled = origTSd
	}()

	runner := newFakeRunner()
	// Idempotency check: not running. All poll attempts: also not running.
	runner.setResp("tailscale status --json",
		fakeResp{out: notRunningJSON()}, // idempotency
	)
	// Subsequent status calls (poll) always return not running.
	// (popResp falls back to last element of queue, which is already exhausted,
	//  so it returns zero fakeResp — nil out, nil err. We need to return
	//  notRunning for all poll calls, so we chain many responses.)
	for i := 0; i < 20; i++ {
		runner.setResp("tailscale status --json",
			fakeResp{out: notRunningJSON()},
		)
	}

	sec := timeoutSection(runner, newFakeFS())

	_, err := sec.Apply(context.Background(), enabledSpec())
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q should mention timeout", err)
	}
}

func TestApply_StatefulPoll_SucceedsOnSecondAttempt(t *testing.T) {
	origTS := EmbeddedTailscale
	origTSd := EmbeddedTailscaled
	EmbeddedTailscale = nil
	EmbeddedTailscaled = nil
	defer func() {
		EmbeddedTailscale = origTS
		EmbeddedTailscaled = origTSd
	}()

	runner := newFakeRunner()
	// idempotency: not running; poll attempt 1: not running; poll attempt 2: running.
	runner.setResp("tailscale status --json",
		fakeResp{out: notRunningJSON()},          // idempotency check
		fakeResp{out: notRunningJSON()},          // first poll
		fakeResp{out: runningJSON("100.64.0.5")}, // second poll
	)
	sec := instantSection(runner, newFakeFS())

	res, err := sec.Apply(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied = false, want true")
	}
	if res.Extra["ip"] != "100.64.0.5" {
		t.Errorf("ip = %v, want 100.64.0.5", res.Extra["ip"])
	}
}

func TestRemove_NotInstalled(t *testing.T) {
	runner := newFakeRunner()
	sec := &Section{Runner: runner, FS: newFakeFS()}
	res, err := sec.Remove(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if res.Applied || res.Reason != "not installed" {
		t.Errorf("res = %+v, want applied=false reason='not installed'", res)
	}
	if len(runner.calls) != 0 {
		t.Errorf("expected no commands run; got %d call(s)", len(runner.calls))
	}
}

func TestRemove_Installed(t *testing.T) {
	fsys := newFakeFS()
	fsys.seed(tailscaledBin, []byte("bin"))
	fsys.seed(tailscaleBin, []byte("bin"))
	fsys.seed(tailscaledUnit, []byte("[Unit]\n"))
	runner := newFakeRunner()

	sec := &Section{Runner: runner, FS: fsys}
	res, err := sec.Remove(context.Background(), enabledSpec())
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied = false, want true")
	}

	sysctlCalls := runner.callsFor("systemctl")
	var sawStop, sawDisable bool
	for _, c := range sysctlCalls {
		if argsContain(c.args, "stop", "tailscaled") {
			sawStop = true
		}
		if argsContain(c.args, "disable", "tailscaled") {
			sawDisable = true
		}
	}
	if !sawStop {
		t.Errorf("systemctl stop tailscaled not called")
	}
	if !sawDisable {
		t.Errorf("systemctl disable tailscaled not called")
	}

	// Files must have been removed.
	for _, path := range []string{tailscaledBin, tailscaleBin, tailscaledUnit} {
		if fsys.removed[path] == false {
			// Also check if it's still readable (not removed).
			if _, err := fsys.ReadFile(path); err == nil {
				t.Errorf("file %q still present after Remove", path)
			}
		}
	}
}

func TestApply_WriteError_Surfaces(t *testing.T) {
	origTSd := EmbeddedTailscaled
	EmbeddedTailscaled = []byte("bin")
	defer func() { EmbeddedTailscaled = origTSd }()

	runner := newFakeRunner()
	runner.setResp("tailscale status --json",
		fakeResp{out: notRunningJSON()},
	)

	fsys := &errorFS{err: fmt.Errorf("disk full")}
	sec := &Section{Runner: runner, FS: fsys}
	_, err := sec.Apply(context.Background(), enabledSpec())
	if err == nil {
		t.Fatal("expected error from WriteFile, got nil")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error %q should mention disk full", err)
	}
}

// errorFS always returns an error from WriteFile.
type errorFS struct {
	err error
}

func (e *errorFS) Stat(p string) (fs.FileInfo, error)                { return nil, fs.ErrNotExist }
func (e *errorFS) ReadFile(p string) ([]byte, error)                 { return nil, fs.ErrNotExist }
func (e *errorFS) WriteFile(p string, d []byte, m fs.FileMode) error { return e.err }
func (e *errorFS) MkdirAll(p string, m fs.FileMode) error            { return nil }
