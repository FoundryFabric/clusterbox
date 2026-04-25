package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/foundryfabric/clusterbox/internal/provision"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// ReconcileDeps groups injectable dependencies for the post-operation
// reconciler hook. Tests replace fields; nil fields fall back to the
// production hcloud / sqlite implementations.
type ReconcileDeps struct {
	// OpenRegistry opens the local registry. Defaults to
	// registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)
	// NewLister builds a HCloudResourceLister around the given Hetzner
	// API token. Defaults to wrapping hcloud.NewClient.
	NewLister func(token string) provision.HCloudResourceLister
}

// runReconcileHook is the best-effort post-operation reconciliation pass
// invoked at the end of up / add-node / remove-node / destroy. It logs
// failures to stderr but never returns an error: a reconciler hiccup
// must not fail a parent command whose primary work has already
// succeeded.
//
// hetznerToken is read from the caller's environment (HETZNER_API_TOKEN);
// when empty the function logs a warning and skips the pass.
func runReconcileHook(ctx context.Context, deps ReconcileDeps, clusterName, hetznerToken string) {
	if clusterName == "" {
		return
	}
	if hetznerToken == "" {
		fmt.Fprintln(os.Stderr, "warning: reconciler skipped: HETZNER_API_TOKEN unset")
		return
	}

	openRegistry := deps.OpenRegistry
	if openRegistry == nil {
		openRegistry = registry.NewRegistry
	}
	newLister := deps.NewLister
	if newLister == nil {
		newLister = func(token string) provision.HCloudResourceLister {
			return provision.NewHCloudLister(hcloud.NewClient(hcloud.WithToken(token)))
		}
	}

	reg, err := openRegistry(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: reconciler skipped: open registry: %v\n", err)
		return
	}
	defer func() {
		if cerr := reg.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "warning: reconciler: close registry: %v\n", cerr)
		}
	}()

	r := &provision.Reconciler{
		Registry: reg,
		Lister:   newLister(hetznerToken),
	}
	summary, err := r.Reconcile(ctx, clusterName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: reconciler failed for cluster %q: %v\n", clusterName, err)
		return
	}

	fmt.Fprintf(os.Stderr,
		"reconciler: cluster=%s added=%d existing=%d marked_destroyed=%d unmanaged=%d\n",
		clusterName, summary.Added, summary.Existing, summary.MarkedDestroyed, len(summary.Unmanaged),
	)
	if len(summary.Unmanaged) > 0 {
		fmt.Fprintf(os.Stderr, "warning: reconciler: %d unmanaged resources detected: %v\n", len(summary.Unmanaged), summary.Unmanaged)
	}
}
