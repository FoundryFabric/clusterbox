// Package provision label helpers.
//
// POLICY: Every Hetzner Cloud resource created by clusterbox MUST attach
// the `managed-by=clusterbox` and `cluster-name=<name>` labels at creation
// time. The post-operation reconciler in inventory.go relies on these
// labels to track the resource in the registry. Resources missing these
// labels will not be tracked and will be flagged as "unmanaged" on
// destroy.
package provision

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

// Standard label keys attached to every clusterbox-managed resource.
const (
	// LabelManagedBy identifies clusterbox as the controlling tool.
	LabelManagedBy = "managed-by"
	// LabelClusterName scopes a resource to a particular cluster.
	LabelClusterName = "cluster-name"
	// LabelResourceRole records the semantic role of the resource within
	// the cluster (e.g. "control-plane", "worker", "ingress-lb",
	// "ssh-bootstrap").
	LabelResourceRole = "resource-role"

	// ManagedByValue is the canonical value of the managed-by label.
	ManagedByValue = "clusterbox"
)

// StandardLabels returns the canonical Go-side label map for a
// clusterbox-managed Hetzner resource. The returned map is a fresh copy;
// callers may safely mutate it.
//
// resourceRole is the semantic role of the resource (e.g. "control-plane",
// "worker", "ingress-lb"). When empty the role label is omitted so callers
// that haven't yet classified a resource don't accidentally pin it to an
// incorrect role.
func StandardLabels(clusterName, resourceRole string) map[string]string {
	out := map[string]string{
		LabelManagedBy:   ManagedByValue,
		LabelClusterName: clusterName,
	}
	if resourceRole != "" {
		out[LabelResourceRole] = resourceRole
	}
	return out
}

// PulumiLabels is the Pulumi-input flavour of StandardLabels: the same
// canonical map expressed as pulumi.StringMap so it can be assigned
// directly to a hcloud.*Args.Labels field.
func PulumiLabels(clusterName, resourceRole string) pulumi.StringMap {
	out := pulumi.StringMap{
		LabelManagedBy:   pulumi.String(ManagedByValue),
		LabelClusterName: pulumi.String(clusterName),
	}
	if resourceRole != "" {
		out[LabelResourceRole] = pulumi.String(resourceRole)
	}
	return out
}

// LabelSelector returns the Hetzner Cloud API label selector matching every
// resource that carries the standard managed-by=clusterbox and
// cluster-name=<name> labels. It is used by the reconciler to query
// labelled resources for a single cluster.
func LabelSelector(clusterName string) string {
	return LabelManagedBy + "=" + ManagedByValue + "," + LabelClusterName + "=" + clusterName
}
