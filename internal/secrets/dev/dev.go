// Package dev implements a flat-file secrets provider for local development.
// It reads deploy/config/dev.secrets.json which must be a flat JSON object:
//
//	{"KEY": "value", ...}
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
)

const DefaultPath = "deploy/config/dev.secrets.json"

// Provider reads secrets from a local JSON file.
type Provider struct {
	// Path is the filesystem path to the JSON secrets file.
	Path string

	// ReadFileFn is the I/O primitive used to read the file.
	// Tests inject a fake; production code uses os.ReadFile when nil.
	ReadFileFn func(name string) ([]byte, error)
}

// New returns a Provider that reads from path.
// When path is empty, DefaultPath is used.
func New(path string) *Provider {
	if path == "" {
		path = DefaultPath
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
				"run 'cp deploy/config/dev.secrets.example.json deploy/config/dev.secrets.json' to get started",
			p.Path, err,
		)
	}

	var out map[string]string
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("secrets/dev: parse %q: %w", p.Path, err)
	}

	return out, nil
}

// Get returns the value for key from the secrets file.
// ctx is accepted for interface conformance but is not used.
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
// ctx is accepted for interface conformance but is not used.
func (p *Provider) GetAll(_ context.Context) (map[string]string, error) {
	return p.load()
}
