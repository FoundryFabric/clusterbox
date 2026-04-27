package cmd_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/cmd"
	"github.com/foundryfabric/clusterbox/internal/config"
)

func TestContextShow_NoActiveContext(t *testing.T) {
	var buf bytes.Buffer
	err := cmd.RunContextShowWith(func() (*config.Config, error) {
		return &config.Config{}, nil
	}, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "No active context") {
		t.Errorf("expected no-context message, got: %q", buf.String())
	}
}

func TestContextShow_PrintsActiveContext(t *testing.T) {
	var buf bytes.Buffer
	err := cmd.RunContextShowWith(func() (*config.Config, error) {
		return &config.Config{
			CurrentContext: "foundryfabric",
			Contexts: map[string]*config.Context{
				"foundryfabric": {
					SecretsBackend: "onepassword",
					Infra: config.InfraConfig{
						Hetzner: "op://FoundryFabric/Hetzner/credential",
					},
				},
			},
		}, nil
	}, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"foundryfabric", "onepassword", "op://FoundryFabric/Hetzner/credential"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestContextList_Empty(t *testing.T) {
	var buf bytes.Buffer
	err := cmd.RunContextListWith(func() (*config.Config, error) {
		return &config.Config{}, nil
	}, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "No contexts") {
		t.Errorf("expected empty message, got: %q", buf.String())
	}
}

func TestContextList_MarksActive(t *testing.T) {
	var buf bytes.Buffer
	err := cmd.RunContextListWith(func() (*config.Config, error) {
		return &config.Config{
			CurrentContext: "foundryfabric",
			Contexts: map[string]*config.Context{
				"foundryfabric": {SecretsBackend: "onepassword"},
				"personal":      {SecretsBackend: "onepassword"},
			},
		}, nil
	}, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var activeLine string
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "*") {
			activeLine = l
		}
	}
	if !strings.Contains(activeLine, "foundryfabric") {
		t.Errorf("active marker not on foundryfabric line; lines: %v", lines)
	}
}
