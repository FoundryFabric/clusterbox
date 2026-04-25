package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/foundryfabric/clusterbox/internal/registry"
	_ "github.com/foundryfabric/clusterbox/internal/registry/sqlite"
	"github.com/spf13/cobra"
)

// statusFlags holds CLI flags for the status command.
type statusFlags struct {
	json bool
}

var statusF statusFlags

var statusCmd = &cobra.Command{
	Use:   "status <cluster>",
	Short: "Show registry-recorded state for a single cluster",
	Long: `Status renders the locally-recorded state of one cluster: its header
metadata, registered nodes, and most-recent service deployments. It reads the
local clusterbox registry only and does not contact any live nodes.`,
	Args: cobra.ExactArgs(1),
	RunE: runStatus,
}

func init() {
	statusCmd.Flags().BoolVar(&statusF.json, "json", false, "Emit machine-readable JSON instead of tables")
}

// statusTimestampFormat is the human-readable timestamp format used for every
// time column in the status output.
const statusTimestampFormat = "2006-01-02 15:04 UTC"

// statusClusterJSON is the JSON shape for the cluster header.
type statusClusterJSON struct {
	Name           string `json:"name"`
	Provider       string `json:"provider"`
	Region         string `json:"region"`
	Env            string `json:"env"`
	CreatedAt      string `json:"created_at"`
	KubeconfigPath string `json:"kubeconfig_path"`
	LastSynced     string `json:"last_synced"`
}

// statusNodeJSON is the JSON shape for a single node row.
type statusNodeJSON struct {
	Hostname string `json:"hostname"`
	Role     string `json:"role"`
	JoinedAt string `json:"joined_at"`
}

// statusDeploymentJSON is the JSON shape for a single deployment row.
type statusDeploymentJSON struct {
	Service    string `json:"service"`
	Version    string `json:"version"`
	DeployedAt string `json:"deployed_at"`
	DeployedBy string `json:"deployed_by"`
	Status     string `json:"status"`
}

// statusJSON is the top-level envelope written under --json.
type statusJSON struct {
	Cluster     statusClusterJSON      `json:"cluster"`
	Nodes       []statusNodeJSON       `json:"nodes"`
	Deployments []statusDeploymentJSON `json:"deployments"`
}

// runStatus is the cobra RunE handler for `clusterbox status`.
func runStatus(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	reg, err := registry.NewRegistry(ctx)
	if err != nil {
		return fmt.Errorf("status: open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	// Suppress cobra's automatic usage/error printing for the not-found
	// path so the operator sees only our stderr message.
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	return RunStatus(ctx, reg, cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0], statusF.json)
}

// RunStatus renders the registry-recorded status of name to out. Cluster-not-
// found is reported to errOut and returned as an error so callers can exit
// non-zero. It is exported so tests can drive the command with an in-memory
// registry and captured streams.
func RunStatus(ctx context.Context, reg registry.Registry, out, errOut io.Writer, name string, asJSON bool) error {
	cluster, err := reg.GetCluster(ctx, name)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			fmt.Fprintf(errOut, "cluster %q not found in registry\n", name)
			return err
		}
		return fmt.Errorf("status: get cluster %q: %w", name, err)
	}

	nodes, err := reg.ListNodes(ctx, name)
	if err != nil {
		return fmt.Errorf("status: list nodes for %q: %w", name, err)
	}
	deps, err := reg.ListDeployments(ctx, name)
	if err != nil {
		return fmt.Errorf("status: list deployments for %q: %w", name, err)
	}

	// Deterministic order independent of the backend's row ordering.
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Hostname < nodes[j].Hostname })
	sort.Slice(deps, func(i, j int) bool { return deps[i].Service < deps[j].Service })

	if asJSON {
		return writeStatusJSON(out, cluster, nodes, deps)
	}
	return writeStatusText(out, cluster, nodes, deps)
}

// writeStatusText renders the three plain-text sections separated by blank
// lines. Each section is independently flushed so columns within a section
// align without affecting other sections.
func writeStatusText(out io.Writer, c registry.Cluster, nodes []registry.Node, deps []registry.Deployment) error {
	if err := writeClusterHeader(out, c); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out); err != nil {
		return err
	}
	if err := writeNodesTable(out, nodes); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out); err != nil {
		return err
	}
	return writeDeploymentsTable(out, deps)
}

// writeClusterHeader writes the two-column "key: value" cluster summary.
func writeClusterHeader(out io.Writer, c registry.Cluster) error {
	tw := tabwriter.NewWriter(out, 0, 0, 1, ' ', 0)
	rows := [][2]string{
		{"name", c.Name},
		{"provider", c.Provider},
		{"region", c.Region},
		{"env", c.Env},
		{"created_at", formatStatusTime(c.CreatedAt)},
		{"kubeconfig_path", c.KubeconfigPath},
		{"last_synced", formatStatusTime(c.LastSynced)},
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(tw, "%s:\t%s\n", r[0], r[1]); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// writeNodesTable writes the HOSTNAME/ROLE/JOINED_AT table or an empty marker.
func writeNodesTable(out io.Writer, nodes []registry.Node) error {
	if len(nodes) == 0 {
		_, err := fmt.Fprintln(out, "(no nodes)")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "HOSTNAME\tROLE\tJOINED_AT"); err != nil {
		return err
	}
	for _, n := range nodes {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n", n.Hostname, n.Role, formatStatusTime(n.JoinedAt)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// writeDeploymentsTable writes the SERVICE/VERSION/DEPLOYED_AT/DEPLOYED_BY/
// STATUS table or an empty marker.
func writeDeploymentsTable(out io.Writer, deps []registry.Deployment) error {
	if len(deps) == 0 {
		_, err := fmt.Fprintln(out, "(no deployments)")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "SERVICE\tVERSION\tDEPLOYED_AT\tDEPLOYED_BY\tSTATUS"); err != nil {
		return err
	}
	for _, d := range deps {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			d.Service, d.Version, formatStatusTime(d.DeployedAt), d.DeployedBy, string(d.Status),
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// writeStatusJSON renders the {cluster, nodes, deployments} envelope.
func writeStatusJSON(out io.Writer, c registry.Cluster, nodes []registry.Node, deps []registry.Deployment) error {
	doc := statusJSON{
		Cluster: statusClusterJSON{
			Name:           c.Name,
			Provider:       c.Provider,
			Region:         c.Region,
			Env:            c.Env,
			CreatedAt:      formatStatusTime(c.CreatedAt),
			KubeconfigPath: c.KubeconfigPath,
			LastSynced:     formatStatusTime(c.LastSynced),
		},
		Nodes:       make([]statusNodeJSON, 0, len(nodes)),
		Deployments: make([]statusDeploymentJSON, 0, len(deps)),
	}
	for _, n := range nodes {
		doc.Nodes = append(doc.Nodes, statusNodeJSON{
			Hostname: n.Hostname,
			Role:     n.Role,
			JoinedAt: formatStatusTime(n.JoinedAt),
		})
	}
	for _, d := range deps {
		doc.Deployments = append(doc.Deployments, statusDeploymentJSON{
			Service:    d.Service,
			Version:    d.Version,
			DeployedAt: formatStatusTime(d.DeployedAt),
			DeployedBy: d.DeployedBy,
			Status:     string(d.Status),
		})
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// formatStatusTime returns "-" for the zero time, else the canonical UTC
// representation. All times are normalised to UTC so output is independent of
// the caller's local zone.
func formatStatusTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(statusTimestampFormat)
}
