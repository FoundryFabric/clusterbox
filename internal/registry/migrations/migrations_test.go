package migrations

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"
)

// openTempDB opens a fresh SQLite file in a tempdir and returns it. The
// file is closed via t.Cleanup.
func openTempDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "test.db") + "?_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestApplySQLite_Fresh verifies a freshly-opened DB ends at the latest
// embedded version after a single ApplySQLite call, and that the expected
// tables exist.
func TestApplySQLite_Fresh(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()

	if err := ApplySQLite(ctx, db); err != nil {
		t.Fatalf("ApplySQLite: %v", err)
	}

	v, err := readSchemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("readSchemaVersion: %v", err)
	}
	if v < 1 {
		t.Fatalf("expected schema_version >= 1 after first apply, got %d", v)
	}

	// Spot-check that core tables exist.
	for _, table := range []string{"clusters", "nodes", "deployments", "deployment_history"} {
		var name string
		row := db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table)
		if err := row.Scan(&name); err != nil {
			t.Fatalf("expected table %q to exist: %v", table, err)
		}
	}
}

// TestApplySQLite_Idempotent verifies that calling ApplySQLite repeatedly
// against an already-migrated DB is a no-op and does not change the
// recorded version.
func TestApplySQLite_Idempotent(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()

	if err := ApplySQLite(ctx, db); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	v1, err := readSchemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if err := ApplySQLite(ctx, db); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	v2, err := readSchemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if v1 != v2 {
		t.Fatalf("schema_version changed across no-op re-apply: %d -> %d", v1, v2)
	}
}

