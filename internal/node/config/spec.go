// Package config defines the YAML schema consumed by clusterboxnode.
//
// The configuration is provider-agnostic: a single Spec describes the host
// hardening, Tailscale enrolment, and k3s install/uninstall steps that the
// section walker performs in order.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Spec is the top-level configuration consumed by clusterboxnode.
//
// All sub-sections are pointer types so that an absent section is distinct
// from a present-but-disabled one. Sections that are nil are skipped by the
// section walker.
type Spec struct {
	Hostname  string         `yaml:"hostname,omitempty"`
	Harden    *HardenSpec    `yaml:"harden,omitempty"`
	Tailscale *TailscaleSpec `yaml:"tailscale,omitempty"`
	K3s       *K3sSpec       `yaml:"k3s,omitempty"`
}

// HardenSpec configures the host-hardening section.
type HardenSpec struct {
	Enabled   bool   `yaml:"enabled"`
	SSHPubKey string `yaml:"ssh_pub_key"`
	User      string `yaml:"user"`
	AllowICMP bool   `yaml:"allow_icmp"`
}

// TailscaleSpec configures Tailscale enrolment.
//
// Exactly one of AuthKey or AuthKeyEnv must be set when Enabled is true.
type TailscaleSpec struct {
	Enabled    bool   `yaml:"enabled"`
	AuthKeyEnv string `yaml:"auth_key_env,omitempty"`
	AuthKey    string `yaml:"auth_key,omitempty"`
	Hostname   string `yaml:"hostname,omitempty"`
}

// K3sSpec configures the k3s install/uninstall section.
type K3sSpec struct {
	Enabled bool   `yaml:"enabled"`
	Role    string `yaml:"role"`
	Version string `yaml:"version"`
}

// AllowedK3sRoles is the set of roles accepted by Validate.
//
// Exposed as a package-level variable so callers can format error messages or
// build documentation without re-deriving the list.
var AllowedK3sRoles = []string{"server", "agent", "server-init"}

// Load reads path, decodes it as YAML, and validates the result.
//
// YAML decode errors include the file path and the underlying error (which
// preserves line/column information from yaml.v3).
func Load(path string) (*Spec, error) {
	if path == "" {
		return nil, errors.New("config: path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var spec Spec
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("config: validate %s: %w", path, err)
	}
	return &spec, nil
}

// Validate enforces the per-section invariants:
//
//   - When Harden.Enabled is true, SSHPubKey and User must be non-empty.
//   - When Tailscale.Enabled is true, exactly one of AuthKey/AuthKeyEnv must
//     be set.
//   - When K3s.Enabled is true, Role must be one of AllowedK3sRoles and
//     Version must be non-empty.
//
// Unknown values are reported with the offending field name to help users
// locate problems in their YAML.
func (s *Spec) Validate() error {
	if s == nil {
		return errors.New("spec: nil")
	}
	if h := s.Harden; h != nil && h.Enabled {
		if h.SSHPubKey == "" {
			return errors.New("harden: ssh_pub_key is required when enabled")
		}
		if h.User == "" {
			return errors.New("harden: user is required when enabled")
		}
	}
	if t := s.Tailscale; t != nil && t.Enabled {
		hasKey := t.AuthKey != ""
		hasEnv := t.AuthKeyEnv != ""
		switch {
		case hasKey && hasEnv:
			return errors.New("tailscale: auth_key and auth_key_env are mutually exclusive")
		case !hasKey && !hasEnv:
			return errors.New("tailscale: one of auth_key or auth_key_env is required when enabled")
		}
	}
	if k := s.K3s; k != nil && k.Enabled {
		if !roleAllowed(k.Role) {
			return fmt.Errorf("k3s: role %q is not allowed (want one of %v)", k.Role, AllowedK3sRoles)
		}
		if k.Version == "" {
			return errors.New("k3s: version is required when enabled")
		}
	}
	return nil
}

func roleAllowed(role string) bool {
	for _, allowed := range AllowedK3sRoles {
		if role == allowed {
			return true
		}
	}
	return false
}
