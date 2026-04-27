// Copyright 2026 Foundry Fabric

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/registry/sync"
	"github.com/spf13/cobra"
)

// DiffDeps groups injectable dependencies for the diff command. Tests
// replace individual fields; nil fields fall back to production defaults
// shared with the sync command.
type DiffDeps struct {
	// OpenRegistry opens the local registry. Defaults to registry.NewRegistry.
	OpenRegistry func(ctx context.Context) (registry.Registry, error)

	// Pulumi enumerates the nodes Pulumi believes exist for a cluster.
	// Defaults to the same auto-API-backed implementation used by sync.
	Pulumi sync.PulumiClient

	// Kubectl runs kubectl commands. Defaults to the os/exec
	// implementation used by sync.
	Kubectl sync.KubectlRunner
}

// diffFlags holds CLI flags for the diff command.
type diffFlags struct {
	json bool
}

var diffF diffFlags

var diffCmd = &cobra.Command{
	Use:   "diff <cluster>",
	Short: "Show drift between the registry and live state for one cluster",
	Long: `Compare what the local registry believes about <cluster> against
what Pulumi and kubectl actually report, without writing anything.

Output groups every divergent item under added (+), changed (~), or removed
(-). Use this to preview what 'clusterbox sync' would change.

Exit codes:
  0  no drift
  1  drift detected
  2  error (cluster not found, kubectl unreachable, ...)`,
	Args: cobra.ExactArgs(1),
	RunE: runDiff,
}

func init() {
	diffCmd.Flags().BoolVar(&diffF.json, "json", false, "Print machine-readable JSON instead of the human-readable diff")
}

// runDiff is the cobra RunE handler. It maps the three documented exit
// codes onto returned errors so the cobra command runner sets the right
// process exit code without needing to call os.Exit directly.
func runDiff(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return RunDiff(ctx, args[0], DiffDeps{}, cmd.OutOrStdout(), cmd.ErrOrStderr(), diffF.json)
}

// errDriftDetected is returned by RunDiff when the diff is non-empty.
// runDiff translates it to exit code 1 via cobra; tests assert on it
// directly.
var errDriftDetected = errors.New("drift detected")

// RunDiff is the exported entry point used by both the cobra command and
// tests. It opens the registry, fetches live state via the injected Pulumi
// and Kubectl dependencies, computes the diff, and writes either a
// human-readable or JSON report.
//
// Returns nil for "no drift", errDriftDetected for "drift detected", or
// any other error for the error case (these become exit codes 0, 1, and
// 2 respectively when wrapped by cobra and the process supervisor).
func RunDiff(ctx context.Context, clusterName string, deps DiffDeps, stdout, stderr io.Writer, asJSON bool) error {
	open := deps.OpenRegistry
	if open == nil {
		open = registry.NewRegistry
	}
	reg, err := open(ctx)
	if err != nil {
		return fmt.Errorf("diff: open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	pulumi := deps.Pulumi
	if pulumi == nil {
		pulumi = newAutoPulumiClient()
	}
	kubectl := deps.Kubectl
	if kubectl == nil {
		kubectl = execKubectlRunner{}
	}

	cluster, err := reg.GetCluster(ctx, clusterName)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return fmt.Errorf("cluster %q not found in registry", clusterName)
		}
		return fmt.Errorf("diff: get cluster: %w", err)
	}

	report, err := computeDiff(ctx, reg, pulumi, kubectl, cluster)
	if err != nil {
		return err
	}

	if asJSON {
		if err := writeDiffJSON(stdout, cluster, report); err != nil {
			return err
		}
	} else {
		writeDiffHuman(stdout, cluster, report)
	}

	if report.empty() {
		return nil
	}
	return errDriftDetected
}

// diffReport holds the per-cluster diff broken out into added / changed /
// removed groups for both nodes and services. All slices are sorted for
// deterministic output.
type diffReport struct {
	NodesAdded     []nodeDelta    `json:"nodes_added"`
	NodesChanged   []nodeDelta    `json:"nodes_changed"`
	NodesRemoved   []nodeDelta    `json:"nodes_removed"`
	ServicesAdded  []serviceDelta `json:"services_added"`
	ServicesChange []serviceDelta `json:"services_changed"`
	ServicesRemove []serviceDelta `json:"services_removed"`
}

func (r diffReport) empty() bool {
	return len(r.NodesAdded) == 0 &&
		len(r.NodesChanged) == 0 &&
		len(r.NodesRemoved) == 0 &&
		len(r.ServicesAdded) == 0 &&
		len(r.ServicesChange) == 0 &&
		len(r.ServicesRemove) == 0
}

