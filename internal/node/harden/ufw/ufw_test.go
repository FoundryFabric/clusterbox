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
	"github.com/foundryfabric/clusterbox/internal/node/distro"
)

type fakeFS struct {
	mu      sync.Mutex
	files   map[string]bool
	written map[string][]byte
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		files:   map[string]bool{},
		written: map[string][]byte{},
	}
}

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

func (f *fakeFS) WriteFile(path string, data []byte, _ fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if data == nil {
		delete(f.written, path)
		return nil
	}
	f.written[path] = append([]byte(nil), data...)
	return nil
}

func (f *fakeFS) writtenFor(path string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written[path]
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
	key := name
	if len(args) > 0 {
		key = name + " " + strings.Join(args, " ")
	}
	f.calls = append(f.calls, call{name: name, args: args})
	// Try full-key match first, then name-only fallback.
	resp, ok := f.runResp[key]
	if !ok {
		resp, ok = f.runResp[name]
	}
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

// ---- Ubuntu / UFW tests (unchanged behaviour) ----------------------------

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

// ---- Flatcar / iptables tests --------------------------------------------

// flatcarSection builds a Section with distro.Flatcar and fake runner/FS.
func flatcarSection() (*Section, *fakeRunner, *fakeFS) {
	runner := newFakeRunner()
	// iptables-save output stub.
	runner.runResp["iptables-save"] = runResp{out: []byte("# fake rules\n")}
	fsys := newFakeFS()
	sec := &Section{
		Runner: runner,
		FS:     fsys,
		Distro: &distro.Flatcar{},
	}
	return sec, runner, fsys
}

// TestApply_Flatcar_BasicRules verifies that on Flatcar path:
//   - iptables -C is called before each -A
//   - all required rules are added (none pre-exist → all -C fail)
//   - iptables-save output is written to /etc/iptables/rules.v4
//   - iptables-restore.service is written
//   - systemctl daemon-reload and enable are called
func TestApply_Flatcar_BasicRules(t *testing.T) {
	sec, runner, fsys := flatcarSection()
	// Inject -C failures for all rules; -A calls default to nil/nil (success).
	runner.runResp["iptables-save"] = runResp{out: []byte("# saved\n")}
	setCAllFail(runner, flatcarRules(false))

	res, err := sec.Apply(context.Background(), enabledSpec(false))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied=false, want true")
	}
	if res.Extra["distro"] != "flatcar" {
		t.Errorf("Extra[distro]=%v, want flatcar", res.Extra["distro"])
	}

	ipt := runner.callsFor("iptables")
	// For each rule we expect a -C then a -A.
	rules := flatcarRules(false)
	if len(ipt) != len(rules)*2 {
		t.Errorf("expected %d iptables calls (-C + -A per rule), got %d", len(rules)*2, len(ipt))
	}

	// Verify -C appears before -A for INPUT DROP (last rule).
	checkIdx, addIdx := -1, -1
	for i, c := range ipt {
		if c.args[0] == "-C" && c.args[len(c.args)-1] == "DROP" {
			checkIdx = i
		}
		if c.args[0] == "-A" && c.args[len(c.args)-1] == "DROP" {
			addIdx = i
		}
	}
	if checkIdx == -1 || addIdx == -1 {
		t.Fatalf("expected -C and -A for DROP rule, checkIdx=%d addIdx=%d", checkIdx, addIdx)
	}
	if checkIdx >= addIdx {
		t.Errorf("-C DROP (idx %d) should precede -A DROP (idx %d)", checkIdx, addIdx)
	}

	// rules.v4 written.
	if got := fsys.writtenFor("/etc/iptables/rules.v4"); string(got) != "# saved\n" {
		t.Errorf("/etc/iptables/rules.v4 = %q, want %q", got, "# saved\n")
	}

	// iptables-restore.service written.
	svcData := fsys.writtenFor("/etc/systemd/system/iptables-restore.service")
	if len(svcData) == 0 {
		t.Errorf("iptables-restore.service not written")
	}
	if !strings.Contains(string(svcData), "ExecStart=/sbin/iptables-restore") {
		t.Errorf("service file missing ExecStart, got:\n%s", svcData)
	}

	// systemctl calls.
	sctl := runner.callsFor("systemctl")
	var sawReload, sawEnable bool
	for _, c := range sctl {
		if argsContain(c.args, "daemon-reload") {
			sawReload = true
		}
		if argsContain(c.args, "enable", "iptables-restore") {
			sawEnable = true
		}
	}
	if !sawReload {
		t.Error("systemctl daemon-reload not called")
	}
	if !sawEnable {
		t.Error("systemctl enable iptables-restore not called")
	}
}

