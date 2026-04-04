// Package onepassword implements a secrets provider backed by 1Password.
//
// Two modes are supported, selected automatically:
//
//  1. Connect API mode: set OP_CONNECT_HOST + OP_CONNECT_TOKEN.
//     Uses the 1Password Connect REST API — no op CLI dependency.
//
//  2. op CLI fallback: if ConnectHost is absent but ServiceAccountToken is set,
//     the provider shells out to `op read "op://<vault>/<item>/<field>"`.
//
// Secret paths map as follows:
//
//	op://<App>/<Env>-<Provider>-<Region>/<Key>
//
// UUID lookups (vault + item) are cached per Provider instance.
//
// This package does not import the root internal/secrets package to avoid
// import cycles. The root package wraps this via an adapter.
package onepassword

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
)

// Config holds the credentials for the 1Password backend.
type Config struct {
	// ConnectHost is the base URL of the 1Password Connect server
	// (e.g. http://localhost:8080). When set, the Connect API is used.
	ConnectHost string
	// ConnectToken is the bearer token for the Connect API.
	ConnectToken string
	// ServiceAccountToken is used for the op CLI fallback when ConnectHost is empty.
	ServiceAccountToken string
}

// HTTPClient is the interface used for Connect API calls.
// Tests inject a fake; production code uses http.DefaultClient.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Provider is the 1Password secrets provider.
type Provider struct {
	cfg    Config
	client HTTPClient

	// mu protects the UUID cache.
	mu    sync.Mutex
	cache map[string]string

	// runFn is the command executor for op CLI fallback. Tests inject a fake.
	runFn func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// New returns a Provider configured by cfg.
func New(cfg Config) *Provider {
	return &Provider{cfg: cfg, cache: make(map[string]string)}
}

// NewWithClient returns a Provider with an injected HTTP client (for tests).
func NewWithClient(cfg Config, client HTTPClient) *Provider {
	p := New(cfg)
	p.client = client
	return p
}

// NewWithRunner returns a Provider with an injected CLI runner (for tests).
func NewWithRunner(cfg Config, run func(ctx context.Context, name string, args ...string) ([]byte, error)) *Provider {
	p := New(cfg)
	p.runFn = run
	return p
}

func (p *Provider) httpClient() HTTPClient {
	if p.client != nil {
		return p.client
	}
	return http.DefaultClient
}

// ItemTitle builds the 1Password item title from env/provider/region.
func ItemTitle(env, provider, region string) string {
	return fmt.Sprintf("%s-%s-%s", env, provider, region)
}

// OPPath returns the op:// URI for the CLI fallback.
func OPPath(app, env, provider, region, key string) string {
	return fmt.Sprintf("op://%s/%s/%s", app, ItemTitle(env, provider, region), key)
}

// ---- Connect API types ----

type opVault struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type opItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type opField struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Value   string `json:"value"`
	Purpose string `json:"purpose"` // non-empty means system field (USERNAME, PASSWORD, etc.)
}

type opItemDetail struct {
	ID     string    `json:"id"`
	Fields []opField `json:"fields"`
}