// nodeDelta describes one node difference. For added/removed entries
// only one side is populated; for changed entries Live and Registry
// hold the diverging values for the same hostname.
type nodeDelta struct {
	Hostname     string `json:"hostname"`
	LiveRole     string `json:"live_role,omitempty"`
	RegistryRole string `json:"registry_role,omitempty"`
}

// serviceDelta describes one service difference within the cluster.
type serviceDelta struct {
	Service         string `json:"service"`
	LiveVersion     string `json:"live_version,omitempty"`
	RegistryVersion string `json:"registry_version,omitempty"`
	LiveStatus      string `json:"live_status,omitempty"`
	RegistryStatus  string `json:"registry_status,omitempty"`
}

// computeDiff fetches live state from Pulumi and kubectl, fetches recorded
// state from the registry, and returns the per-item diff.
func computeDiff(
	ctx context.Context,
	reg registry.Registry,
	pulumi sync.PulumiClient,
	kubectl sync.KubectlRunner,
	cluster registry.Cluster,
) (diffReport, error) {
	livePulumiNodes, err := pulumi.ListClusterNodes(ctx, cluster.Name)
	if err != nil {
		if errors.Is(err, sync.ErrStackNotFound) {
			// Cluster has no Pulumi stack. This is itself a drift signal:
			// every node we have on record now appears as removed. We
			// don't surface a hard error so the diff can still inform
			// the operator about the orphaned registry rows.
			livePulumiNodes = nil
		} else {
			return diffReport{}, fmt.Errorf("diff: pulumi: %w", err)
		}
	}
	rawKube, err := kubectl.Run(ctx, cluster.KubeconfigPath, "get", "deployments", "-A", "-o", "json")
	if err != nil {
		return diffReport{}, fmt.Errorf("diff: kubectl: %w", err)
	}
	liveDeps, err := sync.ParseDeployments(rawKube)
	if err != nil {
		return diffReport{}, fmt.Errorf("diff: parse kubectl output: %w", err)
	}

	regNodes, err := reg.ListNodes(ctx, cluster.Name)
	if err != nil {
		return diffReport{}, fmt.Errorf("diff: registry nodes: %w", err)
	}
	regDeps, err := reg.ListDeployments(ctx, cluster.Name)
	if err != nil {
		return diffReport{}, fmt.Errorf("diff: registry deployments: %w", err)
	}

	rep := diffReport{}
	rep.diffNodes(livePulumiNodes, regNodes)
	rep.diffServices(liveDeps, regDeps)
	rep.sortAll()
	return rep, nil
}

func (r *diffReport) diffNodes(live []sync.PulumiNode, reg []registry.Node) {
	liveByHost := make(map[string]sync.PulumiNode, len(live))
	for _, n := range live {
		liveByHost[n.Hostname] = n
	}
	regByHost := make(map[string]registry.Node, len(reg))
	for _, n := range reg {
		regByHost[n.Hostname] = n
	}

	for host, ln := range liveByHost {
		rn, ok := regByHost[host]
		if !ok {
			r.NodesAdded = append(r.NodesAdded, nodeDelta{Hostname: host, LiveRole: ln.Role})
			continue
		}
		if ln.Role != rn.Role {
			r.NodesChanged = append(r.NodesChanged, nodeDelta{Hostname: host, LiveRole: ln.Role, RegistryRole: rn.Role})
		}
	}
	for host, rn := range regByHost {
		if _, ok := liveByHost[host]; !ok {
			r.NodesRemoved = append(r.NodesRemoved, nodeDelta{Hostname: host, RegistryRole: rn.Role})
		}
	}
}

func (r *diffReport) diffServices(live []sync.Deployment, reg []registry.Deployment) {
	liveByName := make(map[string]sync.Deployment, len(live))
	for _, d := range live {
		liveByName[d.Service] = d
	}
	regByName := make(map[string]registry.Deployment, len(reg))
	for _, d := range reg {
		regByName[d.Service] = d
	}

	for name, ld := range liveByName {
		rd, ok := regByName[name]
		if !ok {
			r.ServicesAdded = append(r.ServicesAdded, serviceDelta{
				Service:     name,
				LiveVersion: ld.Version,
				LiveStatus:  string(ld.Status),
			})
			continue
		}
		if ld.Version != rd.Version || ld.Status != rd.Status {
			r.ServicesChange = append(r.ServicesChange, serviceDelta{
				Service:         name,
				LiveVersion:     ld.Version,
				RegistryVersion: rd.Version,
				LiveStatus:      string(ld.Status),
				RegistryStatus:  string(rd.Status),
			})
		}
	}
	for name, rd := range regByName {
		if _, ok := liveByName[name]; !ok {
			r.ServicesRemove = append(r.ServicesRemove, serviceDelta{
				Service:         name,
				RegistryVersion: rd.Version,
				RegistryStatus:  string(rd.Status),
			})
		}
	}
}

