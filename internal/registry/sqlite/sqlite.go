// Package sqlite provides a SQLite-backed implementation of the
// registry.Registry interface.
//
// The store is a single on-disk file (typically ~/.clusterbox/registry.db)
// opened in WAL mode for crash safety and concurrent reads. The schema is
// applied on first open via the embedded migrations under
// internal/registry/migrations/sqlite.
//
// Field mapping note: a few fields on registry.Cluster, registry.Node, and
// registry.Deployment have no dedicated column in the v1 schema (e.g.
// Node.Address, Cluster.UpdatedAt, Deployment status detail vs duration).
// The implementation maps these conservatively: Roles is stored as a
// comma-separated string, UpdatedAt is derived from the canonical
// timestamp column, and write-only schema columns (kubeconfig_path,
// deployed_by, rollout_duration_ms) default to empty/zero. These are
// preserved on read and may be populated by later tasks that extend the
// type set.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/registry/migrations"

	_ "modernc.org/sqlite"
)

// dsnPragmas are appended to every file: DSN to set WAL, a generous busy
// timeout, and to enable foreign-key cascade behaviour.
const dsnPragmas = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"

// Provider is a SQLite-backed Registry. The zero value is not usable; use
// New.
type Provider struct {
	db *sql.DB

	closeOnce sync.Once
	closeErr  error
}

// New opens (or creates) a SQLite database at path, applies any pending
// migrations, and returns a Provider ready to serve Registry calls.
//
// The caller is responsible for ensuring the parent directory exists with
// appropriate permissions; New does not create directories. After opening,
// New verifies the file is mode 0600 and chmod's it if not.
func New(path string) (*Provider, error) {
	if path == "" {
		return nil, errors.New("registry/sqlite: empty database path")
	}

	dsn := "file:" + path + dsnPragmas
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("registry/sqlite: open: %w", err)
	}

	// modernc.org/sqlite is a single-file driver. We don't need a large
	// pool; serialising writes through a small pool keeps WAL writers
	// happy and avoids spurious busy errors.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	// Force a connection so the file actually gets created before we
	// chmod it. PingContext also surfaces driver errors immediately.
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("registry/sqlite: ping: %w", err)
	}

	if err := chmodDBFile(path); err != nil {
		_ = db.Close()
		return nil, err
	}

	if err := migrations.ApplySQLite(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("registry/sqlite: apply migrations: %w", err)
	}

	return &Provider{db: db}, nil
}

// Close releases the underlying database. It is safe to call multiple
// times; subsequent calls return the result of the first.
func (p *Provider) Close() error {
	p.closeOnce.Do(func() {
		if p.db != nil {
			p.closeErr = p.db.Close()
		}
	})
	return p.closeErr
}

// UpsertCluster inserts the cluster if absent or updates the mutable fields
// in place. CreatedAt is preserved on update.
func (p *Provider) UpsertCluster(ctx context.Context, c registry.Cluster) error {
	const stmt = `
		INSERT INTO clusters (name, provider, region, env, created_at, kubeconfig_path, last_synced_at)
		VALUES (?, ?, ?, ?, ?, '', ?)
		ON CONFLICT(name) DO UPDATE SET
			provider = excluded.provider,
			region = excluded.region,
			env = excluded.env,
			last_synced_at = excluded.last_synced_at
	`
	created := nowIfZero(c.CreatedAt)
	_, err := p.db.ExecContext(ctx, stmt,
		c.Name,
		c.Provider,
		c.Region,
		c.Env,
		created.UTC(),
		nullableTime(c.LastSynced),
	)
	if err != nil {
		return fmt.Errorf("registry/sqlite: upsert cluster %q: %w", c.Name, err)
	}
	return nil
}

// GetCluster returns the cluster row with name == name, or
// registry.ErrNotFound.
func (p *Provider) GetCluster(ctx context.Context, name string) (registry.Cluster, error) {
	const stmt = `
		SELECT name, provider, region, env, created_at, last_synced_at
		FROM clusters
		WHERE name = ?
	`
	var (
		c          registry.Cluster
		lastSynced sql.NullTime
	)
	row := p.db.QueryRowContext(ctx, stmt, name)
	if err := row.Scan(&c.Name, &c.Provider, &c.Region, &c.Env, &c.CreatedAt, &lastSynced); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return registry.Cluster{}, registry.ErrNotFound
		}
		return registry.Cluster{}, fmt.Errorf("registry/sqlite: get cluster %q: %w", name, err)
	}
	c.CreatedAt = c.CreatedAt.UTC()
	c.UpdatedAt = c.CreatedAt
	if lastSynced.Valid {
		c.LastSynced = lastSynced.Time.UTC()
	}
	return c, nil
}