// TestApply_DBNewerThanBinary simulates a binary running against a DB that
// was migrated forward by a future binary. ErrSchemaNewer must surface.
func TestApply_DBNewerThanBinary(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()

	// Stamp a higher version than any embedded migration.
	if err := ensureSchemaVersionTable(ctx, db); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_version(version) VALUES (?)`, 9999); err != nil {
		t.Fatalf("seed version: %v", err)
	}

	err := ApplySQLite(ctx, db)
	if !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("want ErrSchemaNewer, got %v", err)
	}
}

// TestLoadMigrations_MalformedFilenameRejected uses an in-memory FS with a
// non-conforming filename to prove the regex gate works.
func TestLoadMigrations_MalformedFilenameRejected(t *testing.T) {
	fsys := fstest.MapFS{
		"sqlite/init.sql":      {Data: []byte("CREATE TABLE x(y INT);")},
		"sqlite/0001_init.sql": {Data: []byte("CREATE TABLE x(y INT);")},
	}
	if _, err := loadMigrations(fsys, "sqlite"); err == nil {
		t.Fatal("expected error for malformed filename, got nil")
	}
}

// TestLoadMigrations_DuplicateVersionRejected proves we refuse two
// migrations claiming the same version.
func TestLoadMigrations_DuplicateVersionRejected(t *testing.T) {
	fsys := fstest.MapFS{
		"sqlite/0001_a.sql": {Data: []byte("CREATE TABLE a(x INT);")},
		"sqlite/0001_b.sql": {Data: []byte("CREATE TABLE b(x INT);")},
	}
	if _, err := loadMigrations(fsys, "sqlite"); err == nil {
		t.Fatal("expected error for duplicate version, got nil")
	}
}

// TestApplySQLite_ForwardFromV1 stamps a database at the v1 schema (using
// the v0001_init.sql body directly, with schema_version pinned at 1) and
// then runs ApplySQLite. The end state must report version 2, the kind
// column must exist on both deployments and deployment_history with
// DEFAULT 'app', any pre-existing rows must read back as 'app', and the
// idx_deployments_kind index must be present.
func TestApplySQLite_ForwardFromV1(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()

	// Load only the v1 migration so the DB starts at schema_version=1.
	v1Only, err := loadMigrations(sqliteFS, "sqlite")
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	v1 := v1Only[:0]
	for _, m := range v1Only {
		if m.version == 1 {
			v1 = append(v1, m)
		}
	}
	if len(v1) != 1 {
		t.Fatalf("expected exactly one v1 migration, got %d", len(v1))
	}
	if err := apply(ctx, db, v1); err != nil {
		t.Fatalf("apply v1: %v", err)
	}
	if v, _ := readSchemaVersion(ctx, db); v != 1 {
		t.Fatalf("setup: want schema_version=1, got %d", v)
	}

	// Seed a row written under v1 (no kind column yet) — it must inherit
	// the DEFAULT 'app' once the kind column is added.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO clusters (name, provider, region, env, created_at, kubeconfig_path) VALUES (?, ?, ?, ?, ?, ?)`,
		"alpha", "hcloud", "nbg1", "prod", "2026-01-01T00:00:00Z", "/tmp/kc",
	); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO deployments (cluster_name, service, version, deployed_at, deployed_by, status) VALUES (?, ?, ?, ?, ?, ?)`,
		"alpha", "api", "v1", "2026-01-01T00:00:00Z", "alice", "rolled_out",
	); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO deployment_history (cluster_name, service, version, attempted_at, status, rollout_duration_ms, error) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"alpha", "api", "v1", "2026-01-01T00:00:00Z", "rolled_out", 100, "",
	); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	// Now apply all embedded migrations — should advance v1 -> v5.
	if err := ApplySQLite(ctx, db); err != nil {
		t.Fatalf("ApplySQLite forward: %v", err)
	}
	v, err := readSchemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("readSchemaVersion: %v", err)
	}
	if v != 5 {
		t.Fatalf("want schema_version=5 after forward apply, got %d", v)
	}

	// After migration 0005 the deployments table uses cluster_id (not
	// cluster_name). Retrieve the cluster id for 'alpha' and use it.
	var clusterID int64
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM clusters WHERE name='alpha' AND destroyed_at IS NULL`).Scan(&clusterID); err != nil {
		t.Fatalf("read cluster id for alpha: %v", err)
	}

	// Pre-existing rows must show kind='app'.
	var kind string
	if err := db.QueryRowContext(ctx,
		`SELECT kind FROM deployments WHERE cluster_id=? AND service='api'`, clusterID).Scan(&kind); err != nil {
		t.Fatalf("read deployments.kind: %v", err)
	}
	if kind != "app" {
		t.Errorf("deployments.kind: want %q, got %q", "app", kind)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT kind FROM deployment_history WHERE cluster_name='alpha' AND service='api'`).Scan(&kind); err != nil {
		t.Fatalf("read deployment_history.kind: %v", err)
	}
	if kind != "app" {
		t.Errorf("deployment_history.kind: want %q, got %q", "app", kind)
	}

	// idx_deployments_kind must exist.
	var idx string
	if err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_deployments_kind'`).Scan(&idx); err != nil {
		t.Fatalf("expected idx_deployments_kind to exist: %v", err)
	}
}

// TestApply_OrderedByVersion verifies migrations run in version order even
// if one is added "out of order" lexically (synthetic FS test).
func TestApply_OrderedByVersion(t *testing.T) {
	fsys := fstest.MapFS{
		"sqlite/0002_b.sql": {Data: []byte(`CREATE TABLE b(x INTEGER);
INSERT INTO b VALUES (2);`)},
		"sqlite/0001_a.sql": {Data: []byte(`CREATE TABLE a(x INTEGER);
INSERT INTO a VALUES (1);`)},
	}
	migs, err := loadMigrations(fsys, "sqlite")
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migs) != 2 {
		t.Fatalf("want 2 migrations, got %d", len(migs))
	}
	if migs[0].version != 1 || migs[1].version != 2 {
		t.Fatalf("migrations out of order: %+v", migs)
	}

	db := openTempDB(t)
	ctx := context.Background()
	if err := apply(ctx, db, migs); err != nil {
		t.Fatalf("apply: %v", err)
	}

	v, err := readSchemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if v != 2 {
		t.Fatalf("want version 2, got %d", v)
	}
}