// TestApply_Flatcar_Idempotent verifies that when iptables -C exits 0
// (rule already present), no -A is issued for that rule.
func TestApply_Flatcar_Idempotent(t *testing.T) {
	sec, runner, _ := flatcarSection()
	// All -C calls succeed (rules already present) — no per-key overrides
	// needed; default fakeRunner returns nil,nil for unknown keys.
	// iptables-save is already set in flatcarSection.

	res, err := sec.Apply(context.Background(), enabledSpec(false))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied=false, want true")
	}

	// Only -C calls should exist; zero -A calls.
	ipt := runner.callsFor("iptables")
	rules := flatcarRules(false)
	if len(ipt) != len(rules) {
		t.Errorf("expected %d iptables -C calls (all already present), got %d", len(rules), len(ipt))
	}
	for _, c := range ipt {
		if len(c.args) > 0 && c.args[0] == "-A" {
			t.Errorf("unexpected -A call when rules already present: %v", c.args)
		}
	}
}

// setCAllFail marks every iptables -C call for the given rule set as
// "not present" so the code issues -A for each one. -A calls are left
// with the default nil/nil response (success).
func setCAllFail(runner *fakeRunner, rules []iptRule) {
	for _, r := range rules {
		checkArgs := append([]string{"-C", r.chain}, r.args...)
		key := "iptables " + strings.Join(checkArgs, " ")
		runner.runResp[key] = runResp{err: errors.New("no match")}
	}
}

// TestApply_Flatcar_AllowICMP verifies ICMP rule is added before DROP.
func TestApply_Flatcar_AllowICMP(t *testing.T) {
	sec, runner, _ := flatcarSection()
	// Make -C fail for all rules so -A is issued for each.
	setCAllFail(runner, flatcarRules(true))
	runner.runResp["iptables-save"] = runResp{out: []byte("# ok\n")}

	_, err := sec.Apply(context.Background(), enabledSpec(true))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	ipt := runner.callsFor("iptables")
	// Collect the -A INPUT rules in order.
	var addInputArgs [][]string
	for _, c := range ipt {
		if len(c.args) >= 2 && c.args[0] == "-A" && c.args[1] == "INPUT" {
			addInputArgs = append(addInputArgs, c.args)
		}
	}

	// Find ICMP and DROP positions.
	icmpIdx, dropIdx := -1, -1
	for i, args := range addInputArgs {
		if argsContain(args, "-p", "icmp") {
			icmpIdx = i
		}
		if argsContain(args, "-j", "DROP") {
			dropIdx = i
		}
	}
	if icmpIdx == -1 {
		t.Fatal("ICMP -A INPUT rule not found")
	}
	if dropIdx == -1 {
		t.Fatal("DROP -A INPUT rule not found")
	}
	if icmpIdx >= dropIdx {
		t.Errorf("ICMP rule (idx %d) must come before DROP rule (idx %d)", icmpIdx, dropIdx)
	}
}

