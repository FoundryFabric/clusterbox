package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/provision/hetzner"
	"github.com/foundryfabric/clusterbox/internal/registry"
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
// out Pulumi / hcloud SDK calls without standing up a real provider
// dependency tree.
type providerOptions struct {
	// HetznerToken / PulumiToken come from the caller's environment
	// (or test-injected value). They feed the Hetzner provider.
	HetznerToken string
	PulumiToken  string

	// KubeconfigPath is the destination k3sup writes the cluster's
	// kubeconfig to. When empty the provider derives it from $HOME.
	KubeconfigPath string

	// K3sVersion is forwarded to the bootstrap step. Empty falls back
	// to bootstrap.DefaultK3sVersion inside the provider.
	K3sVersion string

	// HetznerOpenRegistry overrides the registry opener used inside
	// the Hetzner provider's Destroy / Reconcile path.
	HetznerOpenRegistry func(ctx context.Context) (registry.Registry, error)

	// HetznerPulumiDestroy overrides the Pulumi destroy path.
	HetznerPulumiDestroy func(ctx context.Context, clusterName, hetznerToken, pulumiToken string) error

	// HetznerNewLister overrides the hcloud lister constructor.
	HetznerNewLister func(token string) hetzner.HCloudResourceLister

	// HetznerDeleteResource overrides the direct-SDK delete path
	// used by the destroy sweep.
	HetznerDeleteResource func(ctx context.Context, token string, resourceType registry.HetznerResourceType, hetznerID string) error

	// HetznerOut overrides the human-readable output sink for the
	// Hetzner provider's progress lines.
	HetznerOut io.Writer
}

// providerRegistry is the canonical map of --provider value → factory.
// Tests overlay entries via withProviderRegistry to assert dispatch
// behaviour without depending on the real Hetzner wiring.
var providerRegistry = map[string]providerFactory{
	hetzner.Name: func(opts providerOptions) provision.Provider {
		return hetzner.New(hetzner.Deps{
			HetznerToken:   opts.HetznerToken,
			PulumiToken:    opts.PulumiToken,
			KubeconfigPath: opts.KubeconfigPath,
			K3sVersion:     opts.K3sVersion,
			Out:            opts.HetznerOut,
			PulumiDestroy:  opts.HetznerPulumiDestroy,
			NewLister:      opts.HetznerNewLister,
			DeleteResource: opts.HetznerDeleteResource,
			OpenRegistry:   opts.HetznerOpenRegistry,
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
