package sqlite

import "github.com/foundryfabric/clusterbox/internal/registry"

// init registers this package's New constructor with the registry factory
// so that registry.NewRegistry can construct a sqlite-backed registry
// when REGISTRY_BACKEND is "sqlite" (or unset, the default).
//
// Importing this package — directly or via blank import — is enough to
// enable the sqlite backend.
func init() {
	registry.Register("sqlite", func(path string) (registry.Registry, error) {
		return New(path)
	})
}
