package onepassword_test

import (
	"context"
	"fmt"
	"testing"

	op "github.com/1password/onepassword-sdk-go"
	"github.com/foundryfabric/clusterbox/internal/secrets/onepassword"
)

// ---- SDK mock types ----------------------------------------------------------

type mockSDKSecrets struct {
	fn func(ctx context.Context, ref string) (string, error)
}

func (m *mockSDKSecrets) Resolve(ctx context.Context, ref string) (string, error) {
	return m.fn(ctx, ref)
}

type mockSDKVaults struct {
	fn func(ctx context.Context) ([]op.VaultOverview, error)
}

func (m *mockSDKVaults) List(ctx context.Context, _ ...op.VaultListParams) ([]op.VaultOverview, error) {
	return m.fn(ctx)
}

type mockSDKItems struct {
	getFn  func(ctx context.Context, vaultID, itemID string) (op.Item, error)
	listFn func(ctx context.Context, vaultID string) ([]op.ItemOverview, error)
}

func (m *mockSDKItems) Get(ctx context.Context, vaultID, itemID string) (op.Item, error) {
	return m.getFn(ctx, vaultID, itemID)
}

func (m *mockSDKItems) List(ctx context.Context, vaultID string, _ ...op.ItemListFilter) ([]op.ItemOverview, error) {
	return m.listFn(ctx, vaultID)
}

// buildProvider returns a Provider wired to vault "dev-chris" with one item
// "k3d" containing the supplied fields.
func buildProvider(t *testing.T, fields []op.ItemField) *onepassword.Provider {
	t.Helper()
	cfg := onepassword.Config{ServiceAccountToken: "svc-token", Vault: "dev-chris"}
	vaults := &mockSDKVaults{fn: func(_ context.Context) ([]op.VaultOverview, error) {
		return []op.VaultOverview{{ID: "vault-1", Title: "dev-chris"}}, nil
	}}
	items := &mockSDKItems{
		listFn: func(_ context.Context, _ string) ([]op.ItemOverview, error) {
			return []op.ItemOverview{{ID: "item-1", Title: "k3d"}}, nil
		},
		getFn: func(_ context.Context, _, _ string) (op.Item, error) {
			return op.Item{ID: "item-1", Fields: fields}, nil
		},
	}
	secrets := &mockSDKSecrets{fn: func(_ context.Context, ref string) (string, error) {
		return "", fmt.Errorf("Resolve not expected in GetAll tests: %s", ref)
	}}
	return onepassword.NewWithSDKParts(cfg, secrets, items, vaults)
}

var sampleFields = []op.ItemField{
	{ID: "f1", Title: "GRAFANA_ADMIN_PASSWORD", Value: "grafana-secret"},
	{ID: "f2", Title: "DB_PASSWORD", Value: "db-secret"},
	{ID: "f3", Title: "", Value: "ignored-empty-title"},
}

