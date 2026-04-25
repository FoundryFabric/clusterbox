package cmd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/cmd"
	"github.com/foundryfabric/clusterbox/internal/addon"
	"github.com/foundryfabric/clusterbox/internal/registry"
)

// TestAddonListCatalog verifies the catalog (no --cluster) mode renders the
// embedded gha-runner-scale-set entry with expected columns.
func TestAddonListCatalog(t *testing.T) {
	var buf bytes.Buffer
	if err := cmd.RunAddonListCatalog(addon.DefaultCatalog(), &buf, false); err != nil {
		t.Fatalf("RunAddonListCatalog: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "NAME") ||
		!strings.Contains(got, "VERSION") ||
		!strings.Contains(got, "DESCRIPTION") ||
		!strings.Contains(got, "REQUIRES") {
		t.Errorf("expected header columns in output:\n%s", got)
	}
	if !strings.Contains(got, "gha-runner-scale-set") {
		t.Errorf("expected embedded addon gha-runner-scale-set in output:\n%s", got)
	}
	if !strings.Contains(got, "0.10.1") {
		t.Errorf("expected version 0.10.1 in output:\n%s", got)
	}
}

// TestAddonListCatalogJSON verifies the catalog --json mode emits a valid
// deterministic JSON array containing the embedded addon.
func TestAddonListCatalogJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := cmd.RunAddonListCatalog(addon.DefaultCatalog(), &buf, true); err != nil {
		t.Fatalf("RunAddonListCatalog: %v", err)
	}

	var rows []struct {
		Name        string   `json:"name"`
		Version     string   `json:"version"`
		Description string   `json:"description"`
		Requires    []string `json:"requires"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, buf.String())
	}
	if len(rows) == 0 {
		t.Fatalf("expected at least one row, got %d", len(rows))
	}

	// Verify deterministic sort by name across all rows.
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Name > rows[i].Name {
			t.Errorf("rows not sorted by name: %s came after %s", rows[i].Name, rows[i-1].Name)
		}
	}

	// Locate the known embedded addon.
	var found bool
	for _, r := range rows {
		if r.Name == "gha-runner-scale-set" {
			found = true
			if r.Version == "" {
				t.Errorf("gha-runner-scale-set: empty version")
			}
			if r.Description == "" {
				t.Errorf("gha-runner-scale-set: empty description")
			}
			if r.Requires == nil {
				t.Errorf("gha-runner-scale-set: requires is nil; expected non-nil slice")
			}
		}
	}
	if !found {
		t.Errorf("expected gha-runner-scale-set in JSON output, got: %s", buf.String())
	}
}

// TestAddonListInstalledEmpty verifies the installed mode prints the friendly
// hint when no addons are installed.
func TestAddonListInstalledEmpty(t *testing.T) {
	reg := newTempRegistry(t)
	ctx := context.Background()

	if err := reg.UpsertCluster(ctx, registry.Cluster{
		Name: "dev-1", Provider: "hetzner", Region: "fsn", Env: "dev",
	}); err != nil {
		t.Fatalf("upsert cluster: %v", err)
	}

	var buf bytes.Buffer
	if err := cmd.RunAddonListInstalled(ctx, reg, &buf, "dev-1", false); err != nil {
		t.Fatalf("RunAddonListInstalled: %v", err)
	}

	want := "(no addons installed)\n"
	if got := buf.String(); got != want {
		t.Errorf("empty installed output mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestAddonListInstalledFiltersByKind verifies that only kind=addon
// deployments appear in installed mode (app deployments are excluded).
func TestAddonListInstalledFiltersByKind(t *testing.T) {
	reg := newTempRegistry(t)
	ctx := context.Background()

	if err := reg.UpsertCluster(ctx, registry.Cluster{
		Name: "dev-1", Provider: "hetzner", Region: "fsn", Env: "dev",
	}); err != nil {
		t.Fatalf("upsert cluster: %v", err)
	}

	installedAt := time.Date(2026, 4, 25, 10, 32, 0, 0, time.UTC)

	// Two addons (insert in reverse order to verify sort) and one app
	// deployment that must NOT appear.
	if err := reg.UpsertDeployment(ctx, registry.Deployment{
		ClusterName: "dev-1",
		Service:     "ingress-nginx",
		Version:     "1.0.0",
		DeployedAt:  installedAt,
		Status:      registry.StatusRolledOut,
		Kind:        registry.KindAddon,
	}); err != nil {
		t.Fatalf("upsert ingress-nginx: %v", err)
	}
	if err := reg.UpsertDeployment(ctx, registry.Deployment{
		ClusterName: "dev-1",
		Service:     "cert-manager",
		Version:     "1.14.0",
		DeployedAt:  installedAt,
		Status:      registry.StatusRolledOut,
		Kind:        registry.KindAddon,
	}); err != nil {
		t.Fatalf("upsert cert-manager: %v", err)
	}
	if err := reg.UpsertDeployment(ctx, registry.Deployment{
		ClusterName: "dev-1",
		Service:     "api",
		Version:     "v1.0.0",
		DeployedAt:  installedAt,
		Status:      registry.StatusRolledOut,
		Kind:        registry.KindApp,
	}); err != nil {
		t.Fatalf("upsert api: %v", err)
	}

	var buf bytes.Buffer
	if err := cmd.RunAddonListInstalled(ctx, reg, &buf, "dev-1", false); err != nil {
		t.Fatalf("RunAddonListInstalled: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "NAME") || !strings.Contains(got, "INSTALLED_AT") || !strings.Contains(got, "STATUS") {
		t.Errorf("expected header row, got:\n%s", got)
	}
	if !strings.Contains(got, "cert-manager") {
		t.Errorf("expected cert-manager in output:\n%s", got)
	}
	if !strings.Contains(got, "ingress-nginx") {
		t.Errorf("expected ingress-nginx in output:\n%s", got)
	}
	if strings.Contains(got, "api") {
		// "api" is a substring of nothing else in the expected output, so a
		// stray match here means the kind filter regressed.
		t.Errorf("expected app deployment 'api' to be filtered out, got:\n%s", got)
	}
	if !strings.Contains(got, "2026-04-25 10:32 UTC") {
		t.Errorf("expected formatted installed_at in output:\n%s", got)
	}

	// Sort order: cert-manager must precede ingress-nginx.
	cmIdx := strings.Index(got, "cert-manager")
	ngIdx := strings.Index(got, "ingress-nginx")
	if cmIdx < 0 || ngIdx < 0 || cmIdx > ngIdx {
		t.Errorf("expected cert-manager before ingress-nginx (alphabetical), got:\n%s", got)
	}
}

// TestAddonListInstalledJSON verifies --json output in installed mode produces
// a valid array sorted by name with stable field names.
func TestAddonListInstalledJSON(t *testing.T) {
	reg := newTempRegistry(t)
	ctx := context.Background()

	if err := reg.UpsertCluster(ctx, registry.Cluster{
		Name: "dev-1", Provider: "hetzner", Region: "fsn", Env: "dev",
	}); err != nil {
		t.Fatalf("upsert cluster: %v", err)
	}

	installedAt := time.Date(2026, 4, 25, 10, 32, 0, 0, time.UTC)
	if err := reg.UpsertDeployment(ctx, registry.Deployment{
		ClusterName: "dev-1",
		Service:     "ingress-nginx",
		Version:     "1.0.0",
		DeployedAt:  installedAt,
		Status:      registry.StatusRolledOut,
		Kind:        registry.KindAddon,
	}); err != nil {
		t.Fatalf("upsert ingress-nginx: %v", err)
	}
	if err := reg.UpsertDeployment(ctx, registry.Deployment{
		ClusterName: "dev-1",
		Service:     "cert-manager",
		Version:     "1.14.0",
		DeployedAt:  installedAt,
		Status:      registry.StatusRolledOut,
		Kind:        registry.KindAddon,
	}); err != nil {
		t.Fatalf("upsert cert-manager: %v", err)
	}

	var buf bytes.Buffer
	if err := cmd.RunAddonListInstalled(ctx, reg, &buf, "dev-1", true); err != nil {
		t.Fatalf("RunAddonListInstalled: %v", err)
	}

	var rows []struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		InstalledAt string `json:"installed_at"`
		Status      string `json:"status"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, buf.String())
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d: %+v", len(rows), rows)
	}
	if rows[0].Name != "cert-manager" || rows[1].Name != "ingress-nginx" {
		t.Errorf("expected sort order [cert-manager, ingress-nginx], got [%s, %s]",
			rows[0].Name, rows[1].Name)
	}
	if rows[0].InstalledAt != "2026-04-25 10:32 UTC" {
		t.Errorf("cert-manager installed_at=%q want %q",
			rows[0].InstalledAt, "2026-04-25 10:32 UTC")
	}
	if rows[0].Status != string(registry.StatusRolledOut) {
		t.Errorf("cert-manager status=%q want %q", rows[0].Status, registry.StatusRolledOut)
	}
}

// TestAddonListInstalledJSONEmpty verifies that --json produces "[]" rather
// than the human-readable hint when no addons are installed.
func TestAddonListInstalledJSONEmpty(t *testing.T) {
	reg := newTempRegistry(t)
	ctx := context.Background()

	if err := reg.UpsertCluster(ctx, registry.Cluster{
		Name: "dev-1", Provider: "hetzner", Region: "fsn", Env: "dev",
	}); err != nil {
		t.Fatalf("upsert cluster: %v", err)
	}

	var buf bytes.Buffer
	if err := cmd.RunAddonListInstalled(ctx, reg, &buf, "dev-1", true); err != nil {
		t.Fatalf("RunAddonListInstalled: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, buf.String())
	}
	if len(rows) != 0 {
		t.Errorf("expected empty array, got %d rows", len(rows))
	}
}
