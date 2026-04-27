package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_Full(t *testing.T) {
	spec, err := Load(filepath.Join("testdata", "full.yaml"))
	if err != nil {
		t.Fatalf("Load full.yaml: %v", err)
	}
	if spec.Hostname != "node-01" {
		t.Errorf("hostname = %q, want node-01", spec.Hostname)
	}
	if spec.Harden == nil || !spec.Harden.Enabled {
		t.Errorf("harden missing or disabled: %+v", spec.Harden)
	}
	if spec.Tailscale == nil || spec.Tailscale.AuthKeyEnv != "TS_AUTHKEY" {
		t.Errorf("tailscale auth_key_env unexpected: %+v", spec.Tailscale)
	}
	if spec.K3s == nil || spec.K3s.Role != "server-init" {
		t.Errorf("k3s role unexpected: %+v", spec.K3s)
	}
}

func TestLoad_Minimal(t *testing.T) {
	spec, err := Load(filepath.Join("testdata", "minimal.yaml"))
	if err != nil {
		t.Fatalf("Load minimal.yaml: %v", err)
	}
	if spec.Hostname != "node-02" {
		t.Errorf("hostname = %q, want node-02", spec.Hostname)
	}
	if spec.Harden != nil || spec.Tailscale != nil || spec.K3s != nil {
		t.Errorf("expected absent sections, got %+v", spec)
	}
}

func TestLoad_Malformed(t *testing.T) {
	_, err := Load(filepath.Join("testdata", "malformed.yaml"))
	if err == nil {
		t.Fatal("expected malformed YAML to fail")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error %q should mention parse", err)
	}
	// yaml.v3 surfaces line numbers — verify the wrapped error retained them.
	if !strings.Contains(err.Error(), "line ") {
		t.Errorf("error %q should retain line info from yaml.v3", err)
	}
}

func TestLoad_MissingPath(t *testing.T) {
	if _, err := Load(""); err == nil {
		t.Fatal("expected empty path to fail")
	}
	if _, err := Load(filepath.Join("testdata", "does-not-exist.yaml")); err == nil {
		t.Fatal("expected missing file to fail")
	}
}

func TestLoad_UnknownField(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "unknown.yaml")
	if err := os.WriteFile(p, []byte("nonsense_field: 1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected unknown field to fail strict decode")
	}
}

func TestValidate_Harden(t *testing.T) {
	cases := []struct {
		name string
		spec *Spec
		ok   bool
	}{
		{"disabled-no-fields", &Spec{Harden: &HardenSpec{Enabled: false}}, true},
		{"missing-key", &Spec{Harden: &HardenSpec{Enabled: true, User: "ops"}}, false},
		{"missing-user", &Spec{Harden: &HardenSpec{Enabled: true, SSHPubKey: "ssh"}}, false},
		{"complete", &Spec{Harden: &HardenSpec{Enabled: true, SSHPubKey: "ssh", User: "ops"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if tc.ok && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestValidate_Tailscale(t *testing.T) {
	cases := []struct {
		name string
		spec *Spec
		ok   bool
	}{
		{"disabled", &Spec{Tailscale: &TailscaleSpec{Enabled: false}}, true},
		{"key-only", &Spec{Tailscale: &TailscaleSpec{Enabled: true, AuthKey: "k"}}, true},
		{"env-only", &Spec{Tailscale: &TailscaleSpec{Enabled: true, AuthKeyEnv: "E"}}, true},
		{"both", &Spec{Tailscale: &TailscaleSpec{Enabled: true, AuthKey: "k", AuthKeyEnv: "E"}}, false},
		{"neither", &Spec{Tailscale: &TailscaleSpec{Enabled: true}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if tc.ok && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestValidate_K3s(t *testing.T) {
	cases := []struct {
		name string
		spec *Spec
		ok   bool
	}{
		{"disabled", &Spec{K3s: &K3sSpec{Enabled: false}}, true},
		{"good-server", &Spec{K3s: &K3sSpec{Enabled: true, Role: "server", Version: "v1"}}, true},
		{"good-server-init", &Spec{K3s: &K3sSpec{Enabled: true, Role: "server-init", Version: "v1"}}, true},
		{"good-agent", &Spec{K3s: &K3sSpec{Enabled: true, Role: "agent", Version: "v1", ServerURL: "https://x:6443", Token: "tok"}}, true},
		{"good-agent-token-env", &Spec{K3s: &K3sSpec{Enabled: true, Role: "agent", Version: "v1", ServerURL: "https://x:6443", TokenEnv: "MY_TOKEN"}}, true},
		{"agent-missing-server-url", &Spec{K3s: &K3sSpec{Enabled: true, Role: "agent", Version: "v1", Token: "tok"}}, false},
		{"agent-missing-token", &Spec{K3s: &K3sSpec{Enabled: true, Role: "agent", Version: "v1", ServerURL: "https://x:6443"}}, false},
		{"agent-both-token-and-env", &Spec{K3s: &K3sSpec{Enabled: true, Role: "agent", Version: "v1", ServerURL: "https://x:6443", Token: "t", TokenEnv: "E"}}, false},
		{"unknown-role", &Spec{K3s: &K3sSpec{Enabled: true, Role: "leader", Version: "v1"}}, false},
		{"missing-version", &Spec{K3s: &K3sSpec{Enabled: true, Role: "server"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if tc.ok && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestValidate_NilSpec(t *testing.T) {
	var s *Spec
	if err := s.Validate(); err == nil {
		t.Error("expected nil spec to fail")
	}
}
