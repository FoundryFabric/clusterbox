package cmd

import (
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/foundryfabric/clusterbox/internal/config"
	"github.com/spf13/cobra"
)

var contextCmd = &cobra.Command{
	Use:   "context",
	Short: "Show or manage named contexts",
	Long: `Show the active context and list all configured contexts.

A context holds the 1Password paths for infrastructure credentials
(Hetzner, Pulumi, Tailscale) so commands like "clusterbox up" and
"clusterbox destroy" work without setting environment variables.

Use "clusterbox login" to create a context and "clusterbox use-context"
to switch between them.`,
	RunE: runContextShow,
}

var contextListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all configured contexts",
	RunE:  runContextList,
}

func init() {
	contextCmd.AddCommand(contextListCmd)
}

// runContextShow prints the active context name and its infra paths.
func runContextShow(_ *cobra.Command, _ []string) error {
	return RunContextShowWith(config.Load, os.Stdout)
}

// RunContextShowWith is the injectable variant used by tests.
func RunContextShowWith(loadFn func() (*config.Config, error), out io.Writer) error {
	cfg, err := loadFn()
	if err != nil {
		return fmt.Errorf("context: load config: %w", err)
	}

	active, name, err := cfg.ActiveContext(globalContextOverride)
	if err != nil {
		_, _ = fmt.Fprintln(out, "No active context — run `clusterbox login` to create one.")
		return nil
	}

	_, _ = fmt.Fprintf(out, "Current context: %s\n", name)
	_, _ = fmt.Fprintf(out, "  backend:                %s\n", active.SecretsBackend)
	if active.Infra.Hetzner != "" {
		_, _ = fmt.Fprintf(out, "  hetzner:                %s\n", active.Infra.Hetzner)
	}
	if active.Infra.Pulumi != "" {
		_, _ = fmt.Fprintf(out, "  pulumi:                 %s\n", active.Infra.Pulumi)
	}
	if active.Infra.TailscaleClientID != "" {
		_, _ = fmt.Fprintf(out, "  tailscale_client_id:    %s\n", active.Infra.TailscaleClientID)
	}
	if active.Infra.TailscaleClientSecret != "" {
		_, _ = fmt.Fprintf(out, "  tailscale_client_secret: %s\n", active.Infra.TailscaleClientSecret)
	}
	return nil
}

// runContextList prints all configured contexts with * marking the active one.
func runContextList(_ *cobra.Command, _ []string) error {
	return RunContextListWith(config.Load, os.Stdout)
}

// RunContextListWith is the injectable variant used by tests.
func RunContextListWith(loadFn func() (*config.Config, error), out io.Writer) error {
	cfg, err := loadFn()
	if err != nil {
		return fmt.Errorf("context list: load config: %w", err)
	}

	if len(cfg.Contexts) == 0 {
		_, _ = fmt.Fprintln(out, "No contexts configured — run `clusterbox login` to create one.")
		return nil
	}

	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)

	active := cfg.CurrentContext
	if globalContextOverride != "" {
		active = globalContextOverride
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, name := range names {
		marker := " "
		if name == active {
			marker = "*"
		}
		ctx := cfg.Contexts[name]
		_, _ = fmt.Fprintf(tw, "%s\t%s\t(%s)\n", marker, name, ctx.SecretsBackend)
	}
	return tw.Flush()
}
