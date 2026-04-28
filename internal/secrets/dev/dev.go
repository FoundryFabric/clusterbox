// Package dev implements a flat-file secrets provider for local development.
// It reads ~/.clusterbox/dev.secrets.json (or CLUSTERBOX_DEV_SECRETS) which
// must be a flat JSON object:
//
//	{"KEY": "value", ...}
//
// Values may be 1Password references (op://vault/item/field). They are
// resolved via the `op` CLI so secrets never have to be stored in plain text:
//
//	{"GH_PAT_TOKEN": "op://Infra/GithubActions/GH_PAT_TOKEN"}
//
// This package deliberately has zero external dependencies and does not import
// the root secrets package to avoid import cycles. The root package wraps this
// via an adapter.
package dev

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultPath returns the default secrets file path: ~/.clusterbox/dev.secrets.json.
// Override with the CLUSTERBOX_DEV_SECRETS environment variable.
func DefaultPath() string {
	if v := os.Getenv("CLUSTERBOX_DEV_SECRETS"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "~/.clusterbox/dev.secrets.json"
	}
	return filepath.Join(home, ".clusterbox", "dev.secrets.json")
}

// Provider reads secrets from a local JSON file.
type Provider struct {
	// Path is the filesystem path to the JSON secrets file.
	Path string

	// ReadFileFn is the I/O primitive used to read the file.
	// Tests inject a fake; production code uses os.ReadFile when nil.
	ReadFileFn func(name string) ([]byte, error)

	// RunOpFn resolves an op:// reference. Tests inject a fake;
	// production code shells out to `op read` when nil.
	RunOpFn func(ref string) (string, error)
}

// New returns a Provider that reads from path.
// When path is empty, DefaultPath() is used.
func New(path string) *Provider {
	if path == "" {
		path = DefaultPath()
	}
	return &Provider{Path: path}
}

// NewWithReader returns a Provider with an injected read function (for tests).
func NewWithReader(path string, fn func(string) ([]byte, error)) *Provider {
	p := New(path)
	p.ReadFileFn = fn
	return p
}

func (p *Provider) load() (map[string]string, error) {
	readFn := p.ReadFileFn
	if readFn == nil {
		readFn = os.ReadFile
	}

	data, err := readFn(p.Path)
	if err != nil {
		return nil, fmt.Errorf(
			"secrets/dev: read %q: %w — "+
				"create ~/.clusterbox/dev.secrets.json (or set CLUSTERBOX_DEV_SECRETS); "+
				`values can be op:// references, e.g. {"GH_PAT_TOKEN": "op://Infra/GithubActions/GH_PAT_TOKEN"}`,
			p.Path, err,
		)
	}

	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("secrets/dev: parse %q: %w", p.Path, err)
	}

	return p.resolveOPRefs(raw)
}

// resolveOPRefs replaces any op:// values in m by calling `op read`.
func (p *Provider) resolveOPRefs(m map[string]string) (map[string]string, error) {
	runOp := p.RunOpFn
	if runOp == nil {
		runOp = opRead
	}

	out := make(map[string]string, len(m))
	for k, v := range m {
		if !strings.HasPrefix(v, "op://") {
			out[k] = v
			continue
		}
		resolved, err := runOp(v)
		if err != nil {
			return nil, fmt.Errorf("secrets/dev: resolve op:// ref for key %q: %w (is `op` signed in?)", k, err)
		}
		out[k] = resolved
	}
	return out, nil
}

// opRead shells out to `op read <ref>` and returns the trimmed result.
func opRead(ref string) (string, error) {
	out, err := exec.Command("op", "read", ref).Output() //nolint:gosec
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Get returns the value for key from the secrets file.
func (p *Provider) Get(_ context.Context, key string) (string, error) {
	m, err := p.load()
	if err != nil {
		return "", err
	}

	val, ok := m[key]
	if !ok {
		return "", fmt.Errorf("secrets/dev: key %q not found in %s", key, p.Path)
	}

	return val, nil
}

// GetAll returns all key-value pairs from the secrets file.
func (p *Provider) GetAll(_ context.Context) (map[string]string, error) {
	return p.load()
}
