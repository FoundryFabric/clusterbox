// Package provision is the cloud-agnostic provisioning surface for
// clusterbox. It defines the Provider interface that cmd-side
// dispatchers consume and the shared ClusterConfig / ProvisionResult /
// ReconcileSummary types every provider speaks.
//
// Concrete implementations live in subpackages: internal/provision/hetzner
// today, with baremetal and other clouds to follow.
package provision

// ClusterConfig holds all inputs required to provision a single
// cluster. Generic fields (ClusterName, Location, DNSDomain,
// ResourceRole, ClusterLabel) apply to every provider; the Tailscale
// and SnapshotName fields are Hetzner-specific knobs that travel here
// today for backward compatibility with add-node / remove-node and
// will move into a per-provider config in a follow-up task once
// non-Hetzner providers exist.
type ClusterConfig struct {
	// ClusterName is the human-readable name for the cluster. Used
	// as a name prefix for all provider-side resources and as the
	// hostname the bootstrap step targets.
	ClusterName string

	// SnapshotName is the exact name of the boot image to provision
	// from. Hetzner-specific today (it is the name of a hcloud
	// snapshot); future providers will read it from a per-provider
	// config.
	SnapshotName string

	// Location is the provider's region/datacenter code, e.g. "ash"
	// or "nbg1" for Hetzner.
	Location string

	// DNSDomain is the DNS zone name to create the A record in,
	// e.g. "example.com". The record name is set to ClusterName,
	// yielding <ClusterName>.<DNSDomain>.
	DNSDomain string

	// TailscaleClientID and TailscaleClientSecret are the OAuth
	// client credentials used to generate an ephemeral Tailscale
	// auth key. Hetzner-specific today (Tailscale activates at
	// first-boot via cloud-init); future providers may use a
	// different VPN bootstrap and read these from a per-provider
	// config. These values are never written to any log.
	TailscaleClientID     string
	TailscaleClientSecret string

	// ResourceRole is the semantic role recorded in the
	// resource-role label of the cluster's primary VM (e.g.
	// "control-plane" for the initial node, "worker" for an added
	// node). When empty the role label is omitted; subordinate
	// resources (firewall, volume) carry their own static role
	// labels.
	ResourceRole string

	// ClusterLabel is the value written to the cluster-name label
	// on every resource. It defaults to ClusterName when empty.
	// add-node uses this to record the parent cluster while
	// ClusterName carries the per-node resource name prefix.
	ClusterLabel string
}

// EffectiveClusterLabel returns the value to write to the cluster-name
// label, falling back to ClusterName when ClusterLabel is unset.
func (c ClusterConfig) EffectiveClusterLabel() string {
	if c.ClusterLabel != "" {
		return c.ClusterLabel
	}
	return c.ClusterName
}
