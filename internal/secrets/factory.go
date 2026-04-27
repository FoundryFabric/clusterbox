package secrets

import (
	"context"
	"fmt"
	"os"

	devpkg "github.com/foundryfabric/clusterbox/internal/secrets/dev"
	oppkg "github.com/foundryfabric/clusterbox/internal/secrets/onepassword"
	vaultpkg "github.com/foundryfabric/clusterbox/internal/secrets/vault"
)

// NewProvider returns a Provider based on the SECRETS_BACKEND environment
// variable. Valid values are:
//
//	dev          — reads deploy/config/dev.secrets.json (default)
//	onepassword  — uses OP_SERVICE_ACCOUNT_TOKEN + OP_VAULT (1Password SDK)
//	vault        — uses VAULT_ADDR + VAULT_TOKEN (or AppRole credentials via
//	               VAULT_ROLE_ID + VAULT_SECRET_ID)
//
// When SECRETS_BACKEND is unset, "dev" is assumed.
func NewProvider(_ context.Context) (Provider, error) {
	backend := os.Getenv("SECRETS_BACKEND")
	if backend == "" {
		backend = "dev"
	}

	switch backend {
	case "dev":
		inner := devpkg.New("")
		return &devAdapter{p: inner}, nil

	case "onepassword":
		// OP_SERVICE_ACCOUNT_TOKEN — service account token (required)
		// OP_VAULT                 — vault name, e.g. dev-chris, staging, prod (required)
		inner := oppkg.New(oppkg.Config{
			ServiceAccountToken: os.Getenv("OP_SERVICE_ACCOUNT_TOKEN"),
			Vault:               os.Getenv("OP_VAULT"),
		})
		return &opAdapter{p: inner}, nil

	case "vault":
		inner := vaultpkg.New(vaultpkg.Config{
			Addr:     os.Getenv("VAULT_ADDR"),
			Token:    os.Getenv("VAULT_TOKEN"),
			RoleID:   os.Getenv("VAULT_ROLE_ID"),
			SecretID: os.Getenv("VAULT_SECRET_ID"),
		})
		return &vaultAdapter{p: inner}, nil

	default:
		return nil, fmt.Errorf("secrets: unknown SECRETS_BACKEND %q (want dev|onepassword|vault)", backend)
	}
}

// ---- dev adapter ----

type devAdapter struct{ p *devpkg.Provider }

func (a *devAdapter) Get(ctx context.Context, path SecretPath) (string, error) {
	return a.p.Get(ctx, path.Key)
}

func (a *devAdapter) GetAll(ctx context.Context, prefix SecretPath) (map[string]string, error) {
	return a.p.GetAll(ctx)
}

// ---- 1Password adapter ----

type opAdapter struct{ p *oppkg.Provider }

func (a *opAdapter) Get(ctx context.Context, path SecretPath) (string, error) {
	return a.p.Get(ctx, path.Provider, path.Region, path.Key)
}

func (a *opAdapter) GetAll(ctx context.Context, prefix SecretPath) (map[string]string, error) {
	return a.p.GetAll(ctx, prefix.Provider, prefix.Region)
}

// ---- Vault adapter ----

type vaultAdapter struct{ p *vaultpkg.Provider }

func (a *vaultAdapter) Get(ctx context.Context, path SecretPath) (string, error) {
	return a.p.Get(ctx, path.App, path.Env, path.Provider, path.Region, path.Key)
}

func (a *vaultAdapter) GetAll(ctx context.Context, prefix SecretPath) (map[string]string, error) {
	return a.p.GetAll(ctx, prefix.App, prefix.Env, prefix.Provider, prefix.Region)
}
