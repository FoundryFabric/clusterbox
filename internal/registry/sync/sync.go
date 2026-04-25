// Package sync reconciles the local clusterbox registry with the two systems
// of record: Pulumi (for nodes) and kubectl (for service deployments).
//
// The reconciliation logic is encapsulated in the Reconciler struct, which
// depends on two small interfaces — PulumiClient and KubectlRunner — so it
// can be exercised exhaustively under test without touching real
// infrastructure. The cmd/sync.go command wires the production
// implementations and invokes Reconcile; cmd/diff.go reuses the same logic in
// dry-run mode (Options.DryRun = true) so its diff output is guaranteed to
// match what sync would do.
//
// All registry mutations are performed inside Reconcile; callers are expected
// to open and close the registry themselves so the sync package does not
// own a process-wide handle.
package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/foundryfabric/clusterbox/internal/registry"
)

// PulumiNode is the minimal description of a node derived from a Pulumi
// stack. The sync package does not care about the underlying provider
// resources; it only needs the hostname (which is also the Tailscale name)
// and the role.
type PulumiNode struct {
	// Hostname is the unique node identifier within a cluster. For the
	// control-plane node it equals the cluster name; for workers it follows
	// the convention chosen by add-node (e.g. "<cluster>-node").
	Hostname string

	// Role is one of "control-plane" or "worker".
	Role string
}

// PulumiClient lists nodes that Pulumi knows about for a given cluster. The
// production implementation queries the Pulumi auto API over both the
// "clusterbox" project (which holds the control-plane stack) and the
// per-cluster project (which holds worker stacks), but Reconciler does not
// depend on those details.
//
// ListClusterNodes returns ErrStackNotFound when no Pulumi stacks exist for
// the cluster at all. This is treated as drift (registry has the cluster but
// Pulumi does not) rather than as a fatal error, so the warning vs. delete
// behaviour can be controlled by --prune at the command layer.
type PulumiClient interface {
	ListClusterNodes(ctx context.Context, clusterName string) ([]PulumiNode, error)
}

// KubectlRunner shells out to kubectl. The signature mirrors the existing
// secrets.CommandRunner used elsewhere in the codebase, scoped to the kubectl
// invocations sync needs.
type KubectlRunner interface {
	// Run invokes kubectl with the given kubeconfig and args, returning
	// stdout. Stderr is expected to be merged into the returned error on
	// non-zero exit so callers can inspect it.
	Run(ctx context.Context, kubeconfig string, args ...string) ([]byte, error)
}

// ErrStackNotFound is returned (possibly wrapped) by PulumiClient
// implementations when no Pulumi stacks exist for the queried cluster. It is
// surfaced as drift rather than an error.
var ErrStackNotFound = fmt.Errorf("sync: pulumi stack not found")

// Options controls per-reconcile behaviour.
type Options struct {
	// DryRun, when true, makes Reconcile compute the diff and produce a
	// Summary but skip every registry write. The diff command (T10) sets
	// this to true to share the implementation.
	DryRun bool

	// Prune, when true, deletes registry rows for clusters/nodes/services
	// that are absent from Pulumi/kubectl. The default is to retain those
	// rows and only print a warning, so an outage of the upstream API
	// cannot silently destroy local audit history.
	Prune bool
}

// Summary is the aggregate result of one reconcile invocation across one or
// more clusters.
type Summary struct {
	// Clusters is the count of clusters reconciled.
	Clusters int

	// ServicesAdded counts deployments inserted because they exist in
	// kubectl but not in the registry.
	ServicesAdded int

	// ServicesUpdated counts deployments whose row was rewritten because
	// the Version, Status, or DeployedAt values diverged from kubectl.
	ServicesUpdated int

	// DriftItems counts entries that were flagged but not deleted (both
	// missing-from-pulumi and missing-from-kubectl rows). When Prune is
	// true the same divergences are deleted instead of flagged and are not
	// counted here.
	DriftItems int

	// NodesUpserted counts node rows written, regardless of whether the
	// row already existed.
	NodesUpserted int

	// NodesRemoved counts node rows deleted because Pulumi no longer knows
	// about them. This always runs (nodes are owned by Pulumi) and is not
	// gated on Prune.
	NodesRemoved int
}

