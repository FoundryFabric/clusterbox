package cmd_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/cmd"
	"github.com/foundryfabric/clusterbox/internal/registry"
)

// TestRunAddonUninstall_WithYesFlag_SkipsPrompt verifies that --yes bypasses
// the confirmation prompt and invokes the installer.
func TestRunAddonUninstall_WithYesFlag_SkipsPrompt(t *testing.T) {
	fi := &fakeInstaller{}
	var out bytes.Buffer

	// in is empty: if the prompt fired it would block or yield an empty
	// reply (which would decline) — either way the installer would not be
	// called. So observing one Uninstall call proves the prompt was skipped.
	in := strings.NewReader("")

	if err := cmd.RunAddonUninstall(context.Background(), "demo", "alpha", true, in, &out, cmd.AddonCmdDeps{Installer: fi}); err != nil {
		t.Fatalf("RunAddonUninstall: %v", err)
	}
	if got := len(fi.uninstallCalls); got != 1 {
		t.Fatalf("uninstall calls: want 1, got %d", got)
	}
	if fi.uninstallCalls[0] != (installCall{"demo", "alpha", ""}) {
		t.Errorf("uninstall call: got %+v", fi.uninstallCalls[0])
	}
	got := out.String()
	if !strings.Contains(got, "demo") || !strings.Contains(got, "alpha") {
		t.Errorf("expected addon and cluster in output, got %q", got)
	}
	if strings.Contains(got, "[y/N]") {
		t.Errorf("--yes should suppress the prompt, got %q", got)
	}
}

// TestRunAddonUninstall_PromptYesProceeds verifies that an interactive "y"
// reply allows the uninstall to run.
func TestRunAddonUninstall_PromptYesProceeds(t *testing.T) {
	fi := &fakeInstaller{}
	var out bytes.Buffer
	in := strings.NewReader("y\n")

	if err := cmd.RunAddonUninstall(context.Background(), "demo", "alpha", false, in, &out, cmd.AddonCmdDeps{Installer: fi}); err != nil {
		t.Fatalf("RunAddonUninstall: %v", err)
	}
	if got := len(fi.uninstallCalls); got != 1 {
		t.Fatalf("uninstall calls: want 1, got %d", got)
	}
	if !strings.Contains(out.String(), "[y/N]") {
		t.Errorf("expected prompt in output, got %q", out.String())
	}
}

// TestRunAddonUninstall_PromptNoAborts verifies that an interactive "n"
// reply aborts before invoking the installer and returns nil (a user
// declining is not an error).
func TestRunAddonUninstall_PromptNoAborts(t *testing.T) {
	fi := &fakeInstaller{}
	var out bytes.Buffer
	in := strings.NewReader("n\n")

	if err := cmd.RunAddonUninstall(context.Background(), "demo", "alpha", false, in, &out, cmd.AddonCmdDeps{Installer: fi}); err != nil {
		t.Fatalf("RunAddonUninstall: %v", err)
	}
	if got := len(fi.uninstallCalls); got != 0 {
		t.Fatalf("installer must not be called on decline; got %d calls", got)
	}
	if !strings.Contains(out.String(), "aborted") {
		t.Errorf("expected abort message, got %q", out.String())
	}
}

// TestRunAddonUninstall_EmptyReplyAborts verifies that an empty reply
// (the operator just hit Enter) aborts: default is No.
func TestRunAddonUninstall_EmptyReplyAborts(t *testing.T) {
	fi := &fakeInstaller{}
	var out bytes.Buffer
	in := strings.NewReader("\n")

	if err := cmd.RunAddonUninstall(context.Background(), "demo", "alpha", false, in, &out, cmd.AddonCmdDeps{Installer: fi}); err != nil {
		t.Fatalf("RunAddonUninstall: %v", err)
	}
	if got := len(fi.uninstallCalls); got != 0 {
		t.Fatalf("installer must not be called on empty reply; got %d calls", got)
	}
}

