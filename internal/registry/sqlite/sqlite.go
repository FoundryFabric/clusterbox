// Package sqlite provides a SQLite-backed implementation of the
// registry.Registry interface.
//
// The store is a single on-disk file (typically ~/.clusterbox/registry.db)
// opened in WAL mode for crash safety and concurrent reads. The schema is
// applied on first open via the embedded migrations under
// internal/registry/migrations/sqlite.
//
// The mapping between registry types and the v1 schema is 1:1. All
// timestamps are stored and returned in UTC.
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
// in place. CreatedAt is preserved on update. DestroyedAt is intentionally
// not written here: it is managed exclusively by MarkClusterDestroyed so an
// upsert from a sync job never accidentally clears a tombstone.
func (p *Provider) UpsertCluster(ctx context.Context, c registry.Cluster) error {
	const stmt = `
		INSERT INTO clusters (name, provider, region, env, created_at, kubeconfig_path, last_synced_at, destroyed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			provider = excluded.provider,
			region = excluded.region,
			env = excluded.env,
			kubeconfig_path = excluded.kubeconfig_path,
			last_synced_at = excluded.last_synced_at
	`
	created := nowIfZero(c.CreatedAt)
	_, err := p.db.ExecContext(ctx, stmt,
		c.Name,
		c.Provider,
		c.Region,
		c.Env,
		created.UTC(),
		c.KubeconfigPath,
		nullableTime(c.LastSynced),
		nullableTime(c.DestroyedAt),
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
		SELECT name, provider, region, env, created_at, kubeconfig_path, last_synced_at, destroyed_at
		FROM clusters
		WHERE name = ?
	`
	var (
		c           registry.Cluster
		lastSynced  sql.NullTime
		destroyedAt sql.NullTime
	)
	row := p.db.QueryRowContext(ctx, stmt, name)
	if err := row.Scan(&c.Name, &c.Provider, &c.Region, &c.Env, &c.CreatedAt, &c.KubeconfigPath, &lastSynced, &destroyedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return registry.Cluster{}, registry.ErrNotFound
		}
		return registry.Cluster{}, fmt.Errorf("registry/sqlite: get cluster %q: %w", name, err)
	}
	c.CreatedAt = c.CreatedAt.UTC()
	if lastSynced.Valid {
		c.LastSynced = lastSynced.Time.UTC()
	}
	if destroyedAt.Valid {
		c.DestroyedAt = destroyedAt.Time.UTC()
	}
	return c, nil
}

// ListClusters returns every cluster row in arbitrary order.
func (p *Provider) ListClusters(ctx context.Context) ([]registry.Cluster, error) {
	const stmt = `
		SELECT name, provider, region, env, created_at, kubeconfig_path, last_synced_at, destroyed_at
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
			c           registry.Cluster
			lastSynced  sql.NullTime
			destroyedAt sql.NullTime
		)
		if err := rows.Scan(&c.Name, &c.Provider, &c.Region, &c.Env, &c.CreatedAt, &c.KubeconfigPath, &lastSynced, &destroyedAt); err != nil {
			return nil, fmt.Errorf("registry/sqlite: scan cluster: %w", err)
		}
		c.CreatedAt = c.CreatedAt.UTC()
		if lastSynced.Valid {
			c.LastSynced = lastSynced.Time.UTC()
		}
		if destroyedAt.Valid {
			c.DestroyedAt = destroyedAt.Time.UTC()
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
//
// The metadata columns (arch, os_version, k3s_version, agent_version,
// last_inspected_at) are nullable: an empty string or zero time persists as
// NULL rather than the literal empty value, so a node row that has never
// been inspected reads back with zero-value Go fields.
func (p *Provider) UpsertNode(ctx context.Context, n registry.Node) error {
	const stmt = `
		INSERT INTO nodes (cluster_name, hostname, role, joined_at, arch, os_version, k3s_version, agent_version, last_inspected_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(cluster_name, hostname) DO UPDATE SET
			role = excluded.role,
			arch = excluded.arch,
			os_version = excluded.os_version,
			k3s_version = excluded.k3s_version,
			agent_version = excluded.agent_version,
			last_inspected_at = excluded.last_inspected_at
	`
	joined := nowIfZero(n.JoinedAt).UTC()
	if _, err := p.db.ExecContext(ctx, stmt,
		n.ClusterName,
		n.Hostname,
		n.Role,
		joined,
		nullableString(n.Arch),
		nullableString(n.OSVersion),
		nullableString(n.K3sVersion),
		nullableString(n.AgentVersion),
		nullableTime(n.LastInspectedAt),
	); err != nil {
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
		SELECT cluster_name, hostname, role, joined_at, arch, os_version, k3s_version, agent_version, last_inspected_at
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
			n               registry.Node
			joined          time.Time
			arch            sql.NullString
			osVersion       sql.NullString
			k3sVersion      sql.NullString
			agentVersion    sql.NullString
			lastInspectedAt sql.NullTime
		)
		if err := rows.Scan(&n.ClusterName, &n.Hostname, &n.Role, &joined, &arch, &osVersion, &k3sVersion, &agentVersion, &lastInspectedAt); err != nil {
			return nil, fmt.Errorf("registry/sqlite: scan node: %w", err)
		}
		n.JoinedAt = joined.UTC()
		if arch.Valid {
			n.Arch = arch.String
		}
		if osVersion.Valid {
			n.OSVersion = osVersion.String
		}
		if k3sVersion.Valid {
			n.K3sVersion = k3sVersion.String
		}
		if agentVersion.Valid {
			n.AgentVersion = agentVersion.String
		}
		if lastInspectedAt.Valid {
			n.LastInspectedAt = lastInspectedAt.Time.UTC()
		}
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
		INSERT INTO deployments (cluster_name, service, version, deployed_at, deployed_by, status, kind)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(cluster_name, service) DO UPDATE SET
			version = excluded.version,
			deployed_at = excluded.deployed_at,
			deployed_by = excluded.deployed_by,
			status = excluded.status,
			kind = excluded.kind
	`
	deployedAt := nowIfZero(d.DeployedAt).UTC()
	if _, err := p.db.ExecContext(ctx, stmt,
		d.ClusterName, d.Service, d.Version, deployedAt, d.DeployedBy, string(d.Status), string(defaultKind(d.Kind)),
	); err != nil {
		return fmt.Errorf("registry/sqlite: upsert deployment %s/%s: %w", d.ClusterName, d.Service, err)
	}
	return nil
}

// GetDeployment returns the current deployment for (clusterName, service),
// or registry.ErrNotFound.
func (p *Provider) GetDeployment(ctx context.Context, clusterName, service string) (registry.Deployment, error) {
	const stmt = `
		SELECT cluster_name, service, version, deployed_at, deployed_by, status, kind
		FROM deployments
		WHERE cluster_name = ? AND service = ?
	`
	var (
		d      registry.Deployment
		status string
		kind   string
	)
	row := p.db.QueryRowContext(ctx, stmt, clusterName, service)
	if err := row.Scan(&d.ClusterName, &d.Service, &d.Version, &d.DeployedAt, &d.DeployedBy, &status, &kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return registry.Deployment{}, registry.ErrNotFound
		}
		return registry.Deployment{}, fmt.Errorf("registry/sqlite: get deployment %s/%s: %w", clusterName, service, err)
	}
	d.Status = registry.DeploymentStatus(status)
	d.Kind = registry.DeploymentKind(kind)
	d.DeployedAt = d.DeployedAt.UTC()
	return d, nil
}

// ListDeployments returns every deployment for clusterName.
func (p *Provider) ListDeployments(ctx context.Context, clusterName string) ([]registry.Deployment, error) {
	const stmt = `
		SELECT cluster_name, service, version, deployed_at, deployed_by, status, kind
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
			kind   string
		)
		if err := rows.Scan(&d.ClusterName, &d.Service, &d.Version, &d.DeployedAt, &d.DeployedBy, &status, &kind); err != nil {
			return nil, fmt.Errorf("registry/sqlite: scan deployment: %w", err)
		}
		d.Status = registry.DeploymentStatus(status)
		d.Kind = registry.DeploymentKind(kind)
		d.DeployedAt = d.DeployedAt.UTC()
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry/sqlite: iterate deployments: %w", err)
	}
	return out, nil
}

