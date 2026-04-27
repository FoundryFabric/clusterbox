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

	// StatusUninstalled indicates a previously rolled-out addon (or other
	// deployment kind) was removed from the cluster. The deployments row
	// itself is deleted on uninstall; this status appears only on
	// deployment_history rows so the audit trail captures the removal.
	StatusUninstalled DeploymentStatus = "uninstalled"
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
	// ID is the surrogate integer primary key assigned by the database. It
	// is zero until the cluster is persisted. Each cluster lifetime gets a
	// unique ID, so a destroyed cluster and a new cluster sharing the same
	// Name will have different IDs.
	ID             int64  `json:"id"`
	Name           string
	Provider       string
	Region         string
	Env            string
	CreatedAt      time.Time
	KubeconfigPath string
	LastSynced     time.Time
	// DestroyedAt records when the cluster was torn down. The zero value
	// means the cluster is still active.
	DestroyedAt time.Time
}

// HetznerResourceType is a named-string enum identifying the kind of Hetzner
// Cloud resource recorded in the inventory table.
type HetznerResourceType string

const (
	// ResourceServer is a Hetzner Cloud server (cx-/ax- compute instance).
	ResourceServer HetznerResourceType = "server"
	// ResourceLoadBalancer is a Hetzner Cloud load balancer.
	ResourceLoadBalancer HetznerResourceType = "load_balancer"
	// ResourceSSHKey is an SSH public key uploaded to Hetzner Cloud.
	ResourceSSHKey HetznerResourceType = "ssh_key"
	// ResourceFirewall is a Hetzner Cloud firewall.
	ResourceFirewall HetznerResourceType = "firewall"
	// ResourceNetwork is a Hetzner Cloud private network.
	ResourceNetwork HetznerResourceType = "network"
	// ResourceVolume is a Hetzner Cloud block storage volume.
	ResourceVolume HetznerResourceType = "volume"
	// ResourcePrimaryIP is a Hetzner Cloud primary IP.
	ResourcePrimaryIP HetznerResourceType = "primary_ip"
	// ResourceTailscaleDevice is a Tailscale device (tracked alongside
	// Hetzner-side resources because clusterbox provisions them in lockstep).
	ResourceTailscaleDevice HetznerResourceType = "tailscale_device"
)

// HetznerResource is one row in the hetzner_resources inventory: a single
// cloud-side object that clusterbox created on behalf of a cluster.
//
// HetznerID is the provider-side identifier (server ID, load-balancer ID,
// SSH-key ID, etc.) stored as a string so non-numeric IDs (e.g. Tailscale
// node IDs) round-trip cleanly.
//
// DestroyedAt is the zero value while the resource is still live. Callers
// stamp it via MarkResourceDestroyed when the resource is torn down; the row
// is retained for audit rather than deleted.
//
// Metadata is opaque JSON text; helpers to marshal/unmarshal will follow.
type HetznerResource struct {
	ID           int64
	ClusterName  string
	ResourceType HetznerResourceType
	HetznerID    string
	Hostname     string
	CreatedAt    time.Time
	DestroyedAt  time.Time
	Metadata     string
}

// Node is the registry record for a single host that participates in a
// cluster. Hostname is unique within a cluster.
//
// Arch, OSVersion, K3sVersion, AgentVersion, and LastInspectedAt are
// populated by the inspection step of `clusterbox sync` / `clusterbox up`
// and are nullable in the underlying schema. The zero value (empty string,
// or zero time) means the field has not yet been observed for this node.
//
// AgentVersion records the clusterbox release that deployed the
// clusterboxnode agent currently running on this host. K3sVersion records
// the k3s release installed on the host.
type Node struct {
	ClusterName     string
	Hostname        string
	Role            string
	JoinedAt        time.Time
	Arch            string
	OSVersion       string
	K3sVersion      string
	AgentVersion    string
	LastInspectedAt time.Time
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
