package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/provision/baremetal"
	"github.com/foundryfabric/clusterbox/internal/provision/hetzner"
	"github.com/foundryfabric/clusterbox/internal/provision/k3d"
	"github.com/foundryfabric/clusterbox/internal/provision/qemu"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/secrets"
)

// providerFactory builds a fresh provision.Provider from the cmd-side
// flag inputs. Each --provider value maps to exactly one factory; the
// factory hides the per-provider construction details (env-var lookups,
// default deps) from the cobra layer.
type providerFactory func(opts providerOptions) provision.Provider

// providerOptions carries the cmd-side knobs every provider may need.
// Fields that don't apply to a particular provider are ignored by that
// provider's factory.
//
// Hetzner-specific test hooks travel as Hetzner* fields. Production
// callers leave them nil; the destroy_test suite uses them to stub
// out hcloud SDK calls without standing up a real provider
// dependency tree.
type providerOptions struct {
	// HetznerToken comes from the caller's environment (or test-injected
	// value). It feeds the Hetzner provider.
	HetznerToken string

	// TailscaleClientID and TailscaleClientSecret are passed to the Hetzner
	// provider so it can remove the Tailscale device on destroy.
	TailscaleClientID     string
	TailscaleClientSecret string

	// KubeconfigPath is the destination k3sup writes the cluster's
	// kubeconfig to. When empty the provider derives it from $HOME.
	KubeconfigPath string

	// K3sVersion is forwarded to the bootstrap step. Empty falls back
	// to bootstrap.DefaultK3sVersion inside the provider.
	K3sVersion string

	// HetznerOpenRegistry overrides the registry opener used inside
	// the Hetzner provider's Destroy / Reconcile path.
	HetznerOpenRegistry func(ctx context.Context) (registry.Registry, error)

	// HetznerNewLister overrides the hcloud lister constructor.
	HetznerNewLister func(token string) hetzner.HCloudResourceLister

	// HetznerDeleteResource overrides the direct-SDK delete path
	// used by the destroy sweep.
	HetznerDeleteResource func(ctx context.Context, token string, resourceType registry.HetznerResourceType, hetznerID string) error

	// HetznerOut overrides the human-readable output sink for the
	// Hetzner provider's progress lines.
	HetznerOut io.Writer

	// Baremetal* fields configure the bare-metal provider. They are
	// only consulted when --provider=baremetal.
	BaremetalHost       string
	BaremetalUser       string
	BaremetalSSHKeyPath string
	BaremetalConfigPath string
	// BaremetalSecretsResolver is consulted to resolve any *_env
	// references in the install Spec. cmd/up wires this from the
	// process's chosen resolver (DevResolver / OPResolver).
	BaremetalSecretsResolver secrets.Resolver
	// BaremetalAgentVersion is forwarded to the provider so it can
	// stamp the registry rows once the T10 schema lands.
	BaremetalAgentVersion string
	// BaremetalOpenRegistry overrides the registry opener used by the
	// provider's best-effort registry write.
	BaremetalOpenRegistry func(ctx context.Context) (registry.Registry, error)
	// BaremetalOut overrides the progress-line writer.
	BaremetalOut io.Writer

	// K3dNodes is the total node count for the k3d cluster (1 server +
	// N-1 agents). Zero and one both produce a single-server cluster.
	K3dNodes int

	// QEMUSSHKeyPath is the path to the SSH private key for the QEMU provider.
	// Defaults to ~/.ssh/id_ed25519 when empty.
	QEMUSSHKeyPath string
}

// providerRegistry is the canonical map of --provider value → factory.
// Tests overlay entries via withProviderRegistry to assert dispatch
// behaviour without depending on the real Hetzner wiring.
var providerRegistry = map[string]providerFactory{
	hetzner.Name: func(opts providerOptions) provision.Provider {
		return hetzner.New(hetzner.Deps{
			HetznerToken:          opts.HetznerToken,
			TailscaleClientID:     opts.TailscaleClientID,
			TailscaleClientSecret: opts.TailscaleClientSecret,
			KubeconfigPath:        opts.KubeconfigPath,
			K3sVersion:            opts.K3sVersion,
			Out:                   opts.HetznerOut,
			NewLister:             opts.HetznerNewLister,
			DeleteResource:        opts.HetznerDeleteResource,
			OpenRegistry:          opts.HetznerOpenRegistry,
		})
	},
	baremetal.Name: func(opts providerOptions) provision.Provider {
		return baremetal.New(baremetal.Deps{
			Host:            opts.BaremetalHost,
			User:            opts.BaremetalUser,
			SSHKeyPath:      opts.BaremetalSSHKeyPath,
			ConfigPath:      opts.BaremetalConfigPath,
			KubeconfigPath:  opts.KubeconfigPath,
			AgentVersion:    opts.BaremetalAgentVersion,
			SecretsResolver: opts.BaremetalSecretsResolver,
			OpenRegistry:    opts.BaremetalOpenRegistry,
			Out:             opts.BaremetalOut,
		})
	},
	k3d.Name: func(opts providerOptions) provision.Provider {
		return k3d.New(k3d.Deps{
			Nodes:          opts.K3dNodes,
			K3sVersion:     opts.K3sVersion,
			KubeconfigPath: opts.KubeconfigPath,
		})
	},
	qemu.Name: func(opts providerOptions) provision.Provider {
		return qemu.New(qemu.Deps{
			KubeconfigPath: opts.KubeconfigPath,
			SSHKeyPath:     opts.QEMUSSHKeyPath,
		})
	},
}

// resolveProvider looks up the requested provider in providerRegistry
// (or testRegistry when non-nil) and returns a constructed instance.
// An unknown provider yields a descriptive error listing the
// registered names so the operator can self-correct.
func resolveProvider(name string, opts providerOptions, testRegistry map[string]providerFactory) (provision.Provider, error) {
	registry := providerRegistry
	if testRegistry != nil {
		registry = testRegistry
	}
	factory, ok := registry[name]
	if !ok {
		known := make([]string, 0, len(registry))
		for k := range registry {
			known = append(known, k)
		}
		sort.Strings(known)
		return nil, fmt.Errorf("unknown provider %q (known: %v)", name, known)
	}
	return factory(opts), nil
}
