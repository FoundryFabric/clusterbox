// Package secrets provides secret resolution for clusterbox deployments.
//
// Two resolvers are available:
//
//   - DevResolver reads secrets from a local JSON file (deploy/config/dev.secrets.json).
//   - OPResolver fetches secrets from 1Password using the `op` CLI with paths of the
//     form op://<app>/<env>/<provider>/<region>/<key>.
//
// Usage pattern in a deploy workflow:
//
//  1. Select the appropriate Resolver based on the target environment.
//  2. Call Resolve to obtain all secrets for the deployment.
//  3. Use ApplySecrets to create/update a k8s Secret named <app>-secrets.
//  4. Only then apply the workload manifest.
package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// Resolver is the interface for loading deployment secrets.
// All implementations must guarantee that secret values are never written to
// any log or error message.
type Resolver interface {
	// Resolve returns a map of key → secret-value for the given deployment
	// target. Keys correspond to the entries in dev.secrets.json or the
	// leaf names of 1Password items.
	Resolve(ctx context.Context, app, env, provider, region string) (map[string]string, error)
}

// --- DevResolver ---

// DevResolver reads secrets from a local JSON file.
// The file path defaults to "deploy/config/dev.secrets.json" relative to the
// working directory; set Path explicitly to override.
type DevResolver struct {
	// Path is the filesystem path to the JSON secrets file.
	// When empty the default path "deploy/config/dev.secrets.json" is used.
	Path string

	// ReadFileFn is the I/O primitive used to read the file.
	// Tests inject a fake; production code uses os.ReadFile when nil.
	ReadFileFn func(name string) ([]byte, error)
}

// NewDevResolver returns a DevResolver that reads from the default path.
func NewDevResolver() *DevResolver {
	return &DevResolver{}
}

// Resolve reads the secrets file and returns its contents.
// The app/env/provider/region parameters are accepted for interface conformance
// but are not used; all secrets for the dev environment live in a single file.
func (r *DevResolver) Resolve(_ context.Context, _, _, _, _ string) (map[string]string, error) {
	path := r.Path
	if path == "" {
		path = "deploy/config/dev.secrets.json"
	}

	readFn := r.ReadFileFn
	if readFn == nil {
		readFn = os.ReadFile
	}

	data, err := readFn(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: dev resolver: read %q: %w", path, err)
	}

	var out map[string]string
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("secrets: dev resolver: parse %q: %w", path, err)
	}

	return out, nil
}

// --- OPResolver ---

// OPResolver fetches secrets from 1Password using the `op` CLI.
// Each key is retrieved via a path of the form:
//
//	op://<app>/<env>/<provider>/<region>/<key>
//
// The secret value is never included in any error message or log line.
type OPResolver struct {
	// Keys is the ordered list of secret key names to fetch.
	// The caller is responsible for providing the correct set of keys for the
	// target deployment.
	Keys []string

	// runFn is the command executor used to invoke `op`.
	// Tests inject a fake; production code uses the real os/exec.
	runFn func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// NewOPResolver returns an OPResolver that will fetch the supplied keys using
// the real `op` CLI.
func NewOPResolver(keys []string) *OPResolver {
	return &OPResolver{Keys: keys}
}

// NewOPResolverWithRunner returns an OPResolver that will fetch the supplied
// keys using the provided runner function. This is the preferred constructor
// for tests.
func NewOPResolverWithRunner(keys []string, run func(ctx context.Context, name string, args ...string) ([]byte, error)) *OPResolver {
	return &OPResolver{Keys: keys, runFn: run}
}

// Resolve fetches each key from 1Password and returns the results as a map.
// Secret values are never included in error messages.
func (r *OPResolver) Resolve(ctx context.Context, app, env, provider, region string) (map[string]string, error) {
	runFn := r.runFn
	if runFn == nil {
		runFn = execRun
	}

	out := make(map[string]string, len(r.Keys))
	for _, key := range r.Keys {
		path := fmt.Sprintf("op://%s/%s/%s/%s/%s", app, env, provider, region, key)
		val, err := runFn(ctx, "op", "read", path)
		if err != nil {
			// Deliberately omit the secret value and path detail from the
			// user-visible error to avoid leaking credentials into logs.
			return nil, fmt.Errorf("secrets: op resolver: failed to read key %q: op exited non-zero", key)
		}
		out[key] = string(bytes.TrimRight(val, "\n"))
	}

	return out, nil
}

// execRun is the production CommandRunner that shells out via os/exec.
func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}