// Reconciler is the dependency-injected unit of work. Construct one per
// command invocation; all fields except Warn must be non-nil.
type Reconciler struct {
	// Registry is the local cache to reconcile. Required.
	Registry registry.Registry

	// Pulumi lists the nodes Pulumi believes to exist. Required.
	Pulumi PulumiClient

	// Kubectl runs kubectl commands. Required.
	Kubectl KubectlRunner

	// Now returns the current time. Defaults to time.Now when nil; tests
	// inject a fixed clock to assert MarkSynced uses the right timestamp.
	Now func() time.Time

	// Warn is the destination for drift warnings. Defaults to
	// io.Discard-equivalent (no output) when nil. cmd/sync.go points it at
	// os.Stderr; tests capture it for assertions.
	Warn io.Writer
}

// Reconcile runs reconciliation against every cluster in the registry, or
// against the single named cluster when clusterName is non-empty. The
// returned Summary aggregates the per-cluster counts. An error is returned
// only for failures that prevent reconciliation from making any meaningful
// statement about the registry (e.g. ListClusters failed). Per-cluster
// failures are written to Warn and counted against DriftItems but do not
// abort the run, so a single broken cluster cannot block the others.
//
// last_synced_at is updated only for clusters whose reconcile completed
// without an error, so a partially-failed cluster keeps its previous synced
// timestamp.
func (r *Reconciler) Reconcile(ctx context.Context, clusterName string, opts Options) (Summary, error) {
	if r.Registry == nil || r.Pulumi == nil || r.Kubectl == nil {
		return Summary{}, fmt.Errorf("sync: reconciler missing required dependency")
	}
	now := r.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	clusters, err := r.selectClusters(ctx, clusterName)
	if err != nil {
		return Summary{}, err
	}

	var summary Summary
	for _, c := range clusters {
		summary.Clusters++
		ok := r.reconcileOne(ctx, c, opts, &summary)
		if ok && !opts.DryRun {
			if err := r.Registry.MarkSynced(ctx, c.Name, now()); err != nil {
				r.warnf("cluster %q: mark synced failed: %v", c.Name, err)
			}
		}
	}
	return summary, nil
}

// selectClusters returns either every registered cluster or the single one
// named, depending on clusterName.
func (r *Reconciler) selectClusters(ctx context.Context, clusterName string) ([]registry.Cluster, error) {
	if clusterName == "" {
		clusters, err := r.Registry.ListClusters(ctx)
		if err != nil {
			return nil, fmt.Errorf("sync: list clusters: %w", err)
		}
		return clusters, nil
	}
	c, err := r.Registry.GetCluster(ctx, clusterName)
	if err != nil {
		return nil, fmt.Errorf("sync: get cluster %q: %w", clusterName, err)
	}
	return []registry.Cluster{c}, nil
}

// reconcileOne reconciles a single cluster. Returns true when the cluster's
// reconcile completed cleanly enough that last_synced_at should be advanced.
// A return of false means at least one phase failed and was logged via Warn;
// the caller should leave the cluster's previous LastSynced untouched.
func (r *Reconciler) reconcileOne(ctx context.Context, c registry.Cluster, opts Options, summary *Summary) bool {
	clean := true
	if !r.reconcileNodes(ctx, c, opts, summary) {
		clean = false
	}
	if !r.reconcileDeployments(ctx, c, opts, summary) {
		clean = false
	}
	return clean
}

