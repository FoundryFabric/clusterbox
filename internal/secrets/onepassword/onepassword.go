// Package onepassword implements a secrets provider backed by 1Password.
//
// Three modes, selected automatically:
//
//  1. SDK mode (recommended): set OP_SERVICE_ACCOUNT_TOKEN.
//     Uses the official 1Password Go SDK — no CLI, no Connect server required.
//
//  2. Connect API mode (legacy): set OP_CONNECT_HOST + OP_CONNECT_TOKEN.
//     Talks directly to a 1Password Connect server via HTTP.
//
//  3. op CLI fallback: neither token is set.
//     Shells out to `op item get` / `op read`. Works with ambient auth
//     from the 1Password desktop app — no service account token needed.
//
// Secret path convention:
//
//	Vault: <App>  (or OP_VAULT when set, for a shared vault)
//	Item:  <Env>-<Provider>[-<Region>]  (empty parts omitted)
//	Field: <Key>
//
// For a k3d cluster named "dev": vault="my-addon" item="dev-k3d" field="MY_SECRET"
// With OP_VAULT=clusterbox:        vault="clusterbox"   item="dev-k3d" field="MY_SECRET"
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

	op "github.com/1password/onepassword-sdk-go"
)

// Config holds credentials for the 1Password backend.
type Config struct {
	// ServiceAccountToken enables SDK mode (recommended).
	// Populated from OP_SERVICE_ACCOUNT_TOKEN.
	ServiceAccountToken string

	// ConnectHost + ConnectToken enable Connect API mode (legacy).
	// Populated from OP_CONNECT_HOST + OP_CONNECT_TOKEN.
	ConnectHost  string
	ConnectToken string

	// Vault overrides the vault name for all lookups.
	// When empty the vault defaults to the addon (app) name.
	// Populated from OP_VAULT.
	Vault string
}

// ItemTitle builds the 1Password item title from env/provider/region,
// joining only the non-empty parts with "-".
// e.g. ("dev","k3d","") → "dev-k3d", ("dev","hetzner","ash") → "dev-hetzner-ash".
func ItemTitle(env, provider, region string) string {
	var parts []string
	for _, s := range []string{env, provider, region} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "-")
}

// OPPath returns the op:// URI used in CLI fallback mode.
func OPPath(app, env, provider, region, key string) string {
	return fmt.Sprintf("op://%s/%s/%s", app, ItemTitle(env, provider, region), key)
}

// ---- narrow interfaces used internally (keeps tests slim) ------------------

type sdkSecretsAPI interface {
	Resolve(ctx context.Context, secretReference string) (string, error)
}

type sdkItemsAPI interface {
	Get(ctx context.Context, vaultID string, itemID string) (op.Item, error)
	List(ctx context.Context, vaultID string, filters ...op.ItemListFilter) ([]op.ItemOverview, error)
}

type sdkVaultsAPI interface {
	List(ctx context.Context, params ...op.VaultListParams) ([]op.VaultOverview, error)
}

// sdkHandle groups the three API handles produced by an SDK client.
type sdkHandle struct {
	secrets sdkSecretsAPI
	items   sdkItemsAPI
	vaults  sdkVaultsAPI
}

// ---- provider ---------------------------------------------------------------

