package cmd_test

import (
	"testing"

	"github.com/foundryfabric/clusterbox/cmd"
	"github.com/foundryfabric/clusterbox/internal/config"
)

func TestUseContext_SwitchesContext(t *testing.T) {
	var saved *config.Config

	existing := &config.Config{
		CurrentContext: "old",
		Contexts: map[string]*config.Context{
			"old":     {SecretsBackend: "onepassword"},
			"newctx":  {SecretsBackend: "onepassword"},
		},
	}

	err := cmd.RunUseContextWith(
		"newctx",
		func() (*config.Config, error) { return existing, nil },
		func(cfg *config.Config) error { saved = cfg; return nil },
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if saved == nil {
		t.Fatal("saveFn was not called")
	}
	if saved.CurrentContext != "newctx" {
		t.Errorf("CurrentContext: want %q, got %q", "newctx", saved.CurrentContext)
	}
}

func TestUseContext_NotFound_Error(t *testing.T) {
	existing := &config.Config{
		Contexts: map[string]*config.Context{
			"prod": {SecretsBackend: "onepassword"},
		},
	}

	err := cmd.RunUseContextWith(
		"missing",
		func() (*config.Config, error) { return existing, nil },
		func(cfg *config.Config) error { return nil },
	)
	if err == nil {
		t.Fatal("expected error for missing context, got nil")
	}
}

func TestUseContext_NilContextsMap_Error(t *testing.T) {
	existing := &config.Config{}

	err := cmd.RunUseContextWith(
		"anything",
		func() (*config.Config, error) { return existing, nil },
		func(cfg *config.Config) error { return nil },
	)
	if err == nil {
		t.Fatal("expected error for nil contexts map, got nil")
	}
}
