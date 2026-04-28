package cmd

import (
	"fmt"

	"github.com/foundryfabric/clusterbox/internal/config"
)

// resolveCluster returns the cluster name from flagValue, or falls back to
// the active context's default Cluster when flagValue is empty.
// Returns an error with a helpful message when neither is set.
func resolveCluster(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return "", fmt.Errorf("--cluster not set and config could not be loaded: %w", err)
	}
	active, _, err := cfg.ActiveContext(globalContextOverride)
	if err != nil {
		return "", fmt.Errorf("--cluster not set and no active context — use --cluster or run `clusterbox login --cluster <name>`")
	}
	if active.Cluster == "" {
		return "", fmt.Errorf("--cluster not set and context has no default cluster — use --cluster or run `clusterbox login --cluster <name>`")
	}
	return active.Cluster, nil
}