// DeleteDeployment removes the deployments row for (clusterName, service).
// Deleting a non-existent row is a no-op.
func (p *Provider) DeleteDeployment(ctx context.Context, clusterName, service string) error {
	const stmt = `DELETE FROM deployments WHERE cluster_name = ? AND service = ?`
	if _, err := p.db.ExecContext(ctx, stmt, clusterName, service); err != nil {
		return fmt.Errorf("registry/sqlite: delete deployment %s/%s: %w", clusterName, service, err)
	}
	return nil
}

// AppendHistory records a single deployment attempt. Entries are
// append-only.
func (p *Provider) AppendHistory(ctx context.Context, e registry.DeploymentHistoryEntry) error {
	const stmt = `
		INSERT INTO deployment_history
			(cluster_name, service, version, attempted_at, status, rollout_duration_ms, error, kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`
	attemptedAt := nowIfZero(e.AttemptedAt).UTC()
	if _, err := p.db.ExecContext(ctx, stmt,
		e.ClusterName, e.Service, e.Version, attemptedAt, string(e.Status), e.RolloutDurationMs, e.Error, string(defaultKind(e.Kind)),
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

	q := "SELECT id, cluster_name, service, version, attempted_at, status, rollout_duration_ms, error, kind FROM deployment_history"
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
			kind   string
		)
		if err := rows.Scan(&e.ID, &e.ClusterName, &e.Service, &e.Version, &e.AttemptedAt, &status, &e.RolloutDurationMs, &e.Error, &kind); err != nil {
			return nil, fmt.Errorf("registry/sqlite: scan history: %w", err)
		}
		e.Status = registry.DeploymentStatus(status)
		e.Kind = registry.DeploymentKind(kind)
		e.AttemptedAt = e.AttemptedAt.UTC()
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

// defaultKind returns k if non-empty, otherwise registry.KindApp. This
// mirrors the SQL column DEFAULT 'app' so a Deployment value with the zero
// Kind round-trips as KindApp rather than the empty string.
func defaultKind(k registry.DeploymentKind) registry.DeploymentKind {
	if k == "" {
		return registry.KindApp
	}
	return k
}

// RecordResource inserts a new hetzner_resources row and returns the
// auto-generated id. CreatedAt defaults to the current UTC time when zero;
// DestroyedAt is always written as NULL on insert (use MarkResourceDestroyed
// to retire a row).
func (p *Provider) RecordResource(ctx context.Context, r registry.HetznerResource) (int64, error) {
	const stmt = `
		INSERT INTO hetzner_resources
			(cluster_name, resource_type, hetzner_id, hostname, created_at, destroyed_at, metadata)
		VALUES (?, ?, ?, ?, ?, NULL, ?)
	`
	createdAt := nowIfZero(r.CreatedAt).UTC()
	res, err := p.db.ExecContext(ctx, stmt,
		r.ClusterName,
		string(r.ResourceType),
		r.HetznerID,
		nullableString(r.Hostname),
		createdAt,
		nullableString(r.Metadata),
	)
	if err != nil {
		return 0, fmt.Errorf("registry/sqlite: record resource %s/%s: %w", r.ClusterName, r.ResourceType, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("registry/sqlite: record resource %s/%s last id: %w", r.ClusterName, r.ResourceType, err)
	}
	return id, nil
}

// MarkResourceDestroyed stamps destroyed_at on the inventory row with the
// given id. It is idempotent: rows whose destroyed_at is already non-NULL
// are left untouched, and an unknown id is a no-op (UPDATE matches zero
// rows). Stamping a non-existent id is therefore not an error.
func (p *Provider) MarkResourceDestroyed(ctx context.Context, id int64, at time.Time) error {
	const stmt = `UPDATE hetzner_resources SET destroyed_at = ? WHERE id = ? AND destroyed_at IS NULL`
	if _, err := p.db.ExecContext(ctx, stmt, at.UTC(), id); err != nil {
		return fmt.Errorf("registry/sqlite: mark resource destroyed id=%d: %w", id, err)
	}
	return nil
}

// ListResources returns inventory rows for clusterName. When
// includeDestroyed is false, only rows with destroyed_at IS NULL are
// returned. Rows are ordered by created_at, id ascending so callers see a
// stable creation timeline.
func (p *Provider) ListResources(ctx context.Context, clusterName string, includeDestroyed bool) ([]registry.HetznerResource, error) {
	q := `
		SELECT id, cluster_name, resource_type, hetzner_id, hostname, created_at, destroyed_at, metadata
		FROM hetzner_resources
		WHERE cluster_name = ?
	`
	if !includeDestroyed {
		q += ` AND destroyed_at IS NULL`
	}
	q += ` ORDER BY created_at ASC, id ASC`

	rows, err := p.db.QueryContext(ctx, q, clusterName)
	if err != nil {
		return nil, fmt.Errorf("registry/sqlite: list resources for %q: %w", clusterName, err)
	}
	defer rows.Close()

	return scanResources(rows)
}

// ListResourcesByType returns active (non-destroyed) inventory rows for
// clusterName narrowed to a single resource_type. resourceType is taken as
// a plain string so callers can pass a registry.HetznerResourceType cast or
// a literal interchangeably.
func (p *Provider) ListResourcesByType(ctx context.Context, clusterName, resourceType string) ([]registry.HetznerResource, error) {
	const q = `
		SELECT id, cluster_name, resource_type, hetzner_id, hostname, created_at, destroyed_at, metadata
		FROM hetzner_resources
		WHERE cluster_name = ? AND resource_type = ? AND destroyed_at IS NULL
		ORDER BY created_at ASC, id ASC
	`
	rows, err := p.db.QueryContext(ctx, q, clusterName, resourceType)
	if err != nil {
		return nil, fmt.Errorf("registry/sqlite: list resources for %q/%s: %w", clusterName, resourceType, err)
	}
	defer rows.Close()

	return scanResources(rows)
}

// MarkClusterDestroyed stamps clusters.destroyed_at without removing the
// row. It is idempotent: calling on an already-destroyed cluster preserves
// the existing tombstone, and calling on an unknown name is a no-op
// (UPDATE matches zero rows).
func (p *Provider) MarkClusterDestroyed(ctx context.Context, clusterName string, at time.Time) error {
	const stmt = `UPDATE clusters SET destroyed_at = ? WHERE name = ? AND destroyed_at IS NULL`
	if _, err := p.db.ExecContext(ctx, stmt, at.UTC(), clusterName); err != nil {
		return fmt.Errorf("registry/sqlite: mark cluster destroyed %q: %w", clusterName, err)
	}
	return nil
}

// scanResources consumes *sql.Rows already positioned over the canonical
// hetzner_resources column list and returns the materialised slice.
func scanResources(rows *sql.Rows) ([]registry.HetznerResource, error) {
	var out []registry.HetznerResource
	for rows.Next() {
		var (
			r            registry.HetznerResource
			resourceType string
			hostname     sql.NullString
			destroyedAt  sql.NullTime
			metadata     sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.ClusterName, &resourceType, &r.HetznerID, &hostname, &r.CreatedAt, &destroyedAt, &metadata); err != nil {
			return nil, fmt.Errorf("registry/sqlite: scan resource: %w", err)
		}
		r.ResourceType = registry.HetznerResourceType(resourceType)
		r.CreatedAt = r.CreatedAt.UTC()
		if hostname.Valid {
			r.Hostname = hostname.String
		}
		if destroyedAt.Valid {
			r.DestroyedAt = destroyedAt.Time.UTC()
		}
		if metadata.Valid {
			r.Metadata = metadata.String
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry/sqlite: iterate resources: %w", err)
	}
	return out, nil
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

// nullableString returns sql.NullString with .Valid=false when s is empty,
// so optional inventory columns persist NULL rather than the empty string.
func nullableString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