// TestApply_Flatcar_NoICMP verifies ICMP rule is absent when AllowICMP=false.
func TestApply_Flatcar_NoICMP(t *testing.T) {
	sec, runner, _ := flatcarSection()
	setCAllFail(runner, flatcarRules(false))
	runner.runResp["iptables-save"] = runResp{out: []byte("# ok\n")}

	_, err := sec.Apply(context.Background(), enabledSpec(false))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for _, c := range runner.callsFor("iptables") {
		if argsContain(c.args, "-p", "icmp") {
			t.Errorf("unexpected ICMP rule when AllowICMP=false: %v", c.args)
		}
	}
}

// TestApply_Flatcar_DROPIsLast verifies that the INPUT DROP rule is the
// last INPUT rule applied.
func TestApply_Flatcar_DROPIsLast(t *testing.T) {
	sec, runner, _ := flatcarSection()
	setCAllFail(runner, flatcarRules(true))
	runner.runResp["iptables-save"] = runResp{out: []byte("# ok\n")}

	_, err := sec.Apply(context.Background(), enabledSpec(true))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	ipt := runner.callsFor("iptables")
	// Gather -A INPUT rules in order.
	var inputAdds [][]string
	for _, c := range ipt {
		if len(c.args) >= 2 && c.args[0] == "-A" && c.args[1] == "INPUT" {
			inputAdds = append(inputAdds, c.args)
		}
	}
	if len(inputAdds) == 0 {
		t.Fatal("no -A INPUT calls found")
	}
	last := inputAdds[len(inputAdds)-1]
	if !argsContain(last, "-j", "DROP") {
		t.Errorf("last -A INPUT rule should be DROP, got %v", last)
	}
}

// TestRemove_Flatcar verifies that Remove on Flatcar flushes iptables
// and disables the restore service.
func TestRemove_Flatcar(t *testing.T) {
	runner := newFakeRunner()
	fsys := newFakeFS()
	sec := &Section{
		Runner: runner,
		FS:     fsys,
		Distro: &distro.Flatcar{},
	}

	res, err := sec.Remove(context.Background(), enabledSpec(false))
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied=false, want true for Flatcar Remove")
	}

	// iptables -F called.
	ipt := runner.callsFor("iptables")
	if len(ipt) == 0 || !argsContain(ipt[0].args, "-F") {
		t.Errorf("expected iptables -F, got %v", ipt)
	}

	// systemctl disable called.
	var sawDisable bool
	for _, c := range runner.callsFor("systemctl") {
		if argsContain(c.args, "disable", "iptables-restore") {
			sawDisable = true
		}
	}
	if !sawDisable {
		t.Error("systemctl disable iptables-restore not called")
	}
}

// TestApply_NilDistroUsesUFW confirms that nil Distro still runs the UFW path.
func TestApply_NilDistroUsesUFW(t *testing.T) {
	runner := newFakeRunner()
	fsys := newFakeFS()
	fsys.addFile(UfwBinary)
	sec := &Section{Runner: runner, FS: fsys, Distro: nil}

	res, err := sec.Apply(context.Background(), enabledSpec(false))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied=false, want true")
	}
	if len(runner.callsFor("ufw")) == 0 {
		t.Error("expected UFW calls for nil distro")
	}
}

// TestApply_UbuntuDistroUsesUFW confirms that distro.Ubuntu still runs UFW.
func TestApply_UbuntuDistroUsesUFW(t *testing.T) {
	runner := newFakeRunner()
	fsys := newFakeFS()
	fsys.addFile(UfwBinary)
	sec := &Section{Runner: runner, FS: fsys, Distro: &distro.Ubuntu{}}

	res, err := sec.Apply(context.Background(), enabledSpec(false))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("Applied=false, want true")
	}
	if len(runner.callsFor("ufw")) == 0 {
		t.Error("expected UFW calls for Ubuntu distro")
	}
	if len(runner.callsFor("iptables")) > 0 {
		t.Error("unexpected iptables calls for Ubuntu distro")
	}
}