func (p *Provider) doJSON(ctx context.Context, method, url string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.ConnectToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("secrets/1password: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

func (p *Provider) vaultUUID(ctx context.Context, app string) (string, error) {
	cacheKey := "vault:" + app

	p.mu.Lock()
	if uuid, ok := p.cache[cacheKey]; ok {
		p.mu.Unlock()
		return uuid, nil
	}
	p.mu.Unlock()

	url := strings.TrimRight(p.cfg.ConnectHost, "/") + "/v1/vaults"
	var vaults []opVault
	if err := p.doJSON(ctx, http.MethodGet, url, &vaults); err != nil {
		return "", fmt.Errorf("secrets/1password: list vaults: %w", err)
	}

	for _, v := range vaults {
		if strings.EqualFold(v.Name, app) {
			p.mu.Lock()
			p.cache[cacheKey] = v.ID
			p.mu.Unlock()
			return v.ID, nil
		}
	}

	return "", fmt.Errorf("secrets/1password: vault %q not found", app)
}

func (p *Provider) itemUUID(ctx context.Context, vaultID, title string) (string, error) {
	cacheKey := "item:" + vaultID + "/" + title

	p.mu.Lock()
	if uuid, ok := p.cache[cacheKey]; ok {
		p.mu.Unlock()
		return uuid, nil
	}
	p.mu.Unlock()

	url := fmt.Sprintf("%s/v1/vaults/%s/items", strings.TrimRight(p.cfg.ConnectHost, "/"), vaultID)
	var items []opItem
	if err := p.doJSON(ctx, http.MethodGet, url, &items); err != nil {
		return "", fmt.Errorf("secrets/1password: list items in vault %s: %w", vaultID, err)
	}

	for _, it := range items {
		if strings.EqualFold(it.Title, title) {
			p.mu.Lock()
			p.cache[cacheKey] = it.ID
			p.mu.Unlock()
			return it.ID, nil
		}
	}

	return "", fmt.Errorf("secrets/1password: item %q not found in vault %s", title, vaultID)
}

func (p *Provider) itemFields(ctx context.Context, vaultID, itemID string) (map[string]string, error) {
	url := fmt.Sprintf("%s/v1/vaults/%s/items/%s",
		strings.TrimRight(p.cfg.ConnectHost, "/"), vaultID, itemID)
	var detail opItemDetail
	if err := p.doJSON(ctx, http.MethodGet, url, &detail); err != nil {
		return nil, fmt.Errorf("secrets/1password: get item %s: %w", itemID, err)
	}

	out := make(map[string]string)
	for _, f := range detail.Fields {
		if f.Purpose != "" {
			continue // skip system fields
		}
		label := f.Label
		if label == "" {
			label = f.ID
		}
		out[label] = f.Value
	}
	return out, nil
}

// connectGetAll fetches all user-defined fields for the given coordinates.
func (p *Provider) connectGetAll(ctx context.Context, app, env, provider, region string) (map[string]string, error) {
	vUUID, err := p.vaultUUID(ctx, app)
	if err != nil {
		return nil, err
	}

	title := ItemTitle(env, provider, region)
	iUUID, err := p.itemUUID(ctx, vUUID, title)
	if err != nil {
		return nil, err
	}

	return p.itemFields(ctx, vUUID, iUUID)
}

// ---- CLI fallback ----

func (p *Provider) cliRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	if p.runFn != nil {
		return p.runFn(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if p.cfg.ServiceAccountToken != "" {
		cmd.Env = append(cmd.Environ(), "OP_SERVICE_ACCOUNT_TOKEN="+p.cfg.ServiceAccountToken)
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (p *Provider) useConnectAPI() bool {
	return p.cfg.ConnectHost != ""
}

// Get returns the value for key within the item identified by app/env/provider/region.
// In CLI fallback mode it shells out to `op read`.
func (p *Provider) Get(ctx context.Context, app, env, provider, region, key string) (string, error) {
	if !p.useConnectAPI() {
		ref := OPPath(app, env, provider, region, key)
		out, err := p.cliRun(ctx, "op", "read", ref)
		if err != nil {
			// Deliberately omit the path to avoid leaking credential naming conventions.
			return "", fmt.Errorf("secrets/1password: op CLI failed to read key %q", key)
		}
		return strings.TrimRight(string(out), "\n"), nil
	}

	m, err := p.connectGetAll(ctx, app, env, provider, region)
	if err != nil {
		return "", err
	}

	val, ok := m[key]
	if !ok {
		return "", fmt.Errorf("secrets/1password: key %q not found in item %s",
			key, ItemTitle(env, provider, region))
	}
	return val, nil
}

// GetAll returns all user-defined fields for the given coordinates.
// Not available in CLI fallback mode.
func (p *Provider) GetAll(ctx context.Context, app, env, provider, region string) (map[string]string, error) {
	if !p.useConnectAPI() {
		return nil, fmt.Errorf("secrets/1password: GetAll is not supported in op CLI fallback mode")
	}
	return p.connectGetAll(ctx, app, env, provider, region)
}
