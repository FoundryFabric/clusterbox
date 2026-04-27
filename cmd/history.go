package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/foundryfabric/clusterbox/internal/registry"
	_ "github.com/foundryfabric/clusterbox/internal/registry/sqlite"
	"github.com/spf13/cobra"
)

// historyFlags holds CLI flags for the history command.
type historyFlags struct {
	cluster string
	service string
	limit   int
	json    bool
}

var historyF historyFlags

// defaultHistoryLimit caps the result set when the operator does not pass
// --limit explicitly.
const defaultHistoryLimit = 50

// historyWhenFormat is the human-readable timestamp format used in the table.
const historyWhenFormat = "2006-01-02 15:04"

// historyErrorMaxLen is the maximum width of the ERROR column before
// truncation. Truncation is rune-aware so multi-byte characters are not split.
const historyErrorMaxLen = 60

var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "Show deployment history recorded in the local registry",
	Long: `History prints every recorded deployment attempt, most recent first.
Use --cluster and --service to narrow the result set; --limit caps how many
rows are returned (default 50). Pass --json for machine-readable output.`,
	RunE: runHistory,
}

func init() {
	historyCmd.Flags().StringVar(&historyF.cluster, "cluster", "", "Filter by cluster name (default: any)")
	historyCmd.Flags().StringVar(&historyF.service, "service", "", "Filter by service name (default: any)")
	historyCmd.Flags().IntVar(&historyF.limit, "limit", defaultHistoryLimit, "Maximum number of rows to return")
	historyCmd.Flags().BoolVar(&historyF.json, "json", false, "Emit machine-readable JSON instead of a table")
}

// historyRow is the JSON representation of a single history row.
type historyRow struct {
	When       string `json:"when"`
	Cluster    string `json:"cluster"`
	Service    string `json:"service"`
	Version    string `json:"version"`
	Status     string `json:"status"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// runHistory is the cobra RunE handler for `clusterbox history`.
func runHistory(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	reg, err := registry.NewRegistry(ctx)
	if err != nil {
		return fmt.Errorf("history: open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	return RunHistory(ctx, reg, cmd.OutOrStdout(), registry.HistoryFilter{
		ClusterName: historyF.cluster,
		Service:     historyF.service,
		Limit:       historyF.limit,
	}, historyF.json)
}

// RunHistory renders the deployment history to out using the supplied
// registry. It is exported so tests can drive the command with an in-memory
// registry and a captured writer.
func RunHistory(ctx context.Context, reg registry.Registry, out io.Writer, filter registry.HistoryFilter, asJSON bool) error {
	entries, err := reg.ListHistory(ctx, filter)
	if err != nil {
		return fmt.Errorf("history: list history: %w", err)
	}

	rows := buildHistoryRows(entries)

	if asJSON {
		return writeHistoryJSON(out, rows)
	}

	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "no history matches.")
		return nil
	}

	return writeHistoryTable(out, rows)
}

// buildHistoryRows projects registry entries into rendered row structs in
// input order (which the registry guarantees is reverse-chronological).
func buildHistoryRows(entries []registry.DeploymentHistoryEntry) []historyRow {
	rows := make([]historyRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, historyRow{
			When:       e.AttemptedAt.UTC().Format(historyWhenFormat),
			Cluster:    e.ClusterName,
			Service:    e.Service,
			Version:    e.Version,
			Status:     string(e.Status),
			DurationMs: e.RolloutDurationMs,
			Error:      e.Error,
		})
	}
	return rows
}

// formatDuration renders a millisecond count as "<ms>ms" below 1000 and
// "<s>s" (integer seconds, truncated) at or above 1000.
func formatDuration(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%ds", ms/1000)
}

// truncateError shortens s to at most historyErrorMaxLen runes, replacing the
// tail with an ellipsis when truncation occurs. Operating on runes rather
// than bytes avoids splitting multi-byte UTF-8 sequences.
func truncateError(s string) string {
	runes := []rune(s)
	if len(runes) <= historyErrorMaxLen {
		return s
	}
	if historyErrorMaxLen <= 3 {
		return string(runes[:historyErrorMaxLen])
	}
	return string(runes[:historyErrorMaxLen-3]) + "..."
}

// writeHistoryTable emits a tab-aligned table of rows to out.
func writeHistoryTable(out io.Writer, rows []historyRow) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "WHEN\tCLUSTER\tSERVICE\tVERSION\tSTATUS\tDURATION\tERROR"); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.When, r.Cluster, r.Service, r.Version, r.Status,
			formatDuration(r.DurationMs), truncateError(r.Error),
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// writeHistoryJSON emits a JSON array of rows. An empty result produces
// "[]\n". Durations are emitted as raw milliseconds so consumers can apply
// their own formatting; the human-readable formatting is table-only.
func writeHistoryJSON(out io.Writer, rows []historyRow) error {
	if rows == nil {
		rows = []historyRow{}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}
