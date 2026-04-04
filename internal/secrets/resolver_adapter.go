package secrets

import "context"

// resolverFromProvider wraps a Provider so it satisfies the legacy Resolver
// interface. This allows the production deploy path to use NewProvider while
// existing code that accepts a Resolver (including test mocks) continues to work.
type resolverFromProvider struct {
	p      Provider
	app    string
	env    string
	pvdr   string
	region string
}

// NewResolverFromProvider wraps a Provider as a Resolver.
// The app/env/provider/region arguments are captured so that calls to
// Resolve (which pass the same values) are forwarded to Provider.GetAll.
func NewResolverFromProvider(p Provider) Resolver {
	return &resolverFromProvider{p: p}
}

func (a *resolverFromProvider) Resolve(ctx context.Context, app, env, provider, region string) (map[string]string, error) {
	return a.p.GetAll(ctx, SecretPath{
		App:      app,
		Env:      env,
		Provider: provider,
		Region:   region,
	})
}
