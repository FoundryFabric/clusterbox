package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/registry/sqlite"
)

// TestFilePermissions verifies that after a successful New, the on-disk
// SQLite file is mode 0600 and (when the test creates one) the parent
// directory is mode 0700.
func TestFilePermissions(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "regdir")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	dbPath := filepath.Join(dir, "registry.db")
	p, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	dbInfo, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if got := dbInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("db file mode: want 0600, got %o", got)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir mode: want 0700, got %o", got)
	}
}

// TestFilePermissions_TightenedFromUmask covers the branch where the
// driver creates the file with permissive bits (e.g. 0644) and our
// chmod-after-Open tightens it to 0600.
func TestFilePermissions_TightenedFromUmask(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "registry.db")

	// Pre-create the file with an overly-permissive mode so we can
	// observe that chmodDBFile tightens it. The driver will reuse the
	// existing file rather than re-creating it.
	if f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0o644); err != nil {
		t.Fatalf("pre-create: %v", err)
	} else {
		_ = f.Close()
	}

	p, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode: want 0600, got %o", got)
	}

	// Sanity: provider works.
	if _, err := p.ListClusters(context.Background()); err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
}
