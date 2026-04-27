// Package onepassword implements a secrets provider backed by 1Password.
//
// The provider shells out to the op CLI (https://developer.1password.com/docs/cli/).
// A service account token and vault name are required:
//
//	OP_SERVICE_ACCOUNT_TOKEN=ops_...
//	OP_VAULT=dev-chris        # or staging / prod in CI
//
// Vault naming convention:
//
//	dev-<name>   personal dev vault (e.g. dev-chris, dev-alice)
//	staging      shared CI vault, read-only service account
//	prod         production CI vault, tightly gated service account
//
// Items within each vault are named <provider>[-<region>]:
//
//	k3d, hetzner-ash
//
// Fields within each item are addon secret keys:
//
//	GRAFANA_ADMIN_PASSWORD, CLICKHOUSE_ADMIN_PASSWORD, …
//
// Vault and item UUIDs are cached per Provider instance.
package onepassword

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Config holds credentials for the 1Password CLI.
type Config struct {
	// ServiceAccountToken is the 1Password service account token (ops_...).
	// Populated from OP_SERVICE_ACCOUNT_TOKEN.
	ServiceAccountToken string

	// Vault is the vault name for all lookups (e.g. dev-chris, staging, prod).
	// Populated from OP_VAULT.
	Vault string
}

// ItemTitle builds the item title from provider and region, joining only
// non-empty parts with "-". e.g. ("k3d","") → "k3d", ("hetzner","ash") → "hetzner-ash".
func ItemTitle(provider, region string) string {
	var parts []string
	for _, s := range []string{provider, region} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "-")
}

// Provider is the 1Password secrets provider.
type Provider struct {
	cfg Config

	// runFn executes op subcommands. Tests inject a fake; nil uses os/exec.
	runFn func(ctx context.Context, env []string, args ...string) ([]byte, error)

	// UUID cache (vault and item lookups).
	mu    sync.Mutex
	cache map[string]string
}

// New returns a Provider configured by cfg.
func New(cfg Config) *Provider {
	return &Provider{cfg: cfg, cache: make(map[string]string)}
}

// NewWithRunner returns a Provider with an injected CLI runner (for tests).
func NewWithRunner(cfg Config, run func(ctx context.Context, env []string, args ...string) ([]byte, error)) *Provider {
	p := New(cfg)
	p.runFn = run
	return p
}

func (p *Provider) run(ctx context.Context, args ...string) ([]byte, error) {
	if p.runFn != nil {
		return p.runFn(ctx, p.opEnv(), args...)
	}
	cmd := exec.CommandContext(ctx, "op", args...)
	cmd.Env = p.opEnv()
	return cmd.Output()
}

// opEnv returns the subprocess environment with OP_SERVICE_ACCOUNT_TOKEN injected.
func (p *Provider) opEnv() []string {
	env := os.Environ()
	if p.cfg.ServiceAccountToken != "" {
		env = append(env, "OP_SERVICE_ACCOUNT_TOKEN="+p.cfg.ServiceAccountToken)
	}
	return env
}

// Get returns a single secret field using op read.
func (p *Provider) Get(ctx context.Context, provider, region, key string) (string, error) {
	ref := fmt.Sprintf("op://%s/%s/%s", p.cfg.Vault, ItemTitle(provider, region), key)
	out, err := p.run(ctx, "read", ref)
	if err != nil {
		return "", fmt.Errorf("secrets/1password: key %q not found", key)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// GetAll returns all user-defined fields for the given cluster coordinates using
// op item get. A missing item is treated as empty (not an error) so addons with
// only optional secrets install cleanly on clusters with no item configured.
func (p *Provider) GetAll(ctx context.Context, provider, region string) (map[string]string, error) {
	item := ItemTitle(provider, region)
	out, err := p.run(ctx, "item", "get", item, "--vault", p.cfg.Vault, "--format", "json")
	if err != nil {
		return map[string]string{}, nil // item not found or auth failure → no secrets
	}

	var raw struct {
		Fields []struct {
			Label   string `json:"label"`
			Value   string `json:"value"`
			Purpose string `json:"purpose"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("secrets/1password: parse op item get output: %w", err)
	}

	result := make(map[string]string, len(raw.Fields))
	for _, f := range raw.Fields {
		if f.Label == "" || f.Purpose != "" {
			continue
		}
		result[f.Label] = f.Value
	}
	return result, nil
}