// TestRunAddonUninstall_EOFAborts verifies that an immediate EOF (e.g.
// stdin attached to /dev/null in CI) declines rather than erroring.
func TestRunAddonUninstall_EOFAborts(t *testing.T) {
	fi := &fakeInstaller{}
	var out bytes.Buffer
	in := strings.NewReader("")

	if err := cmd.RunAddonUninstall(context.Background(), "demo", "alpha", false, in, &out, cmd.AddonCmdDeps{Installer: fi}); err != nil {
		t.Fatalf("RunAddonUninstall: %v", err)
	}
	if got := len(fi.uninstallCalls); got != 0 {
		t.Fatalf("installer must not be called on EOF; got %d calls", got)
	}
}

// TestRunAddonUninstall_PropagatesInstallerError verifies that a kubectl
// failure inside the installer surfaces verbatim.
func TestRunAddonUninstall_PropagatesInstallerError(t *testing.T) {
	want := errors.New("kubectl delete boom")
	fi := &fakeInstaller{uninstallErr: want}
	var out bytes.Buffer

	err := cmd.RunAddonUninstall(context.Background(), "demo", "alpha", true, strings.NewReader(""), &out, cmd.AddonCmdDeps{Installer: fi})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, want) {
		t.Errorf("error chain: want wraps %v, got %v", want, err)
	}
}

// --- end-to-end --------------------------------------------------------------

// TestRunAddonUninstall_EndToEnd_RowRemoved seeds a registry row, uninstalls,
// and confirms the deployments row is gone.
func TestRunAddonUninstall_EndToEnd_RowRemoved(t *testing.T) {
	env := newAddonTestEnv(t)

	// Seed an installed deployment row directly so we don't have to install
	// first (Install is covered by its own end-to-end test).
	{
		reg := env.reopenRegistry()
		if err := reg.UpsertDeployment(context.Background(), registry.Deployment{
			ClusterName: "alpha",
			Service:     "demo",
			Version:     "v1.0.0",
			DeployedAt:  time.Now().UTC(),
			Status:      registry.StatusRolledOut,
			Kind:        registry.KindAddon,
		}); err != nil {
			t.Fatalf("seed deployment: %v", err)
		}
	}

	var out bytes.Buffer
	if err := cmd.RunAddonUninstall(context.Background(), "demo", "alpha", true, strings.NewReader(""), &out, env.deps); err != nil {
		t.Fatalf("RunAddonUninstall: %v", err)
	}

	reg := env.reopenRegistry()
	if _, err := reg.GetDeployment(context.Background(), "alpha", "demo"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("deployments row should be removed; GetDeployment err=%v", err)
	}

	// kubectl delete must have been invoked.
	var deleted bool
	for _, c := range env.runner.calls {
		for _, a := range c.args {
			if a == "delete" {
				deleted = true
			}
		}
	}
	if !deleted {
		t.Errorf("expected at least one kubectl delete call, got %+v", env.runner.calls)
	}
}

// TestRunAddonUninstall_EndToEnd_DeclineSkipsKubectl verifies that declining
// the prompt issues no kubectl call and leaves the registry row intact.
func TestRunAddonUninstall_EndToEnd_DeclineSkipsKubectl(t *testing.T) {
	env := newAddonTestEnv(t)
	{
		reg := env.reopenRegistry()
		if err := reg.UpsertDeployment(context.Background(), registry.Deployment{
			ClusterName: "alpha",
			Service:     "demo",
			Version:     "v1.0.0",
			DeployedAt:  time.Now().UTC(),
			Status:      registry.StatusRolledOut,
			Kind:        registry.KindAddon,
		}); err != nil {
			t.Fatalf("seed deployment: %v", err)
		}
	}

	var out bytes.Buffer
	if err := cmd.RunAddonUninstall(context.Background(), "demo", "alpha", false, strings.NewReader("n\n"), &out, env.deps); err != nil {
		t.Fatalf("RunAddonUninstall: %v", err)
	}
	if got := len(env.runner.calls); got != 0 {
		t.Errorf("expected no kubectl calls on decline, got %d (%+v)", got, env.runner.calls)
	}

	reg := env.reopenRegistry()
	d, err := reg.GetDeployment(context.Background(), "alpha", "demo")
	if err != nil {
		t.Fatalf("deployments row should still exist after decline; GetDeployment err=%v", err)
	}
	if d.Service != "demo" {
		t.Errorf("unexpected row after decline: %+v", d)
	}
}
