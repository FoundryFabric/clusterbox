package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// NewRegistry returns a Registry based on the REGISTRY_BACKEND environment
// variable. Valid values are:
//
//	sqlite — local SQLite-backed registry (default)
//
// When REGISTRY_BACKEND is unset, "sqlite" is assumed.
//
// The sqlite backend itself is intentionally not wired up in this skeleton —
// it is delivered in task T2 and lives in the internal/registry/sqlite
// subpackage. Until then, selecting it returns a clear error that mentions
// T2 so callers can distinguish "unknown backend" from "not yet implemented".
func NewRegistry(_ context.Context) (Registry, error) {
	backend := os.Getenv("REGISTRY_BACKEND")
	if backend == "" {
		backend = "sqlite"
	}

	switch backend {
	case "sqlite":
		// TODO(T2): construct internal/registry/sqlite.New(...) here.
		return nil, errors.New("registry: sqlite backend not yet implemented (see T2)")

	default:
		return nil, fmt.Errorf("registry: unknown backend %q", backend)
	}
}
