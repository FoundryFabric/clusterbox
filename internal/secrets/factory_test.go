package secrets_test

import (
	"context"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/secrets"
)

// TestNewProvider_DevBackend verifies that SECRETS_BACKEND=dev (or unset) returns
// a non-nil Provider without error.
func TestNewProvider_DevBackend(t *testing.T) {
	t.Setenv("SECRETS_BACKEND", "dev")

	p, err := secrets.NewProvider(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil Provider")
	}
}

// TestNewProvider_DefaultIsDevBackend verifies that an unset SECRETS_BACKEND
// defaults to the dev backend.
func TestNewProvider_DefaultIsDevBackend(t *testing.T) {
	t.Setenv("SECRETS_BACKEND", "")

	p, err := secrets.NewProvider(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil Provider")
	}
}

// TestNewProvider_OnePasswordBackend verifies that SECRETS_BACKEND=onepassword
// returns a non-nil Provider (credentials are not validated at construction).
func TestNewProvider_OnePasswordBackend(t *testing.T) {
	t.Setenv("SECRETS_BACKEND", "onepassword")

	p, err := secrets.NewProvider(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil Provider")
	}
}

// TestNewProvider_VaultBackend verifies that SECRETS_BACKEND=vault returns a
// non-nil Provider (credentials are not validated at construction).
func TestNewProvider_VaultBackend(t *testing.T) {
	t.Setenv("SECRETS_BACKEND", "vault")

	p, err := secrets.NewProvider(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil Provider")
	}
}

// TestNewProvider_UnknownBackend verifies that an unknown SECRETS_BACKEND
// returns a descriptive error.
func TestNewProvider_UnknownBackend(t *testing.T) {
	t.Setenv("SECRETS_BACKEND", "unknown-backend")

	_, err := secrets.NewProvider(context.Background())
	if err == nil {
		t.Fatal("expected error for unknown backend, got nil")
	}
	if msg := err.Error(); msg == "" {
		t.Error("error message must not be empty")
	}
}
