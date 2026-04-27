package cmd_test

import (
	"bytes"
	"testing"

	"github.com/foundryfabric/clusterbox/cmd"
	"github.com/foundryfabric/clusterbox/internal/config"
	"github.com/spf13/cobra"
)

// stubCmd returns a minimal cobra.Command whose output goes to a buffer.
func stubCmd(buf *bytes.Buffer) *cobra.Command {
	c := &cobra.Command{}
	c.SetOut(buf)
	return c
}

func TestLogin_SavesConfig(t *testing.T) {
	var saved *config.Config

	flags := cmd.LoginFlags{
		ContextName:           "myprod",
		Hetzner:               "op://Prod/Hetzner/credential",
		TailscaleClientID:     "op://Prod/Tailscale/id",
		TailscaleClientSecret: "op://Prod/Tailscale/secret",
		Activate:              true,
	}

	err := cmd.RunLoginWith(
		flags,
		func() (*config.Config, error) { return &config.Config{}, nil },
		func(cfg *config.Config) error { saved = cfg; return nil },
		stubCmd(new(bytes.Buffer)),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if saved == nil {
		t.Fatal("saveFn was not called")
	}
	ctx, ok := saved.Contexts["myprod"]
	if !ok {
		t.Fatal("context 'myprod' not in saved config")
	}
	if ctx.Infra.Hetzner != "op://Prod/Hetzner/credential" {
		t.Errorf("Hetzner: unexpected value %q", ctx.Infra.Hetzner)
	}
	if ctx.Infra.TailscaleClientID != "op://Prod/Tailscale/id" {
		t.Errorf("TailscaleClientID: unexpected value %q", ctx.Infra.TailscaleClientID)
	}
	if ctx.Infra.TailscaleClientSecret != "op://Prod/Tailscale/secret" {
		t.Errorf("TailscaleClientSecret: unexpected value %q", ctx.Infra.TailscaleClientSecret)
	}
}

func TestLogin_ActivateByDefault(t *testing.T) {
	var saved *config.Config

	flags := cmd.LoginFlags{
		ContextName: "prod",
		Hetzner:     "op://Prod/Hetzner/credential",
		Activate:    true,
	}

	err := cmd.RunLoginWith(
		flags,
		func() (*config.Config, error) { return &config.Config{}, nil },
		func(cfg *config.Config) error { saved = cfg; return nil },
		stubCmd(new(bytes.Buffer)),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if saved.CurrentContext != "prod" {
		t.Errorf("CurrentContext: want %q, got %q", "prod", saved.CurrentContext)
	}
}

func TestLogin_ActivateFalse_SkipsActivation(t *testing.T) {
	var saved *config.Config

	flags := cmd.LoginFlags{
		ContextName: "staging",
		Hetzner:     "op://Staging/Hetzner/credential",
		Activate:    false,
	}

	err := cmd.RunLoginWith(
		flags,
		func() (*config.Config, error) {
			return &config.Config{CurrentContext: "existing"}, nil
		},
		func(cfg *config.Config) error { saved = cfg; return nil },
		stubCmd(new(bytes.Buffer)),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if saved.CurrentContext != "existing" {
		t.Errorf("CurrentContext should remain %q, got %q", "existing", saved.CurrentContext)
	}
}

func TestLogin_UpdatesExistingContext(t *testing.T) {
	var saved *config.Config

	existing := &config.Config{
		CurrentContext: "prod",
		Contexts: map[string]*config.Context{
			"prod": {
				SecretsBackend: "onepassword",
				Infra: config.InfraConfig{
					Hetzner:           "op://Old/Hetzner/credential",
					TailscaleClientID: "op://Old/Tailscale/id",
				},
			},
		},
	}

	flags := cmd.LoginFlags{
		ContextName: "prod",
		Hetzner:     "op://New/Hetzner/credential",
		Activate:    true,
	}

	err := cmd.RunLoginWith(
		flags,
		func() (*config.Config, error) { return existing, nil },
		func(cfg *config.Config) error { saved = cfg; return nil },
		stubCmd(new(bytes.Buffer)),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx := saved.Contexts["prod"]
	if ctx == nil {
		t.Fatal("context 'prod' missing after update")
	}
	// Hetzner should be updated.
	if ctx.Infra.Hetzner != "op://New/Hetzner/credential" {
		t.Errorf("Hetzner: want new value, got %q", ctx.Infra.Hetzner)
	}
	// TailscaleClientID should be preserved from the existing context.
	if ctx.Infra.TailscaleClientID != "op://Old/Tailscale/id" {
		t.Errorf("TailscaleClientID: want preserved value, got %q", ctx.Infra.TailscaleClientID)
	}
}

func TestLogin_NoFlagsNoExistingContext_PrintsUsage(t *testing.T) {
	var saved *config.Config
	out := new(bytes.Buffer)

	flags := cmd.LoginFlags{
		ContextName: "newctx",
		Activate:    true,
	}

	err := cmd.RunLoginWith(
		flags,
		func() (*config.Config, error) { return &config.Config{}, nil },
		func(cfg *config.Config) error { saved = cfg; return nil },
		stubCmd(out),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// saveFn should NOT have been called when printing usage.
	if saved != nil {
		t.Error("saveFn should not be called when no flags provided")
	}
	if out.Len() == 0 {
		t.Error("expected usage output, got nothing")
	}
}
