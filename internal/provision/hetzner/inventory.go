package hetzner

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// HCloudResourceLister is the narrow read-side surface of the Hetzner
// Cloud API used by the reconciler. The real implementation wraps
// *hcloud.Client; tests inject an in-memory fake to assert behaviour
// without making network calls.
//
// Each method returns the labelled resources that match the cluster's
// label selector. Implementations are expected to honour
// LabelSelector(clusterName) so the reconciler only ever sees resources
// owned by the cluster.
type HCloudResourceLister interface {
	ListServers(ctx context.Context, clusterName string) ([]LabelledResource, error)
	ListLoadBalancers(ctx context.Context, clusterName string) ([]LabelledResource, error)
	ListSSHKeys(ctx context.Context, clusterName string) ([]LabelledResource, error)
	ListFirewalls(ctx context.Context, clusterName string) ([]LabelledResource, error)
	ListNetworks(ctx context.Context, clusterName string) ([]LabelledResource, error)
	ListVolumes(ctx context.Context, clusterName string) ([]LabelledResource, error)
	ListPrimaryIPs(ctx context.Context, clusterName string) ([]LabelledResource, error)
}

// LabelledResource is the reconciler's normalised view of a Hetzner
// Cloud resource: enough fields to write/update a cluster_resources row
// and to surface unmanaged resources on stderr.
//
// Labels is the full label map; the reconciler inspects it to confirm the
// resource carries managed-by=clusterbox and cluster-name=<name>.
type LabelledResource struct {
	ExternalID string
	Hostname   string
	Labels     map[string]string
}

// Summary is the outcome of one Reconcile call. Counts are aggregated
// across resource types; Unmanaged lists Hetzner ids of resources that
// exist in the cluster's project but lack the required labels and so
// cannot be tracked.
type Summary struct {
	// Added is the number of new hetzner_resources rows written.
	Added int
	// Existing is the number of rows already present that the reconciler
	// did not need to touch.
	Existing int
	// MarkedDestroyed is the number of inventory rows whose corresponding
	// Hetzner resource is no longer present and which were tombstoned.
	MarkedDestroyed int
	// Unmanaged lists Hetzner ids whose resources lack the required
	// labels. The reconciler never modifies these.
	Unmanaged []string
}

// Reconciler binds the registry and the Hetzner Cloud lister into the
// post-operation reconciliation pass. A zero-value Reconciler is not
// usable; callers populate Registry and Lister explicitly.
type Reconciler struct {
	Registry registry.Registry
	Lister   HCloudResourceLister
	// Now overrides time.Now in tests. Nil falls back to time.Now.
	Now func() time.Time
}

// NewHCloudLister wraps an hcloud.Client in the HCloudResourceLister
// interface used by the reconciler. The returned lister filters every
// listing by LabelSelector(clusterName) so callers only see resources
// belonging to the requested cluster.
func NewHCloudLister(client *hcloud.Client) HCloudResourceLister {
	return &hcloudLister{client: client}
}

