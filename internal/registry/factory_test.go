package registry_test

import (
	"context"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/registry"
)

// TestNewRegistry_UnknownBackend verifies that an unrecognised
// REGISTRY_BACKEND value produces the documented error message.
func TestNewRegistry_UnknownBackend(t *testing.T) {
	t.Setenv("REGISTRY_BACKEND", "not-a-real-backend")

	_, err := registry.NewRegistry(context.Background())
	if err == nil {
		t.Fatal("expected error for unknown backend, got nil")
	}
	want := `registry: unknown backend "not-a-real-backend"`
	if err.Error() != want {
		t.Errorf("error mismatch:\n  got:  %q\n  want: %q", err.Error(), want)
	}
}

// TestNewRegistry_DefaultIsSQLite verifies that an unset REGISTRY_BACKEND
// selects the sqlite backend (which currently returns the T2 stub error
// rather than the unknown-backend error).
func TestNewRegistry_DefaultIsSQLite(t *testing.T) {
	t.Setenv("REGISTRY_BACKEND", "")

	_, err := registry.NewRegistry(context.Background())
	if err == nil {
		t.Fatal("expected sqlite stub error, got nil")
	}
	if strings.Contains(err.Error(), "unknown backend") {
		t.Fatalf("default backend should be sqlite, not unknown: %v", err)
	}
	if !strings.Contains(err.Error(), "T2") {
		t.Errorf("expected sqlite stub error to mention T2, got: %v", err)
	}
}

// TestNewRegistry_SQLiteStubMentionsT2 verifies that explicitly selecting the
// sqlite backend returns the temporary stub error pointing at task T2.
func TestNewRegistry_SQLiteStubMentionsT2(t *testing.T) {
	t.Setenv("REGISTRY_BACKEND", "sqlite")

	_, err := registry.NewRegistry(context.Background())
	if err == nil {
		t.Fatal("expected sqlite stub error, got nil")
	}
	if !strings.Contains(err.Error(), "sqlite") {
		t.Errorf("expected error to mention sqlite, got: %v", err)
	}
	if !strings.Contains(err.Error(), "T2") {
		t.Errorf("expected error to mention T2, got: %v", err)
	}
}
