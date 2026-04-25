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

// DeploymentKind is a named-string enum describing the broad category of a
// deployment recorded in the registry. The default is KindApp; addons and
// system components are tagged so list/status views can distinguish them
// without a separate table.
type DeploymentKind string

const (
	// KindApp denotes a normal user-facing application deployment. This is
	// the default kind applied to rows that pre-date the kind column and to
	// any Deployment value that leaves Kind unset.
	KindApp DeploymentKind = "app"

	// KindAddon denotes a cluster addon (e.g. ingress controller, cert
	// manager) installed by clusterbox on behalf of the operator.
	KindAddon DeploymentKind = "addon"

	// KindSystem denotes an internal system component installed and
	// managed by clusterbox itself.
	KindSystem DeploymentKind = "system"
)

// Cluster is the registry record describing a logical cluster of nodes that
// run one or more services together.
type Cluster struct {
	Name           string
	Provider       string
	Region         string
	Env            string
	CreatedAt      time.Time
	KubeconfigPath string
	LastSynced     time.Time
}

// Node is the registry record for a single host that participates in a
// cluster. Hostname is unique within a cluster.
type Node struct {
	ClusterName string
	Hostname    string
	Role        string
	JoinedAt    time.Time
}

// Deployment is the most recent known deployment of a service onto a cluster.
// Exactly one row exists per (ClusterName, Service) pair; AppendHistory
// preserves the audit trail of past deployments.
//
// Kind defaults to KindApp when left as the zero value, matching the SQL
// column default applied to rows written before the kind column existed.
type Deployment struct {
	ClusterName string
	Service     string
	Version     string
	DeployedAt  time.Time
	DeployedBy  string
	Status      DeploymentStatus
	Kind        DeploymentKind
}

// DeploymentHistoryEntry records a single deployment attempt against a
// cluster. Entries are append-only and queried via ListHistory.
//
// Kind defaults to KindApp when left as the zero value, matching the SQL
// column default applied to rows written before the kind column existed.
type DeploymentHistoryEntry struct {
	ID                int64
	ClusterName       string
	Service           string
	Version           string
	AttemptedAt       time.Time
	Status            DeploymentStatus
	RolloutDurationMs int64
	Error             string
	Kind              DeploymentKind
}

// HistoryFilter narrows the result set returned by Registry.ListHistory.
// Empty string fields match any value; Limit of zero means no limit.
type HistoryFilter struct {
	ClusterName string
	Service     string
	Limit       int
}
