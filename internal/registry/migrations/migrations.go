// Package migrations applies forward-only schema migrations to a registry
// backend's underlying database.
//
// Migrations are stored as standalone SQL files embedded into the binary.
// Each file is named with a zero-padded integer version prefix followed by
// an underscore and a short description, for example "0001_init.sql".
// Files are sorted lexically and applied in order. The applied version is
// recorded in a single-row "schema_version" table.
//
// The directory layout reserves separate slots for different backend
// flavours; today only the sqlite/ slot is populated. A future
// FoundryFabric-backed registry will add its own ff/ migrations.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed sqlite/*.sql
var sqliteFS embed.FS

// ErrSchemaNewer is returned by ApplySQLite when the database reports a
// schema_version higher than any embedded migration. This typically means an
// older binary is being run against a database that a newer binary already
// migrated forward.
var ErrSchemaNewer = errors.New("registry: unsupported registry schema version (db is newer than this binary)")

// migrationFilenameRE matches "NNNN_description.sql" where NNNN is a
// non-empty run of digits. The description must be present and non-empty.
var migrationFilenameRE = regexp.MustCompile(`^([0-9]+)_[A-Za-z0-9._-]+\.sql$`)

// migration is a single parsed migration file ready to apply.
type migration struct {
	version int
	name    string
	sql     string
}

// ApplySQLite applies any embedded sqlite/*.sql migrations whose version is
// greater than the current schema_version. The schema_version table is
// created on first use. Each migration runs inside its own transaction; on
// failure the transaction is rolled back and the error is returned.
//
// ApplySQLite is idempotent: running it again with no new migrations is a
// no-op. It is forward-only — it never reverses a migration. If the database
// reports a version higher than this binary knows about, ErrSchemaNewer is
// returned.
func ApplySQLite(ctx context.Context, db *sql.DB) error {
	migs, err := loadMigrations(sqliteFS, "sqlite")
	if err != nil {
		return err
	}
	return apply(ctx, db, migs)
}

// loadMigrations reads dir from fsys, validates filenames, parses versions,
// and returns the list sorted by version ascending. Duplicate versions are
// rejected.
func loadMigrations(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("registry: read migrations dir: %w", err)
	}

	out := make([]migration, 0, len(entries))
	seen := make(map[int]string, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		m := migrationFilenameRE.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("registry: malformed migration filename %q (want NNNN_desc.sql)", name)
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("registry: invalid version in %q: %w", name, err)
		}
		if prev, ok := seen[v]; ok {
			return nil, fmt.Errorf("registry: duplicate migration version %d in %q and %q", v, prev, name)
		}
		seen[v] = name

		body, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("registry: read migration %q: %w", name, err)
		}
		out = append(out, migration{version: v, name: name, sql: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// apply runs every migration whose version is greater than the database's
// current schema_version. It first ensures the schema_version table exists,
// then reads the current version, then iterates.
func apply(ctx context.Context, db *sql.DB, migs []migration) error {
	if len(migs) == 0 {
		return nil
	}

	if err := ensureSchemaVersionTable(ctx, db); err != nil {
		return err
	}

	current, err := readSchemaVersion(ctx, db)
	if err != nil {
		return err
	}

	maxKnown := migs[len(migs)-1].version
	if current > maxKnown {
		return ErrSchemaNewer
	}

	for _, m := range migs {
		if m.version <= current {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return fmt.Errorf("registry: apply migration %s: %w", m.name, err)
		}
	}
	return nil
}

// ensureSchemaVersionTable creates the schema_version bookkeeping table if it
// does not already exist. The table holds at most one row; a missing row is
// treated as version 0.
func ensureSchemaVersionTable(ctx context.Context, db *sql.DB) error {
	const stmt = `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("registry: create schema_version: %w", err)
	}
	return nil
}

// readSchemaVersion returns the highest applied migration version, or 0 if
// the schema_version table is empty.
func readSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v sql.NullInt64
	row := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`)
	if err := row.Scan(&v); err != nil {
		return 0, fmt.Errorf("registry: read schema_version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// applyOne runs a single migration inside a transaction and records the new
// version in schema_version. The full SQL body is sent to ExecContext, which
// the driver parses as a multi-statement script.
func applyOne(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("exec: %w", err)
	}

	// Replace the row rather than insert a new one to keep the table
	// single-row. DELETE + INSERT is safe under the migration transaction.
	if _, err := tx.ExecContext(ctx, `DELETE FROM schema_version`); err != nil {
		return fmt.Errorf("clear schema_version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version(version) VALUES (?)`, m.version); err != nil {
		return fmt.Errorf("write schema_version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}
