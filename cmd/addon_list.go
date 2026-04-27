package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/foundryfabric/clusterbox/internal/addon"
	"github.com/foundryfabric/clusterbox/internal/registry"
	_ "github.com/foundryfabric/clusterbox/internal/registry/sqlite"
	"github.com/spf13/cobra"
)

// addonListFlags holds CLI flags for `clusterbox addon list`.
type addonListFlags struct {
	cluster string
	json    bool
}

var addonListF addonListFlags

var addonListCmd = &cobra.Command{
	Use:   "list",
	Short: "List addons (catalog or installed on a cluster)",
	Long: `List shows either the catalog of addons clusterbox knows about, or the
addons installed on a specific cluster.

Without --cluster, the output is the embedded addon catalog: every addon that
ships with this clusterbox binary, regardless of whether it has been installed
anywhere.

With --cluster <name>, the output is the set of addons that the local registry
records as installed on that cluster (deployments where kind='addon').`,
	RunE: runAddonList,
}

func init() {
	addonListCmd.Flags().StringVar(&addonListF.cluster, "cluster", "", "Show addons installed on this cluster instead of the catalog")
	addonListCmd.Flags().BoolVar(&addonListF.json, "json", false, "Emit machine-readable JSON instead of a table")
}

// addonListTimestampFormat is the human-readable timestamp format used for
// installed-mode rows.
const addonListTimestampFormat = "2006-01-02 15:04 UTC"

// catalogRow is the JSON representation of a single catalog row.
type catalogRow struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Requires    []string `json:"requires"`
}

// installedRow is the JSON representation of a single installed-addon row.
type installedRow struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	InstalledAt string `json:"installed_at"`
	Status      string `json:"status"`
}

// runAddonList is the cobra RunE handler for `clusterbox addon list`.
func runAddonList(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	if addonListF.cluster == "" {
		return RunAddonListCatalog(addon.DefaultCatalog(), cmd.OutOrStdout(), addonListF.json)
	}

	reg, err := registry.NewRegistry(ctx)
	if err != nil {
		return fmt.Errorf("addon list: open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	return RunAddonListInstalled(ctx, reg, cmd.OutOrStdout(), addonListF.cluster, addonListF.json)
}

// RunAddonListCatalog renders the addon catalog to out. It is exported so tests
// can drive it with a constructed Catalog and captured stdout.
func RunAddonListCatalog(cat *addon.Catalog, out io.Writer, asJSON bool) error {
	names, err := cat.List()
	if err != nil {
		return fmt.Errorf("addon list: load catalog: %w", err)
	}

	rows := make([]catalogRow, 0, len(names))
	for _, name := range names {
		a, err := cat.Get(name)
		if err != nil {
			return fmt.Errorf("addon list: get %q: %w", name, err)
		}
		req := a.Requires
		if req == nil {
			req = []string{}
		}
		rows = append(rows, catalogRow{
			Name:        a.Name,
			Version:     a.Version,
			Description: a.Description,
			Requires:    req,
		})
	}

	// cat.List() already returns names sorted, but enforce explicitly so the
	// sort guarantee is local to this function.
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	if asJSON {
		return writeCatalogJSON(out, rows)
	}

	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "(no addons in catalog)")
		return nil
	}
	return writeCatalogTable(out, rows)
}

// RunAddonListInstalled renders the addons installed on clusterName. It is
// exported so tests can drive it with an in-memory registry and captured
// stdout.
func RunAddonListInstalled(ctx context.Context, reg registry.Registry, out io.Writer, clusterName string, asJSON bool) error {
	deps, err := reg.ListDeployments(ctx, clusterName)
	if err != nil {
		return fmt.Errorf("addon list: list deployments for %q: %w", clusterName, err)
	}

	rows := make([]installedRow, 0, len(deps))
	for _, d := range deps {
		if d.Kind != registry.KindAddon {
			continue
		}
		rows = append(rows, installedRow{
			Name:        d.Service,
			Version:     d.Version,
			InstalledAt: formatAddonTime(d.DeployedAt),
			Status:      string(d.Status),
		})
	}

	// Deterministic order independent of registry row ordering.
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	if asJSON {
		return writeInstalledJSON(out, rows)
	}

	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "(no addons installed)")
		return nil
	}
	return writeInstalledTable(out, rows)
}

// writeCatalogTable emits the NAME/VERSION/DESCRIPTION/REQUIRES catalog table.
func writeCatalogTable(out io.Writer, rows []catalogRow) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tVERSION\tDESCRIPTION\tREQUIRES"); err != nil {
		return err
	}
	for _, r := range rows {
		req := "-"
		if len(r.Requires) > 0 {
			req = strings.Join(r.Requires, ",")
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.Version, r.Description, req); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// writeInstalledTable emits the NAME/VERSION/INSTALLED_AT/STATUS table.
func writeInstalledTable(out io.Writer, rows []installedRow) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tVERSION\tINSTALLED_AT\tSTATUS"); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.Version, r.InstalledAt, r.Status); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// writeCatalogJSON emits a JSON array of catalog rows. An empty catalog
// produces "[]\n".
func writeCatalogJSON(out io.Writer, rows []catalogRow) error {
	if rows == nil {
		rows = []catalogRow{}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

// writeInstalledJSON emits a JSON array of installed-addon rows. An empty
// install set produces "[]\n".
func writeInstalledJSON(out io.Writer, rows []installedRow) error {
	if rows == nil {
		rows = []installedRow{}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

// formatAddonTime returns "-" for the zero time, else the canonical UTC
// representation.
func formatAddonTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(addonListTimestampFormat)
}
