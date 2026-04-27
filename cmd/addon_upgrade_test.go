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

// TestRunAddonUpgrade_HappyPath_FakeInstaller verifies that RunAddonUpgrade
// delegates to the injected installer and prints a one-line confirmation.
func TestRunAddonUpgrade_HappyPath_FakeInstaller(t *testing.T) {
	fi := &fakeInstaller{}
	var buf bytes.Buffer
	deps := cmd.AddonCmdDeps{Installer: fi}

	if err := cmd.RunAddonUpgrade(context.Background(), "demo", "alpha", "", &buf, deps); err != nil {
		t.Fatalf("RunAddonUpgrade: %v", err)
	}
	if got := len(fi.upgradeCalls); got != 1 {
		t.Fatalf("upgrade calls: want 1, got %d", got)
	}
	if fi.upgradeCalls[0] != (installCall{"demo", "alpha", ""}) {
		t.Errorf("upgrade call: got %+v", fi.upgradeCalls[0])
	}
	out := buf.String()
	if !strings.Contains(out, "demo") || !strings.Contains(out, "alpha") {
		t.Errorf("expected addon and cluster in output, got %q", out)
	}
	if !strings.Contains(out, "upgraded") {
		t.Errorf("expected confirmation verb in output, got %q", out)
	}
}

// TestRunAddonUpgrade_PropagatesInstallerError verifies that installer
// failures surface verbatim through the error chain.
func TestRunAddonUpgrade_PropagatesInstallerError(t *testing.T) {
	want := errors.New("kubectl apply boom")
	fi := &fakeInstaller{upgradeErr: want}
	var buf bytes.Buffer
	deps := cmd.AddonCmdDeps{Installer: fi}

	err := cmd.RunAddonUpgrade(context.Background(), "demo", "alpha", "", &buf, deps)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, want) {
		t.Errorf("error chain: want wraps %v, got %v", want, err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no success output on failure, got %q", buf.String())
	}
}

// TestRunAddonUpgrade_RequiresCluster verifies the cluster flag guard.
func TestRunAddonUpgrade_RequiresCluster(t *testing.T) {
	fi := &fakeInstaller{}
	var buf bytes.Buffer

	err := cmd.RunAddonUpgrade(context.Background(), "demo", "", "", &buf, cmd.AddonCmdDeps{Installer: fi})
	if err == nil {
		t.Fatal("expected error when --cluster is empty, got nil")
	}
	if len(fi.upgradeCalls) != 0 {
		t.Errorf("installer should not be invoked when --cluster is missing")
	}
}

// --- end-to-end --------------------------------------------------------------

// TestRunAddonUpgrade_EndToEnd_VersionAdvances seeds a stale-version row
// (v0.0.1), runs upgrade against the v1.0.0 demo catalog, and confirms the
// registry row's Version field advanced to v1.0.0.
func TestRunAddonUpgrade_EndToEnd_VersionAdvances(t *testing.T) {
	env := newAddonTestEnv(t)
	{
		reg := env.reopenRegistry()
		if err := reg.UpsertDeployment(context.Background(), registry.Deployment{
			ClusterName: "alpha",
			Service:     "demo",
			Version:     "v0.0.1",
			DeployedAt:  time.Now().UTC().Add(-time.Hour),
			Status:      registry.StatusRolledOut,
			Kind:        registry.KindAddon,
		}); err != nil {
			t.Fatalf("seed deployment: %v", err)
		}
	}

	var buf bytes.Buffer
	if err := cmd.RunAddonUpgrade(context.Background(), "demo", "alpha", "", &buf, env.deps); err != nil {
		t.Fatalf("RunAddonUpgrade: %v", err)
	}
	if !strings.Contains(buf.String(), "v1.0.0") {
		t.Errorf("expected new version in output, got %q", buf.String())
	}

	reg := env.reopenRegistry()
	d, err := reg.GetDeployment(context.Background(), "alpha", "demo")
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if d.Version != "v1.0.0" {
		t.Errorf("Version: want v1.0.0, got %q", d.Version)
	}
	if d.Status != registry.StatusRolledOut {
		t.Errorf("Status: want rolled_out, got %q", d.Status)
	}
}
