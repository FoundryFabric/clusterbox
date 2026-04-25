package sqlite

import (
	"fmt"
	"os"
)

// dbFileMode is the on-disk permission for the SQLite database file. The
// database may contain hostnames, kubeconfig paths, and deployment history
// — we treat it as user-readable only.
const dbFileMode os.FileMode = 0o600

// chmodDBFile ensures the SQLite file at path has mode 0600. The driver
// creates the file on first use with the process umask applied to 0666,
// which on a typical developer machine yields 0644 — we tighten that here.
func chmodDBFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("registry/sqlite: stat db file: %w", err)
	}
	if info.Mode().Perm() == dbFileMode {
		return nil
	}
	if err := os.Chmod(path, dbFileMode); err != nil {
		return fmt.Errorf("registry/sqlite: chmod db file: %w", err)
	}
	return nil
}
