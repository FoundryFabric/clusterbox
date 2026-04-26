package baremetal

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/foundryfabric/clusterbox/internal/bootstrap"
	"github.com/foundryfabric/clusterbox/internal/node/config"
	"github.com/foundryfabric/clusterbox/internal/secrets"
)

// Name is the canonical provider identifier accepted by the `--provider`
// CLI flag.
const Name = "baremetal"

// DefaultSpec returns the install Spec used when the operator does not
// supply a --config file. It enables only the k3s section in
// server-init mode at bootstrap.DefaultK3sVersion.
//
// hostname is set from the cluster name so the section walker reports a
// stable identifier in its JSON output.
func DefaultSpec(clusterName, role string) *config.Spec {
	if role == "" || role == "control-plane" {
		role = "server-init"
	}
	return &config.Spec{
		Hostname: clusterName,
		K3s: &config.K3sSpec{
			Enabled: true,
			Role:    role,
			Version: bootstrap.DefaultK3sVersion,
		},
	}
}

// ResolveSecretsForSpec walks spec for fields whose values reference a
// secrets-backend key and returns an envOverlay map suitable for the
// Transport.Run envOverlay parameter.
//
// The current shape only handles Tailscale.AuthKeyEnv: when set and
// non-empty, the named key is resolved via the supplied Resolver and
// emitted as envOverlay[<name>]=<secret>. The secret value is never
// returned in error messages; callers must not log envOverlay.
//
// Resolver may be nil if no secrets are needed; in that case the spec
// must not declare any *_env fields or an error is returned.
func ResolveSecretsForSpec(ctx context.Context, spec *config.Spec, resolver secrets.Resolver, app, env, provider, region string) (map[string]string, error) {
	if spec == nil {
		return nil, errors.New("baremetal: ResolveSecretsForSpec: nil spec")
	}
	if spec.Tailscale == nil || !spec.Tailscale.Enabled || spec.Tailscale.AuthKeyEnv == "" {
		return nil, nil
	}
	envName := strings.TrimSpace(spec.Tailscale.AuthKeyEnv)
	if envName == "" {
		return nil, nil
	}
	if resolver == nil {
		return nil, fmt.Errorf("baremetal: spec references env %s but no secrets resolver configured", envName)
	}

	all, err := resolver.Resolve(ctx, app, env, provider, region)
	if err != nil {
		return nil, fmt.Errorf("baremetal: resolve secrets: %w", err)
	}
	val, ok := all[envName]
	if !ok || val == "" {
		// Deliberately omit the key value from any future error: only the
		// name is safe to log.
		return nil, fmt.Errorf("baremetal: secret %q not found in resolver output", envName)
	}
	return map[string]string{envName: val}, nil
}