// ListClusters returns every cluster row in arbitrary order.
func (p *Provider) ListClusters(ctx context.Context) ([]registry.Cluster, error) {
	const stmt = `
		SELECT name, provider, region, env, created_at, last_synced_at
		FROM clusters
	`
	rows, err := p.db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("registry/sqlite: list clusters: %w", err)
	}
	defer rows.Close()

	var out []registry.Cluster
	for rows.Next() {
		var (
			c          registry.Cluster
			lastSynced sql.NullTime
		)
		if err := rows.Scan(&c.Name, &c.Provider, &c.Region, &c.Env, &c.CreatedAt, &lastSynced); err != nil {
			return nil, fmt.Errorf("registry/sqlite: scan cluster: %w", err)
		}
		c.CreatedAt = c.CreatedAt.UTC()
		c.UpdatedAt = c.CreatedAt
		if lastSynced.Valid {
			c.LastSynced = lastSynced.Time.UTC()
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry/sqlite: iterate clusters: %w", err)
	}
	return out, nil
}

// DeleteCluster removes the named cluster and (via foreign-key cascade) its
// nodes and current deployments. History rows are not cascaded — they are
// retained for audit. Deleting a non-existent cluster is a no-op.
func (p *Provider) DeleteCluster(ctx context.Context, name string) error {
	if _, err := p.db.ExecContext(ctx, `DELETE FROM clusters WHERE name = ?`, name); err != nil {
		return fmt.Errorf("registry/sqlite: delete cluster %q: %w", name, err)
	}
	return nil
}

// UpsertNode inserts or updates a node identified by (cluster_name, hostname).
func (p *Provider) UpsertNode(ctx context.Context, n registry.Node) error {
	const stmt = `
		INSERT INTO nodes (cluster_name, hostname, role, joined_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(cluster_name, hostname) DO UPDATE SET
			role = excluded.role
	`
	role := strings.Join(n.Roles, ",")
	joined := nowIfZero(n.CreatedAt).UTC()
	if _, err := p.db.ExecContext(ctx, stmt, n.ClusterName, n.Hostname, role, joined); err != nil {
		return fmt.Errorf("registry/sqlite: upsert node %s/%s: %w", n.ClusterName, n.Hostname, err)
	}
	return nil
}

// RemoveNode deletes the node row identified by (clusterName, hostname).
// Removing a non-existent row is a no-op.
func (p *Provider) RemoveNode(ctx context.Context, clusterName, hostname string) error {
	const stmt = `DELETE FROM nodes WHERE cluster_name = ? AND hostname = ?`
	if _, err := p.db.ExecContext(ctx, stmt, clusterName, hostname); err != nil {
		return fmt.Errorf("registry/sqlite: remove node %s/%s: %w", clusterName, hostname, err)
	}
	return nil
}

// ListNodes returns every node attached to clusterName.
func (p *Provider) ListNodes(ctx context.Context, clusterName string) ([]registry.Node, error) {
	const stmt = `
		SELECT cluster_name, hostname, role, joined_at
		FROM nodes
		WHERE cluster_name = ?
	`
	rows, err := p.db.QueryContext(ctx, stmt, clusterName)
	if err != nil {
		return nil, fmt.Errorf("registry/sqlite: list nodes for %q: %w", clusterName, err)
	}
	defer rows.Close()

	var out []registry.Node
	for rows.Next() {
		var (
			n      registry.Node
			role   string
			joined time.Time
		)
		if err := rows.Scan(&n.ClusterName, &n.Hostname, &role, &joined); err != nil {
			return nil, fmt.Errorf("registry/sqlite: scan node: %w", err)
		}
		n.Roles = splitRoles(role)
		n.CreatedAt = joined.UTC()
		n.UpdatedAt = n.CreatedAt
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry/sqlite: iterate nodes: %w", err)
	}
	return out, nil
}

// UpsertDeployment inserts or updates the (cluster_name, service) row.
func (p *Provider) UpsertDeployment(ctx context.Context, d registry.Deployment) error {
	const stmt = `
		INSERT INTO deployments (cluster_name, service, version, deployed_at, deployed_by, status)
		VALUES (?, ?, ?, ?, '', ?)
		ON CONFLICT(cluster_name, service) DO UPDATE SET
			version = excluded.version,
			deployed_at = excluded.deployed_at,
			status = excluded.status
	`
	deployedAt := nowIfZero(d.UpdatedAt).UTC()
	if _, err := p.db.ExecContext(ctx, stmt,
		d.ClusterName, d.Service, d.Version, deployedAt, string(d.Status),
	); err != nil {
		return fmt.Errorf("registry/sqlite: upsert deployment %s/%s: %w", d.ClusterName, d.Service, err)
	}
	return nil
}