// Provider is the 1Password secrets provider.
type Provider struct {
	cfg Config

	// SDK mode — lazily initialised on first use.
	sdkOnce sync.Once
	sdkHnd  *sdkHandle
	sdkErr  error

	// injectSDK allows tests to bypass NewClient.
	injectSDK *sdkHandle

	// UUID cache shared across modes.
	mu    sync.Mutex
	cache map[string]string

	// runFn executes op subcommands (CLI mode). Tests inject a fake.
	runFn func(ctx context.Context, name string, args ...string) ([]byte, error)

	// httpClient is used for Connect API mode. Tests inject a fake.
	httpClient httpDoer
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// New returns a Provider configured by cfg.
func New(cfg Config) *Provider {
	return &Provider{cfg: cfg, cache: make(map[string]string)}
}

// NewWithSDK returns a Provider with an injected SDK handle (for tests).
func NewWithSDK(cfg Config, hnd *sdkHandle) *Provider {
	p := New(cfg)
	p.injectSDK = hnd
	return p
}

// NewWithRunner returns a Provider with an injected CLI runner (for tests).
func NewWithRunner(cfg Config, run func(ctx context.Context, name string, args ...string) ([]byte, error)) *Provider {
	p := New(cfg)
	p.runFn = run
	return p
}

// NewWithClient returns a Provider with an injected HTTP client (for Connect API tests).
func NewWithClient(cfg Config, client httpDoer) *Provider {
	p := New(cfg)
	p.httpClient = client
	return p
}

func (p *Provider) vaultName(app string) string {
	if p.cfg.Vault != "" {
		return p.cfg.Vault
	}
	return app
}

// ---- mode selection ---------------------------------------------------------

func (p *Provider) useSDK() bool      { return p.cfg.ServiceAccountToken != "" }
func (p *Provider) useConnect() bool  { return !p.useSDK() && p.cfg.ConnectHost != "" }

// getSDK initialises the SDK client once and returns the handle.
func (p *Provider) getSDK(ctx context.Context) (*sdkHandle, error) {
	if p.injectSDK != nil {
		return p.injectSDK, nil
	}
	p.sdkOnce.Do(func() {
		client, err := op.NewClient(ctx,
			op.WithServiceAccountToken(p.cfg.ServiceAccountToken),
			op.WithIntegrationInfo("clusterbox", "v0"),
		)
		if err != nil {
			p.sdkErr = fmt.Errorf("secrets/1password: init SDK client: %w", err)
			return
		}
		p.sdkHnd = &sdkHandle{
			secrets: client.Secrets(),
			items:   client.Items(),
			vaults:  client.Vaults(),
		}
	})
	return p.sdkHnd, p.sdkErr
}

// ---- Get --------------------------------------------------------------------

func (p *Provider) Get(ctx context.Context, app, env, provider, region, key string) (string, error) {
	switch {
	case p.useSDK():
		return p.sdkGet(ctx, app, env, provider, region, key)
	case p.useConnect():
		return p.connectGet(ctx, app, env, provider, region, key)
	default:
		return p.cliGet(ctx, app, env, provider, region, key)
	}
}

// GetAll returns all user-defined fields for the given coordinates.
// A missing vault or item is treated as an empty bundle (not an error) so that
// addons with only optional secrets install without requiring a pre-created item.
func (p *Provider) GetAll(ctx context.Context, app, env, provider, region string) (map[string]string, error) {
	switch {
	case p.useSDK():
		return p.sdkGetAll(ctx, app, env, provider, region)
	case p.useConnect():
		return p.connectGetAll(ctx, app, env, provider, region)
	default:
		return p.cliGetAll(ctx, app, env, provider, region)
	}
}

// ---- SDK mode ---------------------------------------------------------------

func (p *Provider) sdkGet(ctx context.Context, app, env, provider, region, key string) (string, error) {
	hnd, err := p.getSDK(ctx)
	if err != nil {
		return "", err
	}
	vault := p.vaultName(app)
	ref := fmt.Sprintf("op://%s/%s/%s", vault, ItemTitle(env, provider, region), key)
	val, err := hnd.secrets.Resolve(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("secrets/1password: key %q not found", key)
	}
	return val, nil
}

func (p *Provider) sdkGetAll(ctx context.Context, app, env, provider, region string) (map[string]string, error) {
	hnd, err := p.getSDK(ctx)
	if err != nil {
		return nil, err
	}

	vault := p.vaultName(app)
	item := ItemTitle(env, provider, region)

	vaultID, err := p.sdkVaultUUID(ctx, hnd, vault)
	if err != nil {
		return map[string]string{}, nil // vault not found → no secrets configured
	}
	itemID, err := p.sdkItemUUID(ctx, hnd, vaultID, item)
	if err != nil {
		return map[string]string{}, nil // item not found → no secrets configured
	}

	itm, err := hnd.items.Get(ctx, vaultID, itemID)
	if err != nil {
		return nil, fmt.Errorf("secrets/1password: get item %s/%s: %w", vault, item, err)
	}
	return fieldsFromItem(itm), nil
}

func (p *Provider) sdkVaultUUID(ctx context.Context, hnd *sdkHandle, name string) (string, error) {
	cacheKey := "vault:" + name
	p.mu.Lock()
	if id, ok := p.cache[cacheKey]; ok {
		p.mu.Unlock()
		return id, nil
	}
	p.mu.Unlock()

	vaults, err := hnd.vaults.List(ctx)
	if err != nil {
		return "", fmt.Errorf("secrets/1password: list vaults: %w", err)
	}
	for _, v := range vaults {
		if strings.EqualFold(v.Title, name) {
			p.mu.Lock()
			p.cache[cacheKey] = v.ID
			p.mu.Unlock()
			return v.ID, nil
		}
	}
	return "", fmt.Errorf("secrets/1password: vault %q not found", name)
}

func (p *Provider) sdkItemUUID(ctx context.Context, hnd *sdkHandle, vaultID, title string) (string, error) {
	cacheKey := "item:" + vaultID + "/" + title
	p.mu.Lock()
	if id, ok := p.cache[cacheKey]; ok {
		p.mu.Unlock()
		return id, nil
	}
	p.mu.Unlock()

	items, err := hnd.items.List(ctx, vaultID)
	if err != nil {
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

// fieldsFromItem extracts all user-defined text fields from an SDK Item.
// Fields with an empty title are skipped.
func fieldsFromItem(itm op.Item) map[string]string {
	out := make(map[string]string, len(itm.Fields))
	for _, f := range itm.Fields {
		if f.Title != "" {
			out[f.Title] = f.Value
		}
	}
	return out
}

// ---- CLI fallback -----------------------------------------------------------

func (p *Provider) cliRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	if p.runFn != nil {
		return p.runFn(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if p.cfg.ServiceAccountToken != "" {
		cmd.Env = append(cmd.Environ(), "OP_SERVICE_ACCOUNT_TOKEN="+p.cfg.ServiceAccountToken)
	}
	return cmd.Output()
}

func (p *Provider) cliGet(ctx context.Context, app, env, provider, region, key string) (string, error) {
	ref := fmt.Sprintf("op://%s/%s/%s", p.vaultName(app), ItemTitle(env, provider, region), key)
	out, err := p.cliRun(ctx, "op", "read", ref)
	if err != nil {
		return "", fmt.Errorf("secrets/1password: op CLI failed to read key %q", key)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func (p *Provider) cliGetAll(ctx context.Context, app, env, provider, region string) (map[string]string, error) {
	vault := p.vaultName(app)
	item := ItemTitle(env, provider, region)
	out, err := p.cliRun(ctx, "op", "item", "get", item, "--vault", vault, "--format", "json")
	if err != nil {
		// Item not found or not authenticated — treat as empty bundle.
		return map[string]string{}, nil
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

// ---- Connect API (legacy) ---------------------------------------------------

func (p *Provider) connectGet(ctx context.Context, app, env, provider, region, key string) (string, error) {
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

func (p *Provider) connectGetAll(ctx context.Context, app, env, provider, region string) (map[string]string, error) {
	vUUID, err := p.connectVaultUUID(ctx, app)
	if err != nil {
		return nil, err
	}
	title := ItemTitle(env, provider, region)
	iUUID, err := p.connectItemUUID(ctx, vUUID, title)
	if err != nil {
		return nil, err
	}
	return p.connectItemFields(ctx, vUUID, iUUID)
}

func (p *Provider) doJSON(ctx context.Context, method, url string, out interface{}) error {
	client := p.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.ConnectToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
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

type connectVault struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type connectItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}
type connectField struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Value   string `json:"value"`
	Purpose string `json:"purpose"`
}
type connectItemDetail struct {
	Fields []connectField `json:"fields"`
}

func (p *Provider) connectVaultUUID(ctx context.Context, app string) (string, error) {
	name := p.vaultName(app)
	cacheKey := "connect-vault:" + name
	p.mu.Lock()
	if id, ok := p.cache[cacheKey]; ok {
		p.mu.Unlock()
		return id, nil
	}
	p.mu.Unlock()

	url := strings.TrimRight(p.cfg.ConnectHost, "/") + "/v1/vaults"
	var vaults []connectVault
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

func (p *Provider) connectItemUUID(ctx context.Context, vaultID, title string) (string, error) {
	cacheKey := "connect-item:" + vaultID + "/" + title
	p.mu.Lock()
	if id, ok := p.cache[cacheKey]; ok {
		p.mu.Unlock()
		return id, nil
	}
	p.mu.Unlock()

	url := fmt.Sprintf("%s/v1/vaults/%s/items", strings.TrimRight(p.cfg.ConnectHost, "/"), vaultID)
	var items []connectItem
	if err := p.doJSON(ctx, http.MethodGet, url, &items); err != nil {
		return "", fmt.Errorf("secrets/1password: list items: %w", err)
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

func (p *Provider) connectItemFields(ctx context.Context, vaultID, itemID string) (map[string]string, error) {
	url := fmt.Sprintf("%s/v1/vaults/%s/items/%s",
		strings.TrimRight(p.cfg.ConnectHost, "/"), vaultID, itemID)
	var detail connectItemDetail
	if err := p.doJSON(ctx, http.MethodGet, url, &detail); err != nil {
		return nil, fmt.Errorf("secrets/1password: get item %s: %w", itemID, err)
	}
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
	return out, nil
}