// reconcileNodes synchronises the registry's node rows for c with what
// Pulumi reports. Returns true on full success, false if any error was
// observed (in which case callers must not advance LastSynced).
func (r *Reconciler) reconcileNodes(ctx context.Context, c registry.Cluster, opts Options, summary *Summary) bool {
	pulumiNodes, perr := r.Pulumi.ListClusterNodes(ctx, c.Name)
	if perr != nil {
		// "No stack" is treated as drift: warn (and optionally prune the
		// whole cluster), don't fail the reconcile.
		if isStackNotFound(perr) {
			r.warnf("cluster %q: no Pulumi stack found", c.Name)
			if opts.Prune && !opts.DryRun {
				if err := r.Registry.DeleteCluster(ctx, c.Name); err != nil {
					r.warnf("cluster %q: prune (DeleteCluster) failed: %v", c.Name, err)
					return false
				}
				return true
			}
			summary.DriftItems++
			return true
		}
		r.warnf("cluster %q: list pulumi stacks failed: %v", c.Name, perr)
		return false
	}

	registryNodes, err := r.Registry.ListNodes(ctx, c.Name)
	if err != nil {
		r.warnf("cluster %q: list registry nodes failed: %v", c.Name, err)
		return false
	}

	// Index by hostname for set-difference work.
	pulumiByName := make(map[string]PulumiNode, len(pulumiNodes))
	for _, n := range pulumiNodes {
		pulumiByName[n.Hostname] = n
	}
	registryByName := make(map[string]registry.Node, len(registryNodes))
	for _, n := range registryNodes {
		registryByName[n.Hostname] = n
	}

	// Upsert every node Pulumi knows about. We always preserve JoinedAt
	// from the existing row so re-running sync does not falsely advance
	// the join timestamp.
	now := r.now()
	for _, pn := range pulumiNodes {
		joinedAt := now
		if existing, ok := registryByName[pn.Hostname]; ok && !existing.JoinedAt.IsZero() {
			joinedAt = existing.JoinedAt
		}
		if !opts.DryRun {
			if err := r.Registry.UpsertNode(ctx, registry.Node{
				ClusterName: c.Name,
				Hostname:    pn.Hostname,
				Role:        pn.Role,
				JoinedAt:    joinedAt,
			}); err != nil {
				r.warnf("cluster %q node %q: upsert failed: %v", c.Name, pn.Hostname, err)
				return false
			}
		}
		summary.NodesUpserted++
	}

	// Delete registry nodes Pulumi has forgotten. Nodes are always Pulumi-
	// owned, so this delete is unconditional (not gated on Prune).
	for _, rn := range registryNodes {
		if _, kept := pulumiByName[rn.Hostname]; kept {
			continue
		}
		if !opts.DryRun {
			if err := r.Registry.RemoveNode(ctx, c.Name, rn.Hostname); err != nil {
				r.warnf("cluster %q node %q: remove failed: %v", c.Name, rn.Hostname, err)
				return false
			}
		}
		summary.NodesRemoved++
	}
	return true
}

// reconcileDeployments synchronises the registry's deployment rows for c
// with what kubectl reports. Behaviour for missing-from-kubectl rows is
// controlled by opts.Prune; missing-from-registry rows are always inserted
// because they represent legitimate out-of-band deploys we want to capture.
func (r *Reconciler) reconcileDeployments(ctx context.Context, c registry.Cluster, opts Options, summary *Summary) bool {
	deps, err := r.fetchDeployments(ctx, c)
	if err != nil {
		r.warnf("cluster %q: fetch deployments failed: %v", c.Name, err)
		return false
	}

	registryDeps, err := r.Registry.ListDeployments(ctx, c.Name)
	if err != nil {
		r.warnf("cluster %q: list registry deployments failed: %v", c.Name, err)
		return false
	}

	kubectlByService := make(map[string]Deployment, len(deps))
	for _, d := range deps {
		kubectlByService[d.Service] = d
	}
	registryByService := make(map[string]registry.Deployment, len(registryDeps))
	for _, d := range registryDeps {
		registryByService[d.Service] = d
	}

	now := r.now()
	for _, kd := range deps {
		want := registry.Deployment{
			ClusterName: c.Name,
			Service:     kd.Service,
			Version:     kd.Version,
			Status:      kd.Status,
		}
		if existing, ok := registryByService[kd.Service]; ok {
			// Service is known; only write if anything observable diverged.
			if existing.Version == want.Version && existing.Status == want.Status {
				continue
			}
			want.DeployedAt = now
			want.DeployedBy = existing.DeployedBy
			if !opts.DryRun {
				if err := r.Registry.UpsertDeployment(ctx, want); err != nil {
					r.warnf("cluster %q service %q: upsert failed: %v", c.Name, kd.Service, err)
					return false
				}
			}
			summary.ServicesUpdated++
			continue
		}
		// New service discovered out-of-band.
		want.DeployedAt = now
		want.DeployedBy = "sync"
		if !opts.DryRun {
			if err := r.Registry.UpsertDeployment(ctx, want); err != nil {
				r.warnf("cluster %q service %q: insert failed: %v", c.Name, kd.Service, err)
				return false
			}
		}
		summary.ServicesAdded++
	}

	// Surface registry-only services as drift. The registry interface has
	// no DeleteDeployment method, so service-level prune is logged as
	// drift even when --prune is set. Cluster-level and node-level prune
	// (handled in reconcileNodes) cover the actionable cases; widening
	// the registry API to support per-service deletes is out of scope for
	// T9 and tracked separately.
	for _, rd := range registryDeps {
		if _, kept := kubectlByService[rd.Service]; kept {
			continue
		}
		r.warnf("cluster %q service %q: in registry but not in kubectl", c.Name, rd.Service)
		summary.DriftItems++
	}
	return true
}

