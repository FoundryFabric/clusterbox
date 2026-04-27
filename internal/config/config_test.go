package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/config"
)

// overrideDefaultPath points DefaultPath at a temp dir by setting HOME.
func withTempHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	return tmp
}

// ---- Load tests ----

func TestLoad_MissingFile_ReturnsEmptyConfig(t *testing.T) {
	withTempHome(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("expected no error for missing file, got %v", err)
	}
	if cfg.CurrentContext != "" {
		t.Errorf("expected empty CurrentContext, got %q", cfg.CurrentContext)
	}
	if len(cfg.Contexts) != 0 {
		t.Errorf("expected no contexts, got %d", len(cfg.Contexts))
	}
}

func TestLoad_ValidFile(t *testing.T) {
	home := withTempHome(t)

	cfgDir := filepath.Join(home, ".clusterbox")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(cfgDir, "config.yaml")

	content := `current_context: myprod
contexts:
  myprod:
    secrets_backend: onepassword
    infra:
      hetzner: "op://Prod/Hetzner/credential"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CurrentContext != "myprod" {
		t.Errorf("CurrentContext: want %q, got %q", "myprod", cfg.CurrentContext)
	}
	ctx, ok := cfg.Contexts["myprod"]
	if !ok {
		t.Fatal("context 'myprod' not found")
	}
	if ctx.SecretsBackend != "onepassword" {
		t.Errorf("SecretsBackend: want %q, got %q", "onepassword", ctx.SecretsBackend)
	}
	if ctx.Infra.Hetzner != "op://Prod/Hetzner/credential" {
		t.Errorf("Hetzner: want op path, got %q", ctx.Infra.Hetzner)
	}
}

func TestLoad_MalformedYAML(t *testing.T) {
	home := withTempHome(t)

	cfgDir := filepath.Join(home, ".clusterbox")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(cfgDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(":\tbad yaml{{{{"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := config.Load()
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
}

// ---- Save / round-trip test ----

func TestSave_RoundTrip(t *testing.T) {
	withTempHome(t)

	original := &config.Config{
		CurrentContext: "staging",
		Contexts: map[string]*config.Context{
			"staging": {
				SecretsBackend: "onepassword",
				Infra: config.InfraConfig{
					Hetzner: "op://Staging/Hetzner/credential",
				},
			},
		},
	}

	if err := original.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify file permissions.
	path, err := config.DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode: want 0600, got %04o", mode)
	}

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if loaded.CurrentContext != original.CurrentContext {
		t.Errorf("CurrentContext: want %q, got %q", original.CurrentContext, loaded.CurrentContext)
	}
	ctx, ok := loaded.Contexts["staging"]
	if !ok {
		t.Fatal("context 'staging' not found after reload")
	}
	if ctx.Infra.Hetzner != "op://Staging/Hetzner/credential" {
		t.Errorf("Hetzner: unexpected value %q", ctx.Infra.Hetzner)
	}
}

// ---- ActiveContext tests ----

func TestActiveContext_NoContextSet(t *testing.T) {
	cfg := &config.Config{}
	_, _, err := cfg.ActiveContext("")
	if err == nil {
		t.Fatal("expected error for no context, got nil")
	}
}

func TestActiveContext_Override(t *testing.T) {
	cfg := &config.Config{
		CurrentContext: "default",
		Contexts: map[string]*config.Context{
			"default": {SecretsBackend: "onepassword"},
			"prod":    {SecretsBackend: "onepassword", Infra: config.InfraConfig{Hetzner: "literal-token"}},
		},
	}
	ctx, name, err := cfg.ActiveContext("prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "prod" {
		t.Errorf("name: want %q, got %q", "prod", name)
	}
	if ctx.Infra.Hetzner != "literal-token" {
		t.Errorf("Hetzner: unexpected value %q", ctx.Infra.Hetzner)
	}
}

func TestActiveContext_CurrentContext(t *testing.T) {
	cfg := &config.Config{
		CurrentContext: "myctx",
		Contexts: map[string]*config.Context{
			"myctx": {SecretsBackend: "onepassword"},
		},
	}
	_, name, err := cfg.ActiveContext("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "myctx" {
		t.Errorf("name: want %q, got %q", "myctx", name)
	}
}

func TestActiveContext_NotFound(t *testing.T) {
	cfg := &config.Config{
		CurrentContext: "missing",
		Contexts:       map[string]*config.Context{},
	}
	_, _, err := cfg.ActiveContext("")
	if err == nil {
		t.Fatal("expected error for missing context, got nil")
	}
}

// ---- ResolveInfra tests ----

func TestResolveInfra_EnvVarWins(t *testing.T) {
	t.Setenv("HETZNER_API_TOKEN", "env-token-value")

	ctx := &config.Context{
		Infra: config.InfraConfig{Hetzner: "op://Vault/Item/field"},
	}
	val, err := ctx.ResolveInfra("hetzner", "HETZNER_API_TOKEN")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "env-token-value" {
		t.Errorf("want %q, got %q", "env-token-value", val)
	}
}

func TestResolveInfra_LiteralPath(t *testing.T) {
	// Ensure env var is not set.
	t.Setenv("HETZNER_API_TOKEN", "")

	ctx := &config.Context{
		Infra: config.InfraConfig{Hetzner: "my-literal-token"},
	}
	val, err := ctx.ResolveInfra("hetzner", "HETZNER_API_TOKEN")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "my-literal-token" {
		t.Errorf("want %q, got %q", "my-literal-token", val)
	}
}

func TestResolveInfra_EmptyPath_Error(t *testing.T) {
	t.Setenv("HETZNER_API_TOKEN", "")

	ctx := &config.Context{
		Infra: config.InfraConfig{},
	}
	_, err := ctx.ResolveInfra("hetzner", "HETZNER_API_TOKEN")
	if err == nil {
		t.Fatal("expected error for unconfigured key, got nil")
	}
}

func TestResolveInfra_UnknownKey_EmptyPath(t *testing.T) {
	t.Setenv("NO_SUCH_VAR", "")

	ctx := &config.Context{}
	_, err := ctx.ResolveInfra("no_such_key", "NO_SUCH_VAR")
	if err == nil {
		t.Fatal("expected error for unknown key, got nil")
	}
}

// TestResolveInfra_OpPrefix_NotCalledWithLiteral verifies that a path without
// the "op://" prefix is returned as-is (i.e. we do NOT exec `op` for literals).
// The real op:// path is exercised by the literal test above.
func TestResolveInfra_OpPrefix_NotCalledWithLiteral(t *testing.T) {
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "")

	ctx := &config.Context{
		Infra: config.InfraConfig{TailscaleClientID: "plaintext-ts-client-id"},
	}
	val, err := ctx.ResolveInfra("tailscale_client_id", "TAILSCALE_OAUTH_CLIENT_ID")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "plaintext-ts-client-id" {
		t.Errorf("want %q, got %q", "plaintext-ts-client-id", val)
	}
}
