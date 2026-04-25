package registry

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// defaultRegistryDir is the parent directory under the user's home where
// clusterbox stores per-user state, including the registry database.
const defaultRegistryDir = ".clusterbox"

// defaultRegistryFile is the on-disk filename of the SQLite registry.
const defaultRegistryFile = "registry.db"

// registryDirMode is the permission applied to the registry parent
// directory when it is created on first use.
const registryDirMode os.FileMode = 0o700

// BackendFactory constructs a Registry. Implementations register
// themselves under a name via Register; NewRegistry dispatches to the
// matching factory based on REGISTRY_BACKEND.
//
// The factory takes a default path that NewRegistry has already prepared
// (parent directory created, etc.). Backends that don't use a filesystem
// path may ignore it.
type BackendFactory func(path string) (Registry, error)

var (
	backendsMu sync.RWMutex
	backends   = map[string]BackendFactory{}
)

// Register associates a backend name (e.g. "sqlite") with a factory. It is
// intended to be called from the backend package's init function. Calling
// Register a second time with the same name overwrites the prior entry,
// which keeps tests that re-register simple.
func Register(name string, f BackendFactory) {
	backendsMu.Lock()
	defer backendsMu.Unlock()
	backends[name] = f
}

// lookup returns the factory registered under name, or false.
func lookup(name string) (BackendFactory, bool) {
	backendsMu.RLock()
	defer backendsMu.RUnlock()
	f, ok := backends[name]
	return f, ok
}

// NewRegistry returns a Registry based on the REGISTRY_BACKEND environment
// variable. Valid values are:
//
//	sqlite — local SQLite-backed registry (default)
//
// When REGISTRY_BACKEND is unset, "sqlite" is assumed. The sqlite backend
// stores its data at ~/.clusterbox/registry.db; the parent directory is
// created with mode 0700 if missing.
//
// Backends register themselves via Register, typically from an init
// function. The package using NewRegistry must therefore have a (possibly
// blank) import of every backend it expects to be available.
func NewRegistry(_ context.Context) (Registry, error) {
	backend := os.Getenv("REGISTRY_BACKEND")
	if backend == "" {
		backend = "sqlite"
	}

	f, ok := lookup(backend)
	if !ok {
		return nil, fmt.Errorf("registry: unknown backend %q", backend)
	}

	path, err := defaultPath()
	if err != nil {
		return nil, err
	}
	if err := ensureParentDir(path); err != nil {
		return nil, err
	}
	return f(path)
}

// defaultPath returns the absolute path to the default registry file under
// the user's home directory.
func defaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("registry: resolve home dir: %w", err)
	}
	return filepath.Join(home, defaultRegistryDir, defaultRegistryFile), nil
}

// ensureParentDir creates the parent directory of path with mode 0700 if it
// does not already exist. ensureParentDir does not relax existing
// permissions; if the directory was created with looser bits the user has
// either intentionally widened access or some other process owns it.
func ensureParentDir(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, registryDirMode); err != nil {
		return fmt.Errorf("registry: create parent dir %q: %w", dir, err)
	}
	return nil
}