// fetchDeployments shells out to kubectl and parses the JSON.
func (r *Reconciler) fetchDeployments(ctx context.Context, c registry.Cluster) ([]Deployment, error) {
	out, err := r.Kubectl.Run(ctx, c.KubeconfigPath, "get", "deployments", "-A", "-o", "json")
	if err != nil {
		return nil, err
	}
	return ParseDeployments(out)
}

// Deployment is the parsed view of a kubectl deployment row, scoped to the
// fields sync needs. It is exported so cmd/sync_test.go can construct
// canned KubectlRunner outputs.
type Deployment struct {
	Service string
	Version string
	Status  registry.DeploymentStatus
}

// kubectlList mirrors the top-level shape of `kubectl get deployments -A -o
// json`. We define a private struct rather than importing k8s.io/api so the
// dependency surface stays minimal.
type kubectlList struct {
	Items []kubectlDeployment `json:"items"`
}

type kubectlDeployment struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Replicas            int32 `json:"replicas"`
		ReadyReplicas       int32 `json:"readyReplicas"`
		UpdatedReplicas     int32 `json:"updatedReplicas"`
		UnavailableReplicas int32 `json:"unavailableReplicas"`
	} `json:"status"`
}

// ParseDeployments converts kubectl's JSON output into the trimmed
// Deployment view sync uses.
func ParseDeployments(raw []byte) ([]Deployment, error) {
	var list kubectlList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("sync: parse kubectl json: %w", err)
	}
	out := make([]Deployment, 0, len(list.Items))
	for _, item := range list.Items {
		service := serviceName(item)
		version := versionFromContainers(item.Spec.Template.Spec.Containers)
		status := rolloutStatus(item)
		out = append(out, Deployment{
			Service: service,
			Version: version,
			Status:  status,
		})
	}
	return out, nil
}

// serviceName picks the most stable identifier we can extract from a
// deployment: the standard label first, with metadata.name as a fallback so
// we never produce empty service names.
func serviceName(d kubectlDeployment) string {
	if d.Metadata.Labels != nil {
		if v, ok := d.Metadata.Labels["app.kubernetes.io/name"]; ok && v != "" {
			return v
		}
	}
	return d.Metadata.Name
}

// versionFromContainers returns the tag of the first container's image, or
// the empty string when the image is untagged or no containers exist.
func versionFromContainers(containers []struct {
	Image string `json:"image"`
}) string {
	if len(containers) == 0 {
		return ""
	}
	img := containers[0].Image
	// Split on the LAST colon to handle registries with explicit ports
	// (e.g. "registry:5000/svc:v1.0.0"). A colon-less image has no tag.
	idx := strings.LastIndex(img, ":")
	if idx < 0 {
		return ""
	}
	// Defensive: refuse a "tag" that looks like a port (digits only and
	// the next slash comes later). A real tag never has a "/" after it.
	tag := img[idx+1:]
	if strings.Contains(tag, "/") {
		return ""
	}
	return tag
}

// rolloutStatus maps the kubectl deployment status fields to the registry's
// DeploymentStatus enum. The mapping mirrors what kubectl rollout status
// would print, but reads the raw Status fields directly because shelling out
// to "rollout status" would block until ready.
func rolloutStatus(d kubectlDeployment) registry.DeploymentStatus {
	s := d.Status
	switch {
	case s.UnavailableReplicas > 0:
		return registry.StatusFailed
	case s.Replicas > 0 && s.ReadyReplicas == s.Replicas && s.UpdatedReplicas == s.Replicas:
		return registry.StatusRolledOut
	default:
		return registry.StatusRolling
	}
}

// isStackNotFound recognises ErrStackNotFound through wrapping.
func isStackNotFound(err error) bool {
	for e := err; e != nil; {
		if e == ErrStackNotFound {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := e.(unwrapper)
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// warnf writes to r.Warn when set and silently drops the message otherwise.
func (r *Reconciler) warnf(format string, args ...any) {
	if r.Warn == nil {
		return
	}
	fmt.Fprintf(r.Warn, "warning: "+format+"\n", args...)
}

// now is a thin helper that funnels every clock read through r.Now so a
// test clock applies uniformly across upsert and synced timestamps.
func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}
