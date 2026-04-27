// Package registry is the persistent record of clusters, nodes, deployments,
// and deployment history that clusterbox commands read and write.
//
// The package defines a backend-agnostic Registry interface; concrete
// implementations (e.g. SQLite) live in subpackages and are selected via
// NewRegistry, which dispatches on the REGISTRY_BACKEND environment variable.
//
// All Registry methods accept a context.Context and return ErrNotFound (or an
// error wrapping it) when a requested record does not exist.
package registry

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned (possibly wrapped) by Get* methods when the
// requested record does not exist. Callers should use errors.Is to detect it.
var ErrNotFound = errors.New("registry: not found")

// Registry is the storage interface for clusterbox's local state. All methods
// are safe to call concurrently from multiple goroutines.
type Registry interface {
	// UpsertCluster inserts the cluster if absent, or updates it in place
	// using Name as the key.
	UpsertCluster(ctx context.Context, c Cluster) error

	// GetCluster returns the cluster with the given name, or ErrNotFound.
	GetCluster(ctx context.Context, name string) (Cluster, error)

	// ListClusters returns every active (non-destroyed) cluster in unspecified
	// order. Clusters whose DestroyedAt is non-zero are excluded; use
	// GetCluster to fetch a specific cluster regardless of its destroyed state.
	ListClusters(ctx context.Context) ([]Cluster, error)

	// DeleteCluster removes a cluster and any rows that reference it
	// (nodes, deployments, history). It is not an error to delete a cluster
	// that does not exist.
	DeleteCluster(ctx context.Context, name string) error

	// UpsertNode inserts or updates a node identified by
	// (ClusterName, Hostname).
	UpsertNode(ctx context.Context, n Node) error

	// RemoveNode deletes the node identified by (clusterName, hostname).
	// It is not an error to remove a node that does not exist.
	RemoveNode(ctx context.Context, clusterName, hostname string) error

	// ListNodes returns every node belonging to clusterName in unspecified
	// order.
	ListNodes(ctx context.Context, clusterName string) ([]Node, error)

	// UpsertDeployment inserts or updates the deployment row identified by
	// (ClusterName, Service).
	UpsertDeployment(ctx context.Context, d Deployment) error

	// GetDeployment returns the deployment for (clusterName, service), or
	// ErrNotFound.
	GetDeployment(ctx context.Context, clusterName, service string) (Deployment, error)

	// ListDeployments returns every deployment for the given cluster in
	// unspecified order.
	ListDeployments(ctx context.Context, clusterName string) ([]Deployment, error)

	// DeleteDeployment removes the deployments row for (clusterName, service).
	// Deleting a non-existent row is not an error. History rows are not
	// touched; callers that want an audit trail of the removal should
	// AppendHistory before or after calling DeleteDeployment.
	DeleteDeployment(ctx context.Context, clusterName, service string) error

	// AppendHistory records a single deployment attempt. Entries are
	// append-only.
	AppendHistory(ctx context.Context, e DeploymentHistoryEntry) error

	// ListHistory returns history entries matching filter. Results are
	// ordered most-recent-first.
	ListHistory(ctx context.Context, filter HistoryFilter) ([]DeploymentHistoryEntry, error)

	// MarkSynced records that clusterName was successfully reconciled with
	// its remote nodes at the given time.
	MarkSynced(ctx context.Context, clusterName string, at time.Time) error

	// RecordResource inserts a Hetzner-side resource into the inventory
	// and returns the generated row id. CreatedAt defaults to the current
	// UTC time when the caller leaves it zero. The row's id is also
	// available via subsequent ListResources/ListResourcesByType calls.
	RecordResource(ctx context.Context, r HetznerResource) (int64, error)

	// MarkResourceDestroyed stamps destroyed_at on the row with the given
	// id. It is idempotent: stamping an already-destroyed row is a no-op
	// (the existing destroyed_at is preserved). Stamping a non-existent id
	// is also a no-op.
	MarkResourceDestroyed(ctx context.Context, id int64, at time.Time) error

	// ListResources returns inventory rows for clusterName. When
	// includeDestroyed is false, rows with a non-NULL destroyed_at are
	// filtered out.
	ListResources(ctx context.Context, clusterName string, includeDestroyed bool) ([]HetznerResource, error)

	// ListResourcesByType returns active (non-destroyed) inventory rows
	// for clusterName narrowed to a single resource_type.
	ListResourcesByType(ctx context.Context, clusterName, resourceType string) ([]HetznerResource, error)

	// MarkClusterDestroyed stamps clusters.destroyed_at without removing
	// the row, preserving the cluster's audit trail. Callers that want to
	// physically remove a cluster (and cascade-delete its rows) should use
	// DeleteCluster instead.
	MarkClusterDestroyed(ctx context.Context, clusterName string, at time.Time) error

	// Close releases backend resources. After Close, no other method may be
	// called.
	Close() error
}
