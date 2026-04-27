// Package vault implements a secrets provider backed by HashiCorp Vault KV v2.
//
// Authentication supports two modes:
//
//  1. Token auth: set VAULT_TOKEN (or pass Token in Config).
//  2. AppRole auth: set VAULT_ROLE_ID + VAULT_SECRET_ID.
//     A client token is fetched lazily on first use and cached for the lifetime
//     of the Provider.
//
// KV v2 path layout:
//
//	secret/data/<App>/<Env>/<Provider>/<Region>
//
// Only net/http is used — no HashiCorp SDK — to keep the dependency footprint
// minimal.
//
// This package does not import the root internal/secrets package to avoid
// import cycles. The root package wraps this via an adapter.
package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Config holds the connection parameters for the Vault backend.
type Config struct {
	// Addr is the base URL of the Vault server (e.g. http://vault.internal:8200).
	Addr string
	// Token is a Vault token (token auth). May be empty if RoleID+SecretID are set.
	Token string
	// RoleID and SecretID enable AppRole authentication.
	RoleID   string
	SecretID string
}

// HTTPClient is the interface used for Vault HTTP calls.
// Tests inject a fake; production code uses http.DefaultClient.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Provider is the Vault secrets provider.
type Provider struct {
	cfg    Config
	client HTTPClient

	mu    sync.Mutex
	token string // lazily resolved from AppRole or copied from cfg.Token
}

// New returns a Provider configured by cfg.
func New(cfg Config) *Provider {
	return &Provider{cfg: cfg, token: cfg.Token}
}

// NewWithClient returns a Provider with an injected HTTP client (for tests).
func NewWithClient(cfg Config, client HTTPClient) *Provider {
	p := New(cfg)
	p.client = client
	return p
}

func (p *Provider) httpClient() HTTPClient {
	if p.client != nil {
		return p.client
	}
	return http.DefaultClient
}

// kvPath returns the KV v2 data path for the given coordinates.
func kvPath(app, env, provider, region string) string {
	return fmt.Sprintf("secret/data/%s/%s/%s/%s", app, env, provider, region)
}

// ensureToken returns a valid Vault token, performing AppRole login if necessary.
func (p *Provider) ensureToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.token != "" {
		return p.token, nil
	}

	if p.cfg.RoleID == "" || p.cfg.SecretID == "" {
		return "", fmt.Errorf("secrets/vault: no VAULT_TOKEN and no AppRole credentials (set VAULT_ROLE_ID + VAULT_SECRET_ID)")
	}

	tok, err := p.appRoleLogin(ctx)
	if err != nil {
		return "", err
	}
	p.token = tok
	return tok, nil
}

// appRoleLogin performs POST /v1/auth/approle/login and returns the client token.
func (p *Provider) appRoleLogin(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"role_id":   p.cfg.RoleID,
		"secret_id": p.cfg.SecretID,
	})

	url := strings.TrimRight(p.cfg.Addr, "/") + "/v1/auth/approle/login"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("secrets/vault: build AppRole login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("secrets/vault: AppRole login request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("secrets/vault: AppRole login HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var result struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("secrets/vault: decode AppRole login response: %w", err)
	}
	if result.Auth.ClientToken == "" {
		return "", fmt.Errorf("secrets/vault: AppRole login returned empty client_token")
	}
	return result.Auth.ClientToken, nil
}

// readKV fetches the KV v2 data map for the given path coordinates.
func (p *Provider) readKV(ctx context.Context, app, env, provider, region string) (map[string]string, error) {
	tok, err := p.ensureToken(ctx)
	if err != nil {
		return nil, err
	}

	path := kvPath(app, env, provider, region)
	url := strings.TrimRight(p.cfg.Addr, "/") + "/v1/" + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("secrets/vault: build KV request: %w", err)
	}
	req.Header.Set("X-Vault-Token", tok)

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("secrets/vault: KV read: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("secrets/vault: path %q not found", path)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("secrets/vault: KV read HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	// Vault KV v2 response envelope:
	// {"data": {"data": {"KEY": "value", ...}, "metadata": {...}}}
	var envelope struct {
		Data struct {
			Data map[string]string `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("secrets/vault: decode KV response: %w", err)
	}

	return envelope.Data.Data, nil
}

// Get returns a single secret value from the KV store.
func (p *Provider) Get(ctx context.Context, app, env, provider, region, key string) (string, error) {
	m, err := p.readKV(ctx, app, env, provider, region)
	if err != nil {
		return "", err
	}

	val, ok := m[key]
	if !ok {
		return "", fmt.Errorf("secrets/vault: key %q not found at %s",
			key, kvPath(app, env, provider, region))
	}
	return val, nil
}

// GetAll returns the full data map from the KV secret.
func (p *Provider) GetAll(ctx context.Context, app, env, provider, region string) (map[string]string, error) {
	return p.readKV(ctx, app, env, provider, region)
}