// Reconcile compares the Hetzner side of the world against the registry
// inventory for clusterName and brings the registry into agreement with
// reality. It is idempotent: calling it twice in a row produces the same
// final state and the second call adds zero rows.
//
// The contract is strictly observational from the cloud side — Reconcile
// never deletes or mutates Hetzner resources. It only writes inventory
// rows and tombstones rows whose underlying resource has disappeared.
func (r *Reconciler) Reconcile(ctx context.Context, clusterName string) (Summary, error) {
	if r.Registry == nil {
		return Summary{}, fmt.Errorf("reconciler: registry is required")
	}
	if r.Lister == nil {
		return Summary{}, fmt.Errorf("reconciler: lister is required")
	}
	if clusterName == "" {
		return Summary{}, fmt.Errorf("reconciler: clusterName is required")
	}

	now := time.Now
	if r.Now != nil {
		now = r.Now
	}

	// Per-type fetch + reconcile. Each entry pairs the registry resource
	// type with the lister method that fetches the corresponding cloud
	// resources. Adding a new resource type is a one-line change here
	// plus an interface method on HCloudResourceLister.
	type fetcher struct {
		resourceType registry.ResourceType
		list         func(ctx context.Context, clusterName string) ([]LabelledResource, error)
	}
	fetchers := []fetcher{
		{registry.ResourceServer, r.Lister.ListServers},
		{registry.ResourceLoadBalancer, r.Lister.ListLoadBalancers},
		{registry.ResourceSSHKey, r.Lister.ListSSHKeys},
		{registry.ResourceFirewall, r.Lister.ListFirewalls},
		{registry.ResourceNetwork, r.Lister.ListNetworks},
		{registry.ResourceVolume, r.Lister.ListVolumes},
		{registry.ResourcePrimaryIP, r.Lister.ListPrimaryIPs},
	}

	var summary Summary

	for _, f := range fetchers {
		cloudList, err := f.list(ctx, clusterName)
		if err != nil {
			return summary, fmt.Errorf("reconciler: list %s: %w", f.resourceType, err)
		}

		// Index the registry's view of this resource type by ExternalID
		// for O(1) presence checks and tombstone lookups. The registry
		// is the smaller side here — clusters typically have a handful
		// of resources per type.
		regRows, err := r.Registry.ListResourcesByType(ctx, clusterName, string(f.resourceType))
		if err != nil {
			return summary, fmt.Errorf("reconciler: list registry %s: %w", f.resourceType, err)
		}
		regByHID := make(map[string]registry.ClusterResource, len(regRows))
		for _, row := range regRows {
			regByHID[row.ExternalID] = row
		}

		// Track which of the registry rows we observed in the cloud so
		// any leftovers can be tombstoned in pass two.
		seen := make(map[string]bool, len(cloudList))

		for _, lr := range cloudList {
			// Sanity: a labelled resource must carry both required
			// labels. The lister's selector should already guarantee
			// this, but if a stale or partial-label resource slips
			// through we surface it as unmanaged rather than recording
			// it.
			if !hasRequiredLabels(lr.Labels, clusterName) {
				summary.Unmanaged = append(summary.Unmanaged, lr.ExternalID)
				continue
			}

			seen[lr.ExternalID] = true
			if _, ok := regByHID[lr.ExternalID]; ok {
				summary.Existing++
				continue
			}

			if _, err := r.Registry.RecordResource(ctx, registry.ClusterResource{
				ClusterName:  clusterName,
				Provider:     registry.ProviderHetzner,
				ResourceType: f.resourceType,
				ExternalID:   lr.ExternalID,
				Hostname:     lr.Hostname,
				CreatedAt:    now().UTC(),
			}); err != nil {
				return summary, fmt.Errorf("reconciler: record %s/%s: %w", f.resourceType, lr.ExternalID, err)
			}
			summary.Added++
		}

		// Tombstone rows that are still active in the registry but have
		// disappeared from the cloud. MarkResourceDestroyed is
		// idempotent so re-running the reconciler is safe.
		for _, row := range regRows {
			if !row.DestroyedAt.IsZero() {
				continue
			}
			if seen[row.ExternalID] {
				continue
			}
			if err := r.Registry.MarkResourceDestroyed(ctx, row.ID, now().UTC()); err != nil {
				return summary, fmt.Errorf("reconciler: mark destroyed %s/%s: %w", f.resourceType, row.ExternalID, err)
			}
			summary.MarkedDestroyed++
		}
	}

	return summary, nil
}

// hasRequiredLabels confirms a labelled resource carries the two labels
// the reconciler relies on for ownership.
func hasRequiredLabels(labels map[string]string, clusterName string) bool {
	if labels[LabelManagedBy] != ManagedByValue {
		return false
	}
	if labels[LabelClusterName] != clusterName {
		return false
	}
	return true
}

// hcloudLister is the production HCloudResourceLister implementation. It
// proxies to *hcloud.Client and applies LabelSelector(clusterName) on
// every list call so callers cannot accidentally fetch foreign
// resources.
type hcloudLister struct {
	client *hcloud.Client
}

func (h *hcloudLister) selectorOpts(clusterName string) hcloud.ListOpts {
	return hcloud.ListOpts{LabelSelector: LabelSelector(clusterName)}
}

