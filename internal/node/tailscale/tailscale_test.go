package tailscale_test

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/node/tailscale"
)

// fakeFS is a test FS that reports TailscaleBinary as present or absent.
type fakeFS struct{ present bool }

func (f fakeFS) Stat(_ string) (fs.FileInfo, error) {
	if f.present {
		return nil, nil // non-nil FileInfo not needed for these tests
	}
	return nil, fs.ErrNotExist
}

// fakeRunner records calls and returns canned outputs.
type fakeRunner struct {
	runs   []string
	output string
	err    error
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	parts := append([]string{name}, args...)
	r.runs = append(r.runs, strings.Join(parts, " "))
	if r.err != nil {
		return nil, r.err
	}
	return []byte(r.output), nil
}

func (r *fakeRunner) RunShell(_ context.Context, _ []string, script string) ([]byte, error) {
	r.runs = append(r.runs, "shell:"+script)
	return nil, r.err
}

// TestApply_Disabled returns Applied=false when Tailscale spec is nil.
func TestApply_Disabled(t *testing.T) {
	sec := &tailscale.Section{}
	res, err := sec.Apply(context.Background(), &config.Spec{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied {
		t.Error("applied=true, want false for nil Tailscale spec")
	}
	if res.Reason != "disabled" {
		t.Errorf("reason=%q, want disabled", res.Reason)
	}
}

// TestApply_DisabledExplicit returns Applied=false when Enabled=false.
func TestApply_DisabledExplicit(t *testing.T) {
	sec := &tailscale.Section{}
	spec := &config.Spec{Tailscale: &config.TailscaleSpec{Enabled: false}}
	res, err := sec.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied {
		t.Error("applied=true, want false when enabled=false")
	}
}

// TestApply_Install installs and brings up Tailscale when not present.
func TestApply_Install(t *testing.T) {
	runner := &fakeRunner{output: "100.64.0.1"}
	sec := &tailscale.Section{
		Runner: runner,
		FS:     fakeFS{present: false},
	}
	spec := &config.Spec{
		Tailscale: &config.TailscaleSpec{
			Enabled:  true,
			AuthKey:  "tskey-auth-test",
			Hostname: "my-node",
		},
	}
	res, err := sec.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("applied=false, want true")
	}
	if res.Reason != "" {
		t.Errorf("reason=%q, want empty (fresh install)", res.Reason)
	}
	// Install script must have been called.
	foundInstall := false
	for _, call := range runner.runs {
		if len(call) > 6 && call[:6] == "shell:" {
			foundInstall = true
		}
	}
	if !foundInstall {
		t.Errorf("install script not called; runs=%v", runner.runs)
	}
	// tailscale up must include --authkey and --hostname.
	foundUp := false
	for _, call := range runner.runs {
		if strings.Contains(call, "--authkey=tskey-auth-test") && strings.Contains(call, "--hostname=my-node") {
			foundUp = true
		}
	}
	if !foundUp {
		t.Errorf("tailscale up not called with expected args; runs=%v", runner.runs)
	}
}

// TestApply_AlreadyInstalled skips the install script when binary is present.
func TestApply_AlreadyInstalled(t *testing.T) {
	runner := &fakeRunner{}
	sec := &tailscale.Section{
		Runner: runner,
		FS:     fakeFS{present: true},
	}
	spec := &config.Spec{
		Tailscale: &config.TailscaleSpec{
			Enabled: true,
			AuthKey: "tskey-auth-test",
		},
	}
	res, err := sec.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied {
		t.Errorf("applied=false, want true")
	}
	if res.Reason != "already installed" {
		t.Errorf("reason=%q, want already installed", res.Reason)
	}
	for _, call := range runner.runs {
		if len(call) > 6 && call[:6] == "shell:" {
			t.Errorf("install script called when binary already present; runs=%v", runner.runs)
		}
	}
}

// TestApply_AuthKeyEnvMissing returns an error when the env var is unset.
func TestApply_AuthKeyEnvMissing(t *testing.T) {
	sec := &tailscale.Section{Runner: &fakeRunner{}, FS: fakeFS{present: false}}
	spec := &config.Spec{
		Tailscale: &config.TailscaleSpec{
			Enabled:    true,
			AuthKeyEnv: "TS_AUTH_KEY_DOES_NOT_EXIST_XYZ",
		},
	}
	_, err := sec.Apply(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error for missing auth key env, got nil")
	}
}

// TestRemove_NotPresent returns Applied=false when tailscale is not installed.
func TestRemove_NotPresent(t *testing.T) {
	sec := &tailscale.Section{Runner: &fakeRunner{}, FS: fakeFS{present: false}}
	res, err := sec.Remove(context.Background(), &config.Spec{})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if res.Applied {
		t.Error("applied=true, want false when not installed")
	}
	if res.Reason != "tailscale not installed" {
		t.Errorf("reason=%q, want tailscale not installed", res.Reason)
	}
}

// TestRemove_Logout calls tailscale logout when present.
func TestRemove_Logout(t *testing.T) {
	runner := &fakeRunner{}
	sec := &tailscale.Section{Runner: runner, FS: fakeFS{present: true}}
	res, err := sec.Remove(context.Background(), &config.Spec{})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !res.Applied {
		t.Error("applied=false, want true")
	}
	foundLogout := false
	for _, call := range runner.runs {
		if strings.Contains(call, "logout") {
			foundLogout = true
		}
	}
	if !foundLogout {
		t.Errorf("tailscale logout not called; runs=%v", runner.runs)
	}
}

// TestApply_InstallError propagates runner errors from the install script.
func TestApply_InstallError(t *testing.T) {
	runner := &fakeRunner{err: errors.New("curl failed")}
	sec := &tailscale.Section{Runner: runner, FS: fakeFS{present: false}}
	spec := &config.Spec{
		Tailscale: &config.TailscaleSpec{
			Enabled: true,
			AuthKey: "tskey-auth-test",
		},
	}
	_, err := sec.Apply(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestApply_DefaultTimeout verifies Apply respects context cancellation.
func TestApply_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	runner := &fakeRunner{err: context.DeadlineExceeded}
	sec := &tailscale.Section{Runner: runner, FS: fakeFS{present: false}}
	spec := &config.Spec{
		Tailscale: &config.TailscaleSpec{
			Enabled: true,
			AuthKey: "tskey-auth-test",
		},
	}
	_, err := sec.Apply(ctx, spec)
	if err == nil {
		t.Fatal("expected error on cancelled context, got nil")
	}
}

