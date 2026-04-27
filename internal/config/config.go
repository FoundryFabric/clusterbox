package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the top-level ~/.clusterbox/config.yaml structure.
type Config struct {
	CurrentContext string              `yaml:"current_context"`
	Contexts       map[string]*Context `yaml:"contexts"`
}

// Context holds the configuration for a single named context.
type Context struct {
	SecretsBackend string      `yaml:"secrets_backend"` // onepassword (only supported value for now)
	Infra          InfraConfig `yaml:"infra"`
}

// InfraConfig holds the resolved paths (literal or op://) for infra credentials.
type InfraConfig struct {
	Hetzner               string `yaml:"hetzner,omitempty"`
	Pulumi                string `yaml:"pulumi,omitempty"`
	TailscaleClientID     string `yaml:"tailscale_client_id,omitempty"`
	TailscaleClientSecret string `yaml:"tailscale_client_secret,omitempty"`
}

// DefaultPath returns the path to the default config file: ~/.clusterbox/config.yaml.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".clusterbox", "config.yaml"), nil
}

// Load reads the config file from DefaultPath. If the file does not exist it
// returns an empty Config (not an error).
func Load() (*Config, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return &cfg, nil
}

// Save writes the config to DefaultPath with mode 0600. The directory is
// created with mode 0700 if it does not yet exist.
func (c *Config) Save() error {
	path, err := DefaultPath()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: create dir %s: %w", dir, err)
	}

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}

// ActiveContext returns the Context for the given override name (if non-empty)
// or for CurrentContext. It errors with a helpful message if no context is set
// or the named context is not found.
func (c *Config) ActiveContext(override string) (*Context, string, error) {
	name := override
	if name == "" {
		name = c.CurrentContext
	}
	if name == "" {
		return nil, "", fmt.Errorf("no context set — run `clusterbox login`")
	}
	ctx, ok := c.Contexts[name]
	if !ok {
		return nil, "", fmt.Errorf("context %q not found — run `clusterbox login`", name)
	}
	return ctx, name, nil
}

// ResolveInfra resolves a single infra credential identified by key
// ("hetzner", "pulumi", "tailscale_client_id", "tailscale_client_secret").
//
// Resolution order:
//  1. If envVar is set in the environment, return it (CI escape hatch).
//  2. Look up the configured path for key.
//  3. If path is empty, return a helpful error.
//  4. If path starts with "op://", exec `op read <path>` and return the result.
//  5. Otherwise return the path as a literal value.
func (ctx *Context) ResolveInfra(key, envVar string) (string, error) {
	// 1. Env var wins.
	if v := os.Getenv(envVar); v != "" {
		return v, nil
	}

	// 2. Look up the configured path.
	path := ctx.infraPath(key)

	// 3. Empty path.
	if path == "" {
		return "", fmt.Errorf("%s not configured — run `clusterbox login`", key)
	}

	// 4. 1Password reference.
	if strings.HasPrefix(path, "op://") {
		out, err := exec.Command("op", "read", path).Output() //nolint:gosec
		if err != nil {
			return "", fmt.Errorf("1Password read failed (is `op` signed in? run `clusterbox login`): %w", err)
		}
		return strings.TrimSpace(string(out)), nil
	}

	// 5. Literal value.
	return path, nil
}

// infraPath returns the configured credential path for the named key.
func (ctx *Context) infraPath(key string) string {
	switch key {
	case "hetzner":
		return ctx.Infra.Hetzner
	case "pulumi":
		return ctx.Infra.Pulumi
	case "tailscale_client_id":
		return ctx.Infra.TailscaleClientID
	case "tailscale_client_secret":
		return ctx.Infra.TailscaleClientSecret
	default:
		return ""
	}
}
