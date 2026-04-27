package cmd

import (
	"fmt"
	"os"

	"github.com/foundryfabric/clusterbox/internal/config"
	"github.com/spf13/cobra"
)

// LoginFlags holds all CLI flags for the login command.
// Exported so tests can construct values without going through cobra.
type LoginFlags struct {
	ContextName           string
	Hetzner               string
	Pulumi                string
	TailscaleClientID     string
	TailscaleClientSecret string
	Activate              bool
}

var loginF LoginFlags

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Configure a named context with infra credentials",
	Long: `Create or update a named context in ~/.clusterbox/config.yaml.

Credentials are stored as 1Password references (op://<vault>/<item>/<field>)
so secrets never touch disk directly.

Example:
  clusterbox login \
    --context foundryfabric \
    --hetzner "op://FoundryFabric/Hetzner/credential" \
    --pulumi "op://FoundryFabric/Pulumi/access-token" \
    --tailscale-client-id "op://FoundryFabric/Tailscale/client-id" \
    --tailscale-client-secret "op://FoundryFabric/Tailscale/client-secret"`,
	RunE: runLogin,
}

func init() {
	loginCmd.Flags().StringVar(&loginF.ContextName, "context", "default", "Context name")
	loginCmd.Flags().StringVar(&loginF.Hetzner, "hetzner", "", "1Password path for Hetzner API token (e.g. op://FoundryFabric/Hetzner/credential)")
	loginCmd.Flags().StringVar(&loginF.Pulumi, "pulumi", "", "1Password path for Pulumi access token")
	loginCmd.Flags().StringVar(&loginF.TailscaleClientID, "tailscale-client-id", "", "1Password path for Tailscale OAuth client ID")
	loginCmd.Flags().StringVar(&loginF.TailscaleClientSecret, "tailscale-client-secret", "", "1Password path for Tailscale OAuth client secret")
	loginCmd.Flags().BoolVar(&loginF.Activate, "activate", true, "Set as current_context after saving")
}

// runLogin is the cobra RunE handler for `clusterbox login`.
func runLogin(cmd *cobra.Command, _ []string) error {
	return RunLoginWith(loginF, config.Load, func(cfg *config.Config) error { return cfg.Save() }, cmd)
}

// RunLoginWith is the injectable variant used by tests.
func RunLoginWith(
	flags LoginFlags,
	loadFn func() (*config.Config, error),
	saveFn func(*config.Config) error,
	cmd *cobra.Command,
) error {
	cfg, err := loadFn()
	if err != nil {
		return fmt.Errorf("login: load config: %w", err)
	}

	// Check whether any infra flags were provided.
	hasFlags := flags.Hetzner != "" || flags.Pulumi != "" ||
		flags.TailscaleClientID != "" || flags.TailscaleClientSecret != ""

	// If no infra flags and no existing context: print helpful usage.
	if !hasFlags {
		existing := cfg.Contexts[flags.ContextName]
		if existing == nil {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), `No credentials provided. To configure a context, run:

  clusterbox login \
    --context foundryfabric \
    --hetzner "op://FoundryFabric/Hetzner/credential" \
    --pulumi "op://FoundryFabric/Pulumi/access-token" \
    --tailscale-client-id "op://FoundryFabric/Tailscale/client-id" \
    --tailscale-client-secret "op://FoundryFabric/Tailscale/client-secret"`)
			return nil
		}
	}

	// Build or update the context.
	if cfg.Contexts == nil {
		cfg.Contexts = make(map[string]*config.Context)
	}
	ctx := cfg.Contexts[flags.ContextName]
	if ctx == nil {
		ctx = &config.Context{SecretsBackend: "onepassword"}
	}

	if flags.Hetzner != "" {
		ctx.Infra.Hetzner = flags.Hetzner
	}
	if flags.Pulumi != "" {
		ctx.Infra.Pulumi = flags.Pulumi
	}
	if flags.TailscaleClientID != "" {
		ctx.Infra.TailscaleClientID = flags.TailscaleClientID
	}
	if flags.TailscaleClientSecret != "" {
		ctx.Infra.TailscaleClientSecret = flags.TailscaleClientSecret
	}

	cfg.Contexts[flags.ContextName] = ctx

	if flags.Activate {
		cfg.CurrentContext = flags.ContextName
	}

	if err := saveFn(cfg); err != nil {
		return fmt.Errorf("login: save config: %w", err)
	}

	_, _ = fmt.Fprintf(os.Stderr, "Context %q saved. Run clusterbox commands without setting env vars.\n", flags.ContextName)
	return nil
}