// GetDeployment returns the current deployment for (clusterName, service),
// or registry.ErrNotFound.
func (p *Provider) GetDeployment(ctx context.Context, clusterName, service string) (registry.Deployment, error) {
	const stmt = `
		SELECT cluster_name, service, version, status, deployed_at
		FROM deployments
		WHERE cluster_name = ? AND service = ?
	`
	var (
		d      registry.Deployment
		status string
	)
	row := p.db.QueryRowContext(ctx, stmt, clusterName, service)
	if err := row.Scan(&d.ClusterName, &d.Service, &d.Version, &status, &d.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return registry.Deployment{}, registry.ErrNotFound
		}
		return registry.Deployment{}, fmt.Errorf("registry/sqlite: get deployment %s/%s: %w", clusterName, service, err)
	}
	d.Status = registry.DeploymentStatus(status)
	d.UpdatedAt = d.UpdatedAt.UTC()
	return d, nil
}

// ListDeployments returns every deployment for clusterName.
func (p *Provider) ListDeployments(ctx context.Context, clusterName string) ([]registry.Deployment, error) {
	const stmt = `
		SELECT cluster_name, service, version, status, deployed_at
		FROM deployments
		WHERE cluster_name = ?
	`
	rows, err := p.db.QueryContext(ctx, stmt, clusterName)
	if err != nil {
		return nil, fmt.Errorf("registry/sqlite: list deployments for %q: %w", clusterName, err)
	}
	defer rows.Close()

	var out []registry.Deployment
	for rows.Next() {
		var (
			d      registry.Deployment
			status string
		)
		if err := rows.Scan(&d.ClusterName, &d.Service, &d.Version, &status, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("registry/sqlite: scan deployment: %w", err)
		}
		d.Status = registry.DeploymentStatus(status)
		d.UpdatedAt = d.UpdatedAt.UTC()
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry/sqlite: iterate deployments: %w", err)
	}
	return out, nil
}

// AppendHistory records a single deployment attempt. Entries are
// append-only.
func (p *Provider) AppendHistory(ctx context.Context, e registry.DeploymentHistoryEntry) error {
	const stmt = `
		INSERT INTO deployment_history
			(cluster_name, service, version, attempted_at, status, rollout_duration_ms, error)
		VALUES (?, ?, ?, ?, ?, 0, ?)
	`
	occurredAt := nowIfZero(e.OccurredAt).UTC()
	if _, err := p.db.ExecContext(ctx, stmt,
		e.ClusterName, e.Service, e.Version, occurredAt, string(e.Status), e.Detail,
	); err != nil {
		return fmt.Errorf("registry/sqlite: append history: %w", err)
	}
	return nil
}

// ListHistory returns history rows matching filter, most-recent-first.
func (p *Provider) ListHistory(ctx context.Context, filter registry.HistoryFilter) ([]registry.DeploymentHistoryEntry, error) {
	var (
		clauses []string
		args    []any
	)
	if filter.ClusterName != "" {
		clauses = append(clauses, "cluster_name = ?")
		args = append(args, filter.ClusterName)
	}
	if filter.Service != "" {
		clauses = append(clauses, "service = ?")
		args = append(args, filter.Service)
	}

	q := "SELECT cluster_name, service, version, status, error, attempted_at FROM deployment_history"
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY attempted_at DESC, id DESC"
	if filter.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, filter.Limit)
	}

	rows, err := p.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("registry/sqlite: list history: %w", err)
	}
	defer rows.Close()

	var out []registry.DeploymentHistoryEntry
	for rows.Next() {
		var (
			e      registry.DeploymentHistoryEntry
			status string
		)
		if err := rows.Scan(&e.ClusterName, &e.Service, &e.Version, &status, &e.Detail, &e.OccurredAt); err != nil {
			return nil, fmt.Errorf("registry/sqlite: scan history: %w", err)
		}
		e.Status = registry.DeploymentStatus(status)
		e.OccurredAt = e.OccurredAt.UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry/sqlite: iterate history: %w", err)
	}
	return out, nil
}

// MarkSynced sets clusters.last_synced_at for the named cluster only.
func (p *Provider) MarkSynced(ctx context.Context, clusterName string, at time.Time) error {
	const stmt = `UPDATE clusters SET last_synced_at = ? WHERE name = ?`
	if _, err := p.db.ExecContext(ctx, stmt, at.UTC(), clusterName); err != nil {
		return fmt.Errorf("registry/sqlite: mark synced %q: %w", clusterName, err)
	}
	return nil
}

// nowIfZero returns t if non-zero, otherwise the current UTC time.
func nowIfZero(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}

// nullableTime returns sql.NullTime with .Valid=false when t is the zero
// value, so a never-synced cluster persists NULL rather than year-0001.
func nullableTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t.UTC(), Valid: true}
}

// splitRoles inverts the comma-join used by UpsertNode. Empty input yields
// nil so a freshly-read node round-trips to a nil slice rather than a
// single-element slice containing "".
func splitRoles(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
