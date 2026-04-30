package baremetal

import (
	"context"
	"errors"

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
			Enabled:       true,
			Role:          role,
			Version:       bootstrap.DefaultK3sVersion,
			DisableAddons: []string{"traefik"},
		},
	}
}

// ResolveSecretsForSpec returns an envOverlay map for the Transport.Run
// envOverlay parameter. Tailscale is now handled at the infrastructure layer
// (cloud-init), so no spec fields require secret resolution. The function is
// retained for API compatibility.
func ResolveSecretsForSpec(_ context.Context, spec *config.Spec, _ secrets.Resolver, _, _, _, _ string) (map[string]string, error) {
	if spec == nil {
		return nil, errors.New("baremetal: ResolveSecretsForSpec: nil spec")
	}
	return nil, nil
}
