package secrets

import "context"

// SecretPath identifies a single secret within the clusterbox path convention.
//
// The logical hierarchy is:
//
//	app / env / provider / region / key
//
// Each backend maps this to its own storage layout:
//   - dev:         flat JSON file (key only)
//   - onepassword: op://<App>/<Env>-<Provider>-<Region>/<Key>
//   - vault:       secret/data/<App>/<Env>/<Provider>/<Region> (key is a field)
type SecretPath struct {
	App      string
	Env      string
	Provider string
	Region   string
	Key      string
}

// Provider is the interface for loading deployment secrets.
// All implementations must guarantee that secret values are never written to
// any log or error message.
type Provider interface {
	// Get returns a single secret value identified by path.
	Get(ctx context.Context, path SecretPath) (string, error)

	// GetAll returns all secrets at the given prefix (same App/Env/Provider/Region).
	// The Key field of prefix is ignored.
	GetAll(ctx context.Context, prefix SecretPath) (map[string]string, error)
}
