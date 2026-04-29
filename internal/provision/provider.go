package provision

import (
	"context"
	"fmt"

	"github.com/foundryfabric/clusterbox/internal/registry"
)

// Provider is the cloud-agnostic abstraction over cluster provisioning.
//
// A Provider knows how to stand up, observe, and tear down the
// infrastructure required to host a clusterbox-managed cluster on a
// specific substrate (Hetzner Cloud today; future providers will cover
// baremetal hosts and other clouds).
//
// Implementations MUST NOT leak provider-specific concepts (Hetzner
// labels, Pulumi stacks, hcloud SDKs, ...) through this surface. Such
// details belong inside the implementation package; callers here
// dispatch through the interface and treat each provider as opaque.
type Provider interface {
	// Name returns the canonical provider identifier (e.g. "hetzner").
	// The returned value is the same string accepted by the
	// `--provider` CLI flag and persisted in registry rows so
	// downstream commands can pick the right provider on follow-up
	// operations.
	Name() string

	// Provision stands up a fresh cluster from cfg. It is idempotent:
	// re-running with the same cfg converges to the same end state and
	// does not duplicate resources.
	//
	// On success the returned ProvisionResult carries the kubeconfig
	// path written for the cluster, the registry-shaped Node rows the
	// caller should record, and any provider-specific resource handles
	// the caller may need (e.g. HetznerLB for Hetzner). Non-Hetzner
	// providers leave HetznerLB nil.
	Provision(ctx context.Context, cfg ClusterConfig) (ProvisionResult, error)

	// Destroy tears down every resource the provider created for the
	// given cluster. It is idempotent: a re-run after partial failure
	// (or against a cluster that has already been destroyed) converges
	// without error.
	//
	// The cluster argument carries the registry row recorded at
	// Provision time, including the provider name, region, and
	// kubeconfig path. Implementations rely on the registry-tracked
	// inventory rather than re-discovering resources from scratch.
	Destroy(ctx context.Context, cluster registry.Cluster) error

	// Reconcile walks the provider's view of the cluster and brings
	// the local registry into agreement with reality. It is strictly
	// observational — Reconcile never mutates provider-side state.
	//
	// The shape of the walk is provider-specific: Hetzner inspects
	// Pulumi outputs and the hcloud API; future baremetal providers
	// will walk via clusterboxnode. The summary surfaces aggregated
	// counts plus the names of any provider-side resources that exist
	// but are not tracked.
	Reconcile(ctx context.Context, clusterName string) (ReconcileSummary, error)

	// AddNode provisions and joins a single worker node to clusterName.
	// It returns the canonical node hostname on success.
	// Implementations that do not support worker nodes should return
	// ErrAddNodeNotSupported.
	AddNode(ctx context.Context, clusterName string) (string, error)

	// RemoveNode tears down the infrastructure for a single worker node
	// (the named nodeName) after it has already been drained and removed
	// from Kubernetes by the cmd layer.
	// Implementations that do not support worker-node removal should
	// return ErrRemoveNodeNotSupported.
	RemoveNode(ctx context.Context, clusterName, nodeName string) error
}

// ErrAddNodeNotSupported is returned by Provider.AddNode when the provider
// does not support adding worker nodes.
var ErrAddNodeNotSupported = fmt.Errorf("provider does not support add-node")

// ErrRemoveNodeNotSupported is returned by Provider.RemoveNode when the
// provider does not support removing individual worker nodes.
var ErrRemoveNodeNotSupported = fmt.Errorf("provider does not support remove-node")

// ProvisionResult is the cloud-agnostic outcome of a successful
// Provider.Provision call.
//
// Fields that do not apply to a given provider are left at their zero
// value. HetznerLB is the only provider-specific handle the caller
// needs to thread through the registry today; future providers will
// add their own optional fields rather than reusing this one.
type ProvisionResult struct {
	// KubeconfigPath is the on-disk path to the cluster's kubeconfig.
	KubeconfigPath string

	// Nodes lists the registry-shaped node records for every host the
	// provider stood up, in role order: index 0 is the control-plane,
	// the rest are workers. Callers persist these via
	// registry.Registry.UpsertNode.
	Nodes []registry.Node

	// HetznerLB is non-nil only when the provider is Hetzner Cloud
	// AND a load-balancer was created as part of provisioning. Other
	// providers leave this field nil.
	HetznerLB *registry.ClusterResource
}

// ReconcileSummary is the cloud-agnostic outcome of a single
// Provider.Reconcile pass. The shape mirrors what the existing Hetzner
// reconciler emits so the dispatch layer can render summaries
// uniformly across providers.
type ReconcileSummary struct {
	// Added is the number of new inventory rows written.
	Added int
	// Existing is the number of rows already present that the
	// reconciler did not need to touch.
	Existing int
	// MarkedDestroyed is the number of inventory rows whose
	// corresponding provider-side resource is no longer present and
	// which were tombstoned.
	MarkedDestroyed int
	// Unmanaged lists provider-side resource ids that exist in the
	// cluster's namespace but lack the labels/metadata required for
	// tracking. The reconciler never modifies these.
	Unmanaged []string
}
