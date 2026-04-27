package cmd

import (
	"os"

	"github.com/foundryfabric/clusterbox/internal/config"
	"github.com/foundryfabric/clusterbox/internal/provision/baremetal"
	"github.com/foundryfabric/clusterbox/internal/provision/k3d"
	"github.com/foundryfabric/clusterbox/internal/provision/qemu"
)

// resolveToken returns the credential value for the given infra key and
// environment variable name. Resolution order:
//  1. Env var (CI / explicit override) — returned if set.
//  2. Active context in ~/.clusterbox/config.yaml via 1Password or literal.
//  3. Empty string when neither is configured (backward-compatible fallback;
//     Pulumi/Hetzner will surface a meaningful auth error downstream).
//
// An error is returned only when a context IS configured but the credential
// lookup fails (e.g., 1Password read error).
func resolveToken(key, envVar string) (string, error) {
	// 1. Env var wins.
	if v := os.Getenv(envVar); v != "" {
		return v, nil
	}

	// 2. Config file / 1Password.
	cfg, err := config.Load()
	if err != nil {
		// Config file is optional; treat load failure as "not configured".
		return "", nil //nolint:nilerr
	}
	active, _, err := cfg.ActiveContext(globalContextOverride)
	if err != nil {
		// No active context — fall back to empty string (no-config path).
		return "", nil //nolint:nilerr
	}

	// 3. Attempt to resolve via the context. If the credential is not
	// configured in the context, ResolveInfra returns an error; surface it
	// so the user gets a helpful message.
	return active.ResolveInfra(key, envVar)
}

// isLocalProvider returns true for providers that run locally and do not
// require Hetzner / Pulumi / Tailscale credentials.
func isLocalProvider(name string) bool {
	return name == k3d.Name || name == baremetal.Name || name == qemu.Name
}
