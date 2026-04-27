// Package onepassword implements a secrets provider backed by 1Password.
//
// Two modes are supported, selected automatically:
//
//  1. Connect API mode: set OP_CONNECT_HOST + OP_CONNECT_TOKEN.
//     Uses the 1Password Connect REST API — no op CLI dependency.
//
//  2. op CLI mode: ConnectHost is absent. The provider shells out to
//     `op item get` (GetAll) or `op read` (Get). Works with a Service
//     Account token (OP_SERVICE_ACCOUNT_TOKEN) or with ambient auth
//     from the 1Password desktop app — no extra token needed.
//
// Secret paths map as follows:
//
//	Vault: <App>  (or OP_VAULT when set, for a shared vault)
//	Item:  <Env>-<Provider>[-<Region>]  (empty parts are omitted)
//	Field: <Key>
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
	// Vault overrides the 1Password vault name for all lookups. When empty, the
	// vault name defaults to the app field of each SecretPath. Set this when all
	// services share a single vault (e.g. "platform") rather than one vault per
	// app. Populated from the OP_VAULT env var by the factory.
	Vault string
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

// ItemTitle builds the 1Password item title from env/provider/region,
// joining only the non-empty parts with "-" so that a k3d cluster with
// env="dev", provider="k3d", region="" produces "dev-k3d" rather than
// "dev-k3d-".
func ItemTitle(env, provider, region string) string {
	var parts []string
	for _, s := range []string{env, provider, region} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "-")
}

// OPPath returns the op:// URI for the CLI fallback.
func OPPath(app, env, provider, region, key string) string {
	return fmt.Sprintf("op://%s/%s/%s", app, ItemTitle(env, provider, region), key)
}

// vaultName returns the vault to look up for the given app. If cfg.Vault is
// set it overrides the app name, allowing all services to share one vault.
func (p *Provider) vaultName(app string) string {
	if p.cfg.Vault != "" {
		return p.cfg.Vault
	}
	return app
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
	name := p.vaultName(app)
	cacheKey := "vault:" + name

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
		if strings.EqualFold(v.Name, name) {
			p.mu.Lock()
			p.cache[cacheKey] = v.ID
			p.mu.Unlock()
			return v.ID, nil
		}
	}

	return "", fmt.Errorf("secrets/1password: vault %q not found", name)
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
	return p.itemFieldsFromDetail(detail), nil
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
// In Connect API mode it calls the REST API; in op CLI mode it runs
// `op item get --format json`. If the item does not exist in CLI mode
// an empty map is returned (not an error) so addons with no secrets
// install without requiring a 1Password item to be pre-created.
func (p *Provider) GetAll(ctx context.Context, app, env, provider, region string) (map[string]string, error) {
	if !p.useConnectAPI() {
		return p.cliGetAll(ctx, app, env, provider, region)
	}
	return p.connectGetAll(ctx, app, env, provider, region)
}

// cliGetAll fetches all fields for an item using `op item get --format json`.
// A missing item is treated as an empty secret bundle (not an error) so that
// addons with only optional secrets do not require the user to pre-create a
// 1Password item. Real errors (auth failure, bad vault name, etc.) surface
// when required secrets are absent and the caller reports which keys are missing.
func (p *Provider) cliGetAll(ctx context.Context, app, env, provider, region string) (map[string]string, error) {
	vault := p.vaultName(app)
	item := ItemTitle(env, provider, region)

	out, err := p.cliRun(ctx, "op", "item", "get", item, "--vault", vault, "--format", "json")
	if err != nil {
		// Item not found or other transient error — treat as empty bundle.
		// Required secrets will be caught by the installer's missing-key check.
		return map[string]string{}, nil
	}

	var detail opItemDetail
	if err := json.Unmarshal(out, &detail); err != nil {
		return nil, fmt.Errorf("secrets/1password: parse op item get output: %w", err)
	}

	return p.itemFieldsFromDetail(detail), nil
}

// itemFieldsFromDetail extracts user-defined fields from an already-fetched
// item detail, filtering out system fields (USERNAME, PASSWORD purpose strings).
func (p *Provider) itemFieldsFromDetail(detail opItemDetail) map[string]string {
	out := make(map[string]string, len(detail.Fields))
	for _, f := range detail.Fields {
		if f.Purpose != "" {
			continue
		}
		label := f.Label
		if label == "" {
			label = f.ID
		}
		out[label] = f.Value
	}
	return out
}
