// Package onepassword implements a secrets provider backed by 1Password.
//
// The provider uses the official 1Password Go SDK (github.com/1password/onepassword-sdk-go).
// A service account token is required:
//
//	OP_SERVICE_ACCOUNT_TOKEN=<token>
//
// Vault convention:
//
//	One vault per environment, named for the developer or CI role:
//	  dev-chris, dev-alice, staging, prod
//
//	OP_VAULT=dev-chris   — which vault to look in (required)
//
//	Items are named <provider>[-<region>] within the vault:
//	  k3d, hetzner-ash
//
//	Fields are addon secret keys:
//	  GRAFANA_ADMIN_PASSWORD, CLICKHOUSE_ADMIN_PASSWORD, …
//
// Vault and item UUIDs are cached per Provider instance to avoid redundant API calls.
package onepassword

import (
	"context"
	"fmt"
	"strings"
	"sync"

	op "github.com/1password/onepassword-sdk-go"
)

// Config holds credentials for the 1Password SDK.
type Config struct {
	// ServiceAccountToken is the 1Password service account token.
	// Populated from OP_SERVICE_ACCOUNT_TOKEN.
	ServiceAccountToken string

	// Vault pins the vault name for all lookups.
	// Populated from OP_VAULT.
	// Convention: dev-<name> for personal dev vaults, staging, prod for CI.
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

// ---- narrow interfaces (keeps test mocks slim) ------------------------------

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

type sdkHandle struct {
	secrets sdkSecretsAPI
	items   sdkItemsAPI
	vaults  sdkVaultsAPI
}

// ---- Provider ---------------------------------------------------------------

// Provider is the 1Password secrets provider.
type Provider struct {
	cfg Config

	// SDK client — lazily initialised on first use.
	sdkOnce sync.Once
	sdkHnd  *sdkHandle
	sdkErr  error

	// injectSDK bypasses NewClient for tests.
	injectSDK *sdkHandle

	// UUID cache.
	mu    sync.Mutex
	cache map[string]string
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
			p.sdkErr = fmt.Errorf("secrets/1password: init SDK: %w", err)
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

// Get returns a single secret field.
// It resolves the op:// reference directly via Secrets().Resolve().
func (p *Provider) Get(ctx context.Context, provider, region, key string) (string, error) {
	hnd, err := p.getSDK(ctx)
	if err != nil {
		return "", err
	}
	ref := fmt.Sprintf("op://%s/%s/%s", p.cfg.Vault, ItemTitle(provider, region), key)
	val, err := hnd.secrets.Resolve(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("secrets/1password: key %q not found", key)
	}
	return val, nil
}

// GetAll returns all user-defined fields for the given cluster coordinates.
// A missing vault or item is treated as empty (not an error) so addons with
// only optional secrets install cleanly on clusters with no item configured.
func (p *Provider) GetAll(ctx context.Context, provider, region string) (map[string]string, error) {
	hnd, err := p.getSDK(ctx)
	if err != nil {
		return nil, err
	}

	vaultID, err := p.vaultUUID(ctx, hnd)
	if err != nil {
		return map[string]string{}, nil
	}
	itemID, err := p.itemUUID(ctx, hnd, vaultID, ItemTitle(provider, region))
	if err != nil {
		return map[string]string{}, nil
	}

	itm, err := hnd.items.Get(ctx, vaultID, itemID)
	if err != nil {
		return nil, fmt.Errorf("secrets/1password: get item %s/%s: %w",
			p.cfg.Vault, ItemTitle(provider, region), err)
	}
	return fieldsFromItem(itm), nil
}

func (p *Provider) vaultUUID(ctx context.Context, hnd *sdkHandle) (string, error) {
	cacheKey := "vault:" + p.cfg.Vault
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
		if strings.EqualFold(v.Title, p.cfg.Vault) {
			p.mu.Lock()
			p.cache[cacheKey] = v.ID
			p.mu.Unlock()
			return v.ID, nil
		}
	}
	return "", fmt.Errorf("secrets/1password: vault %q not found", p.cfg.Vault)
}

func (p *Provider) itemUUID(ctx context.Context, hnd *sdkHandle, vaultID, title string) (string, error) {
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

// fieldsFromItem extracts all user-defined fields from an SDK Item.
func fieldsFromItem(itm op.Item) map[string]string {
	out := make(map[string]string, len(itm.Fields))
	for _, f := range itm.Fields {
		if f.Title != "" {
			out[f.Title] = f.Value
		}
	}
	return out
}