func (h *hcloudLister) ListServers(ctx context.Context, clusterName string) ([]LabelledResource, error) {
	res, err := h.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{ListOpts: h.selectorOpts(clusterName)})
	if err != nil {
		return nil, err
	}
	out := make([]LabelledResource, 0, len(res))
	for _, s := range res {
		out = append(out, LabelledResource{
			ExternalID: strconv.FormatInt(s.ID, 10),
			Hostname:   s.Name,
			Labels:     s.Labels,
		})
	}
	return out, nil
}

func (h *hcloudLister) ListLoadBalancers(ctx context.Context, clusterName string) ([]LabelledResource, error) {
	res, err := h.client.LoadBalancer.AllWithOpts(ctx, hcloud.LoadBalancerListOpts{ListOpts: h.selectorOpts(clusterName)})
	if err != nil {
		return nil, err
	}
	out := make([]LabelledResource, 0, len(res))
	for _, lb := range res {
		out = append(out, LabelledResource{
			ExternalID: strconv.FormatInt(lb.ID, 10),
			Hostname:   lb.Name,
			Labels:     lb.Labels,
		})
	}
	return out, nil
}

func (h *hcloudLister) ListSSHKeys(ctx context.Context, clusterName string) ([]LabelledResource, error) {
	res, err := h.client.SSHKey.AllWithOpts(ctx, hcloud.SSHKeyListOpts{ListOpts: h.selectorOpts(clusterName)})
	if err != nil {
		return nil, err
	}
	out := make([]LabelledResource, 0, len(res))
	for _, k := range res {
		out = append(out, LabelledResource{
			ExternalID: strconv.FormatInt(k.ID, 10),
			Hostname:   k.Name,
			Labels:     k.Labels,
		})
	}
	return out, nil
}

func (h *hcloudLister) ListFirewalls(ctx context.Context, clusterName string) ([]LabelledResource, error) {
	res, err := h.client.Firewall.AllWithOpts(ctx, hcloud.FirewallListOpts{ListOpts: h.selectorOpts(clusterName)})
	if err != nil {
		return nil, err
	}
	out := make([]LabelledResource, 0, len(res))
	for _, fw := range res {
		out = append(out, LabelledResource{
			ExternalID: strconv.FormatInt(fw.ID, 10),
			Hostname:   fw.Name,
			Labels:     fw.Labels,
		})
	}
	return out, nil
}

func (h *hcloudLister) ListNetworks(ctx context.Context, clusterName string) ([]LabelledResource, error) {
	res, err := h.client.Network.AllWithOpts(ctx, hcloud.NetworkListOpts{ListOpts: h.selectorOpts(clusterName)})
	if err != nil {
		return nil, err
	}
	out := make([]LabelledResource, 0, len(res))
	for _, n := range res {
		out = append(out, LabelledResource{
			ExternalID: strconv.FormatInt(n.ID, 10),
			Hostname:   n.Name,
			Labels:     n.Labels,
		})
	}
	return out, nil
}

func (h *hcloudLister) ListVolumes(ctx context.Context, clusterName string) ([]LabelledResource, error) {
	res, err := h.client.Volume.AllWithOpts(ctx, hcloud.VolumeListOpts{ListOpts: h.selectorOpts(clusterName)})
	if err != nil {
		return nil, err
	}
	out := make([]LabelledResource, 0, len(res))
	for _, v := range res {
		out = append(out, LabelledResource{
			ExternalID: strconv.FormatInt(v.ID, 10),
			Hostname:   v.Name,
			Labels:     v.Labels,
		})
	}
	return out, nil
}

func (h *hcloudLister) ListPrimaryIPs(ctx context.Context, clusterName string) ([]LabelledResource, error) {
	res, err := h.client.PrimaryIP.AllWithOpts(ctx, hcloud.PrimaryIPListOpts{ListOpts: h.selectorOpts(clusterName)})
	if err != nil {
		return nil, err
	}
	out := make([]LabelledResource, 0, len(res))
	for _, p := range res {
		out = append(out, LabelledResource{
			ExternalID: strconv.FormatInt(p.ID, 10),
			Hostname:   p.Name,
			Labels:     p.Labels,
		})
	}
	return out, nil
}
