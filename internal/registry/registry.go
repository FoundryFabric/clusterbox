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

	// ListClusters returns every known cluster in unspecified order.
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

	// AppendHistory records a single deployment attempt. Entries are
	// append-only.
	AppendHistory(ctx context.Context, e DeploymentHistoryEntry) error

	// ListHistory returns history entries matching filter. Results are
	// ordered most-recent-first.
	ListHistory(ctx context.Context, filter HistoryFilter) ([]DeploymentHistoryEntry, error)

	// MarkSynced records that clusterName was successfully reconciled with
	// its remote nodes at the given time.
	MarkSynced(ctx context.Context, clusterName string, at time.Time) error

	// Close releases backend resources. After Close, no other method may be
	// called.
	Close() error
}
