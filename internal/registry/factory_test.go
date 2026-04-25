package registry_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/registry"
	_ "github.com/foundryfabric/clusterbox/internal/registry/sqlite"
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
// selects the sqlite backend and successfully opens it. We point HOME at a
// tempdir so the test does not write to the developer's real home.
func TestNewRegistry_DefaultIsSQLite(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("REGISTRY_BACKEND", "")

	reg, err := registry.NewRegistry(context.Background())
	if err != nil {
		t.Fatalf("NewRegistry returned error: %v", err)
	}
	t.Cleanup(func() {
		if cerr := reg.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	})

	dbPath := filepath.Join(tmp, ".clusterbox", "registry.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected db at %q: %v", dbPath, err)
	}
}

// TestNewRegistry_SQLiteExplicit verifies that explicitly selecting the
// sqlite backend opens a working registry.
func TestNewRegistry_SQLiteExplicit(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("REGISTRY_BACKEND", "sqlite")

	reg, err := registry.NewRegistry(context.Background())
	if err != nil {
		t.Fatalf("NewRegistry returned error: %v", err)
	}
	t.Cleanup(func() {
		if cerr := reg.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	})

	// Sanity: a fresh registry has no clusters and Get returns
	// ErrNotFound (proving the schema was applied).
	_, err = reg.GetCluster(context.Background(), "absent")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetCluster on fresh db: want ErrNotFound, got %v", err)
	}
}