// TestGetAll returns all non-empty-title fields.
func TestGetAll(t *testing.T) {
	p := buildProvider(t, sampleFields)

	got, err := p.GetAll(context.Background(), "k3d", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["GRAFANA_ADMIN_PASSWORD"] != "grafana-secret" {
		t.Errorf("GRAFANA_ADMIN_PASSWORD: want grafana-secret got %q", got["GRAFANA_ADMIN_PASSWORD"])
	}
	if got["DB_PASSWORD"] != "db-secret" {
		t.Errorf("DB_PASSWORD: want db-secret got %q", got["DB_PASSWORD"])
	}
	if _, ok := got[""]; ok {
		t.Error("empty-title field must be excluded")
	}
}

// TestGetAll_VaultNotFound returns empty map when vault is missing,
// so addons with only optional secrets install without a pre-created vault.
func TestGetAll_VaultNotFound(t *testing.T) {
	cfg := onepassword.Config{ServiceAccountToken: "svc-token", Vault: "dev-chris"}
	vaults := &mockSDKVaults{fn: func(_ context.Context) ([]op.VaultOverview, error) {
		return []op.VaultOverview{{ID: "v1", Title: "other-vault"}}, nil
	}}
	items := &mockSDKItems{
		listFn: func(_ context.Context, _ string) ([]op.ItemOverview, error) {
			t.Error("items.List should not be called when vault is missing")
			return nil, nil
		},
		getFn: func(_ context.Context, _, _ string) (op.Item, error) {
			t.Error("items.Get should not be called when vault is missing")
			return op.Item{}, nil
		},
	}
	secrets := &mockSDKSecrets{fn: func(_ context.Context, _ string) (string, error) {
		return "", fmt.Errorf("not used")
	}}
	p := onepassword.NewWithSDKParts(cfg, secrets, items, vaults)

	got, err := p.GetAll(context.Background(), "k3d", "")
	if err != nil {
		t.Fatalf("missing vault should return empty map, not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

// TestGetAll_ItemNotFound returns empty map when the cluster item is missing.
func TestGetAll_ItemNotFound(t *testing.T) {
	cfg := onepassword.Config{ServiceAccountToken: "svc-token", Vault: "dev-chris"}
	vaults := &mockSDKVaults{fn: func(_ context.Context) ([]op.VaultOverview, error) {
		return []op.VaultOverview{{ID: "vault-1", Title: "dev-chris"}}, nil
	}}
	items := &mockSDKItems{
		listFn: func(_ context.Context, _ string) ([]op.ItemOverview, error) {
			return []op.ItemOverview{{ID: "i1", Title: "hetzner-ash"}}, nil // k3d not present
		},
		getFn: func(_ context.Context, _, _ string) (op.Item, error) {
			t.Error("items.Get should not be called when item is missing")
			return op.Item{}, nil
		},
	}
	secrets := &mockSDKSecrets{fn: func(_ context.Context, _ string) (string, error) {
		return "", fmt.Errorf("not used")
	}}
	p := onepassword.NewWithSDKParts(cfg, secrets, items, vaults)

	got, err := p.GetAll(context.Background(), "k3d", "")
	if err != nil {
		t.Fatalf("missing item should return empty map, not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

// TestGet resolves a single field via Secrets().Resolve() with the correct ref.
func TestGet(t *testing.T) {
	var capturedRef string
	cfg := onepassword.Config{ServiceAccountToken: "svc-token", Vault: "dev-chris"}
	secrets := &mockSDKSecrets{fn: func(_ context.Context, ref string) (string, error) {
		capturedRef = ref
		return "mypassword", nil
	}}
	items := &mockSDKItems{
		getFn:  func(_ context.Context, _, _ string) (op.Item, error) { return op.Item{}, nil },
		listFn: func(_ context.Context, _ string) ([]op.ItemOverview, error) { return nil, nil },
	}
	vaults := &mockSDKVaults{fn: func(_ context.Context) ([]op.VaultOverview, error) { return nil, nil }}

	p := onepassword.NewWithSDKParts(cfg, secrets, items, vaults)

	val, err := p.Get(context.Background(), "hetzner", "ash", "MY_SECRET")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "mypassword" {
		t.Errorf("want mypassword got %q", val)
	}
	wantRef := "op://dev-chris/hetzner-ash/MY_SECRET"
	if capturedRef != wantRef {
		t.Errorf("op:// ref: want %q got %q", wantRef, capturedRef)
	}
}

// TestGet_k3d verifies empty region is omitted from the item title.
func TestGet_k3d(t *testing.T) {
	var capturedRef string
	cfg := onepassword.Config{ServiceAccountToken: "svc-token", Vault: "dev-chris"}
	secrets := &mockSDKSecrets{fn: func(_ context.Context, ref string) (string, error) {
		capturedRef = ref
		return "val", nil
	}}
	items := &mockSDKItems{
		getFn:  func(_ context.Context, _, _ string) (op.Item, error) { return op.Item{}, nil },
		listFn: func(_ context.Context, _ string) ([]op.ItemOverview, error) { return nil, nil },
	}
	vaults := &mockSDKVaults{fn: func(_ context.Context) ([]op.VaultOverview, error) { return nil, nil }}

	p := onepassword.NewWithSDKParts(cfg, secrets, items, vaults)

	if _, err := p.Get(context.Background(), "k3d", "", "MY_SECRET"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantRef := "op://dev-chris/k3d/MY_SECRET"
	if capturedRef != wantRef {
		t.Errorf("op:// ref: want %q got %q", wantRef, capturedRef)
	}
}

// TestCacheAvoidsDuplicateVaultListCalls verifies vault UUID is cached across calls.
func TestCacheAvoidsDuplicateVaultListCalls(t *testing.T) {
	vaultListCalls := 0
	cfg := onepassword.Config{ServiceAccountToken: "svc-token", Vault: "dev-chris"}
	vaults := &mockSDKVaults{fn: func(_ context.Context) ([]op.VaultOverview, error) {
		vaultListCalls++
		return []op.VaultOverview{{ID: "vault-1", Title: "dev-chris"}}, nil
	}}
	items := &mockSDKItems{
		listFn: func(_ context.Context, _ string) ([]op.ItemOverview, error) {
			return []op.ItemOverview{{ID: "item-1", Title: "k3d"}}, nil
		},
		getFn: func(_ context.Context, _, _ string) (op.Item, error) {
			return op.Item{Fields: []op.ItemField{{ID: "f1", Title: "KEY", Value: "val"}}}, nil
		},
	}
	secrets := &mockSDKSecrets{fn: func(_ context.Context, _ string) (string, error) {
		return "", fmt.Errorf("not used")
	}}

	p := onepassword.NewWithSDKParts(cfg, secrets, items, vaults)

	if _, err := p.GetAll(context.Background(), "k3d", ""); err != nil {
		t.Fatalf("first GetAll: %v", err)
	}
	if _, err := p.GetAll(context.Background(), "k3d", ""); err != nil {
		t.Fatalf("second GetAll: %v", err)
	}

	if vaultListCalls != 1 {
		t.Errorf("vault list should be called once, got %d", vaultListCalls)
	}
}

// TestItemTitle covers the provider/region joining behaviour.
func TestItemTitle(t *testing.T) {
	cases := []struct {
		provider, region string
		want             string
	}{
		{"k3d", "", "k3d"},
		{"hetzner", "ash", "hetzner-ash"},
		{"", "", ""},
		{"hetzner", "", "hetzner"},
	}
	for _, tc := range cases {
		got := onepassword.ItemTitle(tc.provider, tc.region)
		if got != tc.want {
			t.Errorf("ItemTitle(%q,%q) = %q, want %q", tc.provider, tc.region, got, tc.want)
		}
	}
}
