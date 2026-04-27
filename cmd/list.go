package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/foundryfabric/clusterbox/internal/registry"
	_ "github.com/foundryfabric/clusterbox/internal/registry/sqlite"
	"github.com/spf13/cobra"
)

// listFlags holds CLI flags for the list command.
type listFlags struct {
	json bool
}

var listF listFlags

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List clusters tracked in the local registry",
	Long: `List enumerates every cluster recorded in the local clusterbox registry,
along with its node count, current service deployment count, and the time of
its last successful sync.`,
	RunE: runList,
}

func init() {
	listCmd.Flags().BoolVar(&listF.json, "json", false, "Emit machine-readable JSON instead of a table")
}

// listRow is the JSON representation of a single cluster row.
type listRow struct {
	Name       string `json:"name"`
	Provider   string `json:"provider"`
	Region     string `json:"region"`
	Env        string `json:"env"`
	Nodes      int    `json:"nodes"`
	Services   int    `json:"services"`
	LastSynced string `json:"last_synced"`
}

// lastSyncedFormat is the human-readable timestamp format used by the table.
const lastSyncedFormat = "2006-01-02 15:04 UTC"

// runList is the cobra RunE handler for `clusterbox list`.
func runList(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	reg, err := registry.NewRegistry(ctx)
	if err != nil {
		return fmt.Errorf("list: open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	return RunList(ctx, reg, cmd.OutOrStdout(), listF.json)
}

// RunList renders the cluster list to out using the supplied registry. It is
// exported so tests can drive the command with an in-memory registry and
// captured stdout.
func RunList(ctx context.Context, reg registry.Registry, out io.Writer, asJSON bool) error {
	clusters, err := reg.ListClusters(ctx)
	if err != nil {
		return fmt.Errorf("list: list clusters: %w", err)
	}

	// Deterministic order: alphabetical by name. ListClusters returns
	// results in unspecified order per the interface contract.
	sort.Slice(clusters, func(i, j int) bool {
		return clusters[i].Name < clusters[j].Name
	})

	rows, err := buildRows(ctx, reg, clusters)
	if err != nil {
		return err
	}

	if asJSON {
		return writeJSON(out, rows)
	}

	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, `no clusters tracked. run "clusterbox up" to create one.`)
		return nil
	}

	return writeTable(out, rows)
}

// buildRows resolves node and deployment counts for each cluster and returns
// the rendered rows in input order.
func buildRows(ctx context.Context, reg registry.Registry, clusters []registry.Cluster) ([]listRow, error) {
	rows := make([]listRow, 0, len(clusters))
	for _, c := range clusters {
		nodes, err := reg.ListNodes(ctx, c.Name)
		if err != nil {
			return nil, fmt.Errorf("list: list nodes for %q: %w", c.Name, err)
		}
		deps, err := reg.ListDeployments(ctx, c.Name)
		if err != nil {
			return nil, fmt.Errorf("list: list deployments for %q: %w", c.Name, err)
		}

		rows = append(rows, listRow{
			Name:       c.Name,
			Provider:   c.Provider,
			Region:     c.Region,
			Env:        c.Env,
			Nodes:      len(nodes),
			Services:   len(deps),
			LastSynced: formatLastSynced(c.LastSynced),
		})
	}
	return rows, nil
}

// formatLastSynced returns "-" for the zero time and the canonical UTC format
// otherwise. Times are normalised to UTC so output is independent of the
// caller's local zone.
func formatLastSynced(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(lastSyncedFormat)
}

// writeTable emits a tab-aligned table of rows to out.
func writeTable(out io.Writer, rows []listRow) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tPROVIDER\tREGION\tENV\tNODES\tSERVICES\tLAST_SYNCED"); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			r.Name, r.Provider, r.Region, r.Env, r.Nodes, r.Services, r.LastSynced,
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// writeJSON emits a JSON array of rows. An empty registry produces "[]\n".
func writeJSON(out io.Writer, rows []listRow) error {
	if rows == nil {
		rows = []listRow{}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}