func (r *diffReport) sortAll() {
	sort.Slice(r.NodesAdded, func(i, j int) bool { return r.NodesAdded[i].Hostname < r.NodesAdded[j].Hostname })
	sort.Slice(r.NodesChanged, func(i, j int) bool { return r.NodesChanged[i].Hostname < r.NodesChanged[j].Hostname })
	sort.Slice(r.NodesRemoved, func(i, j int) bool { return r.NodesRemoved[i].Hostname < r.NodesRemoved[j].Hostname })
	sort.Slice(r.ServicesAdded, func(i, j int) bool { return r.ServicesAdded[i].Service < r.ServicesAdded[j].Service })
	sort.Slice(r.ServicesChange, func(i, j int) bool { return r.ServicesChange[i].Service < r.ServicesChange[j].Service })
	sort.Slice(r.ServicesRemove, func(i, j int) bool { return r.ServicesRemove[i].Service < r.ServicesRemove[j].Service })
}

func writeDiffHuman(out io.Writer, cluster registry.Cluster, r diffReport) {
	synced := "never"
	if !cluster.LastSynced.IsZero() {
		synced = cluster.LastSynced.UTC().Format("2006-01-02 15:04 UTC")
	}
	_, _ = fmt.Fprintf(out, "cluster %s @ %s\n", cluster.Name, synced)

	if r.empty() {
		_, _ = fmt.Fprintln(out, "no drift")
		return
	}

	if len(r.NodesAdded)+len(r.NodesChanged)+len(r.NodesRemoved) > 0 {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "nodes:")
		for _, n := range r.NodesAdded {
			_, _ = fmt.Fprintf(out, "  + %s (%s)\n", n.Hostname, n.LiveRole)
		}
		for _, n := range r.NodesChanged {
			_, _ = fmt.Fprintf(out, "  ~ %s (registry: %s, live: %s)\n", n.Hostname, n.RegistryRole, n.LiveRole)
		}
		for _, n := range r.NodesRemoved {
			_, _ = fmt.Fprintf(out, "  - %s (was %s)\n", n.Hostname, n.RegistryRole)
		}
	}

	if len(r.ServicesAdded)+len(r.ServicesChange)+len(r.ServicesRemove) > 0 {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "services:")
		for _, s := range r.ServicesAdded {
			_, _ = fmt.Fprintf(out, "  + %s @ %s [%s]\n", s.Service, s.LiveVersion, s.LiveStatus)
		}
		for _, s := range r.ServicesChange {
			parts := []string{}
			if s.LiveVersion != s.RegistryVersion {
				parts = append(parts, fmt.Sprintf("version: %s → %s", s.RegistryVersion, s.LiveVersion))
			}
			if s.LiveStatus != s.RegistryStatus {
				parts = append(parts, fmt.Sprintf("status: %s → %s", s.RegistryStatus, s.LiveStatus))
			}
			_, _ = fmt.Fprintf(out, "  ~ %s (%s)\n", s.Service, strings.Join(parts, ", "))
		}
		for _, s := range r.ServicesRemove {
			_, _ = fmt.Fprintf(out, "  - %s @ %s\n", s.Service, s.RegistryVersion)
		}
	}
}

// diffJSONEnvelope is the on-the-wire JSON shape.
type diffJSONEnvelope struct {
	Cluster    string     `json:"cluster"`
	LastSynced string     `json:"last_synced,omitempty"`
	Drift      bool       `json:"drift"`
	Report     diffReport `json:"report"`
}

func writeDiffJSON(out io.Writer, cluster registry.Cluster, r diffReport) error {
	env := diffJSONEnvelope{
		Cluster: cluster.Name,
		Drift:   !r.empty(),
		Report:  r,
	}
	if !cluster.LastSynced.IsZero() {
		env.LastSynced = cluster.LastSynced.UTC().Format("2006-01-02T15:04:05Z")
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(env)
}
