// Package provision provides a Pulumi stack that provisions a FoundryFabric
// clusterbox on Hetzner Cloud: a CX42 VM, a 100 GB data volume, a firewall
// (443 + Tailscale UDP inbound; port 22 blocked from public internet), and
// a DNS A record in the target zone.
package provision

// ClusterConfig holds all inputs required to provision a single cluster.
type ClusterConfig struct {
	// ClusterName is the human-readable name for the cluster. Used as a name
	// prefix for all Hetzner resources and as the Tailscale hostname.
	ClusterName string

	// SnapshotName is the exact name of the Hetzner snapshot to boot from,
	// e.g. "clusterbox-base-v0.1.0".
	SnapshotName string

	// Location is the Hetzner datacenter location code, e.g. "nbg1" or "fsn1".
	Location string

	// DNSDomain is the DNS zone name to create the A record in, e.g. "example.com".
	// The record name is set to ClusterName, yielding <ClusterName>.<DNSDomain>.
	DNSDomain string

	// TailscaleClientID and TailscaleClientSecret are the OAuth client
	// credentials used to generate an ephemeral Tailscale auth key via the
	// internal/tailscale client. These values are never written to any log.
	TailscaleClientID     string
	TailscaleClientSecret string
}
