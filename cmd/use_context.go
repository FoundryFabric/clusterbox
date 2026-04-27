package cmd

import (
	"fmt"
	"os"

	"github.com/foundryfabric/clusterbox/internal/config"
	"github.com/spf13/cobra"
)

var useContextCmd = &cobra.Command{
	Use:   "use-context <name>",
	Short: "Set the active context",
	Long:  `Set the current_context in ~/.clusterbox/config.yaml.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runUseContext,
}

// runUseContext is the cobra RunE handler for `clusterbox use-context`.
func runUseContext(_ *cobra.Command, args []string) error {
	return RunUseContextWith(args[0], config.Load, func(cfg *config.Config) error { return cfg.Save() })
}

// RunUseContextWith is the injectable variant used by tests.
func RunUseContextWith(
	name string,
	loadFn func() (*config.Config, error),
	saveFn func(*config.Config) error,
) error {
	cfg, err := loadFn()
	if err != nil {
		return fmt.Errorf("use-context: load config: %w", err)
	}

	if cfg.Contexts == nil || cfg.Contexts[name] == nil {
		return fmt.Errorf("use-context: context %q not found — run `clusterbox login --context %s`", name, name)
	}

	cfg.CurrentContext = name

	if err := saveFn(cfg); err != nil {
		return fmt.Errorf("use-context: save config: %w", err)
	}

	_, _ = fmt.Fprintf(os.Stderr, "Switched to context %q.\n", name)
	return nil
}
