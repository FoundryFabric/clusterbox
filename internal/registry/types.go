package registry

import "time"

// DeploymentStatus is a named-string enum describing the terminal or in-flight
// state of a service deployment recorded in the registry.
type DeploymentStatus string

const (
	// StatusRolledOut indicates the deployment finished successfully and the
	// target version is live on every node in the cluster.
	StatusRolledOut DeploymentStatus = "rolled_out"

	// StatusFailed indicates the deployment was attempted but did not reach the
	// rolled-out state. The history entry will carry the error detail.
	StatusFailed DeploymentStatus = "failed"

	// StatusRolling indicates the deployment is currently in progress.
	StatusRolling DeploymentStatus = "rolling"
)

// Cluster is the registry record describing a logical cluster of nodes that
// run one or more services together.
type Cluster struct {
	Name       string
	Provider   string
	Region     string
	Env        string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastSynced time.Time
}

// Node is the registry record for a single host that participates in a
// cluster. Hostname is unique within a cluster.
type Node struct {
	ClusterName string
	Hostname    string
	Address     string
	Roles       []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Deployment is the most recent known deployment of a service onto a cluster.
// Exactly one row exists per (ClusterName, Service) pair; AppendHistory
// preserves the audit trail of past deployments.
type Deployment struct {
	ClusterName string
	Service     string
	Version     string
	Status      DeploymentStatus
	UpdatedAt   time.Time
}

// DeploymentHistoryEntry records a single deployment attempt against a
// cluster. Entries are append-only and queried via ListHistory.
type DeploymentHistoryEntry struct {
	ClusterName string
	Service     string
	Version     string
	Status      DeploymentStatus
	Detail      string
	OccurredAt  time.Time
}

// HistoryFilter narrows the result set returned by Registry.ListHistory.
// Empty string fields match any value; Limit of zero means no limit.
type HistoryFilter struct {
	ClusterName string
	Service     string
	Limit       int
}
