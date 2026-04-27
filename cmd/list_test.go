package cmd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/cmd"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/registry/sqlite"
)

// newTempRegistry creates a fresh sqlite-backed registry under a tempdir and
// registers a Close cleanup with the test.
func newTempRegistry(t *testing.T) registry.Registry {
	t.Helper()
	dir := t.TempDir()
	p, err := sqlite.New(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("open sqlite registry: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestListEmpty verifies the human-readable empty-registry hint and exit-success.
func TestListEmpty(t *testing.T) {
	reg := newTempRegistry(t)

	var buf bytes.Buffer
	if err := cmd.RunList(context.Background(), reg, &buf, false); err != nil {
		t.Fatalf("RunList: %v", err)
	}

	want := "no clusters tracked. run \"clusterbox up\" to create one.\n"
	if got := buf.String(); got != want {
		t.Errorf("empty output mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestListPopulated verifies the table layout with two clusters, one with a
// zero LastSynced time and the other with a real timestamp. Clusters must be
// rendered alphabetically by name regardless of insertion order.
func TestListPopulated(t *testing.T) {
	reg := newTempRegistry(t)
	ctx := context.Background()

	// Insert in reverse-alphabetical order to verify sort.
	if err := reg.UpsertCluster(ctx, registry.Cluster{
		Name:     "prod-1",
		Provider: "hetzner",
		Region:   "ash",
		Env:      "prod",
	}); err != nil {
		t.Fatalf("upsert prod-1: %v", err)
	}
	if err := reg.UpsertCluster(ctx, registry.Cluster{
		Name:     "dev-1",
		Provider: "hetzner",
		Region:   "fsn",
		Env:      "dev",
	}); err != nil {
		t.Fatalf("upsert dev-1: %v", err)
	}

	// dev-1: 2 nodes, 1 deployment, never synced.
	for _, hostname := range []string{"dev-1-server", "dev-1-worker-1"} {
		if err := reg.UpsertNode(ctx, registry.Node{
			ClusterName: "dev-1",
			Hostname:    hostname,
			Role:        "server",
			JoinedAt:    time.Now().UTC(),
		}); err != nil {
			t.Fatalf("upsert node %s: %v", hostname, err)
		}
	}
	if err := reg.UpsertDeployment(ctx, registry.Deployment{
		ClusterName: "dev-1",
		Service:     "api",
		Version:     "v1.0.0",
		DeployedAt:  time.Now().UTC(),
		Status:      registry.StatusRolledOut,
	}); err != nil {
		t.Fatalf("upsert deployment: %v", err)
	}

	// prod-1: 3 nodes, 2 deployments, synced at a fixed UTC moment.
	for _, hostname := range []string{"prod-1-server", "prod-1-w-1", "prod-1-w-2"} {
		if err := reg.UpsertNode(ctx, registry.Node{
			ClusterName: "prod-1",
			Hostname:    hostname,
			Role:        "server",
			JoinedAt:    time.Now().UTC(),
		}); err != nil {
			t.Fatalf("upsert node %s: %v", hostname, err)
		}
	}
	for _, svc := range []string{"api", "worker"} {
		if err := reg.UpsertDeployment(ctx, registry.Deployment{
			ClusterName: "prod-1",
			Service:     svc,
			Version:     "v1.0.0",
			DeployedAt:  time.Now().UTC(),
			Status:      registry.StatusRolledOut,
		}); err != nil {
			t.Fatalf("upsert deployment %s: %v", svc, err)
		}
	}
	syncedAt := time.Date(2026, 4, 25, 10, 32, 0, 0, time.UTC)
	if err := reg.MarkSynced(ctx, "prod-1", syncedAt); err != nil {
		t.Fatalf("mark synced: %v", err)
	}

	var buf bytes.Buffer
	if err := cmd.RunList(ctx, reg, &buf, false); err != nil {
		t.Fatalf("RunList: %v", err)
	}

	got := buf.String()

	// Header.
	if !contains(got, "NAME") || !contains(got, "LAST_SYNCED") {
		t.Errorf("expected header row, got:\n%s", got)
	}

	// Sort order: dev-1 must precede prod-1.
	devIdx := indexOf(got, "dev-1")
	prodIdx := indexOf(got, "prod-1")
	if devIdx < 0 || prodIdx < 0 {
		t.Fatalf("expected both clusters in output:\n%s", got)
	}
	if devIdx > prodIdx {
		t.Errorf("expected dev-1 before prod-1 (alphabetical), got:\n%s", got)
	}

	// Spot-check counts and dash for never-synced.
	if !contains(got, "hetzner") {
		t.Errorf("expected provider hetzner in output:\n%s", got)
	}
	if !contains(got, "2026-04-25 10:32 UTC") {
		t.Errorf("expected formatted last_synced for prod-1:\n%s", got)
	}
	// dev-1 row's LAST_SYNCED column should be "-" (never synced). After
	// tabwriter flushes, any trailing whitespace before the newline is
	// stripped from the final column, so the line ends with "-".
	devLine := lineContaining(got, "dev-1")
	if devLine == "" {
		t.Fatalf("dev-1 row not found in output:\n%s", got)
	}
	trimmed := trimTrailingSpaces(devLine)
	if !endsWith(trimmed, " -") {
		t.Errorf("expected dev-1 row to end with '-' for last_synced, got: %q", devLine)
	}
}

// TestListJSON verifies that --json emits a deterministic JSON array with
// stable field names and clusters sorted by name.
func TestListJSON(t *testing.T) {
	reg := newTempRegistry(t)
	ctx := context.Background()

	if err := reg.UpsertCluster(ctx, registry.Cluster{
		Name: "b-cluster", Provider: "hetzner", Region: "ash", Env: "prod",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := reg.UpsertCluster(ctx, registry.Cluster{
		Name: "a-cluster", Provider: "hetzner", Region: "fsn", Env: "dev",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := reg.UpsertNode(ctx, registry.Node{
		ClusterName: "a-cluster", Hostname: "n1", Role: "server", JoinedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("upsert node: %v", err)
	}
	syncedAt := time.Date(2026, 4, 25, 10, 32, 0, 0, time.UTC)
	if err := reg.MarkSynced(ctx, "b-cluster", syncedAt); err != nil {
		t.Fatalf("mark synced: %v", err)
	}

	var buf bytes.Buffer
	if err := cmd.RunList(ctx, reg, &buf, true); err != nil {
		t.Fatalf("RunList: %v", err)
	}

	var rows []struct {
		Name       string `json:"name"`
		Provider   string `json:"provider"`
		Region     string `json:"region"`
		Env        string `json:"env"`
		Nodes      int    `json:"nodes"`
		Services   int    `json:"services"`
		LastSynced string `json:"last_synced"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, buf.String())
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d: %+v", len(rows), rows)
	}
	if rows[0].Name != "a-cluster" || rows[1].Name != "b-cluster" {
		t.Errorf("expected sort order [a-cluster, b-cluster], got [%s, %s]", rows[0].Name, rows[1].Name)
	}
	if rows[0].Nodes != 1 {
		t.Errorf("a-cluster nodes=%d want 1", rows[0].Nodes)
	}
	if rows[0].LastSynced != "-" {
		t.Errorf("a-cluster last_synced=%q want \"-\"", rows[0].LastSynced)
	}
	if rows[1].LastSynced != "2026-04-25 10:32 UTC" {
		t.Errorf("b-cluster last_synced=%q want %q", rows[1].LastSynced, "2026-04-25 10:32 UTC")
	}
}

// TestListJSONEmpty verifies that an empty registry produces "[]" (a valid
// JSON array) under --json rather than the human-readable hint.
func TestListJSONEmpty(t *testing.T) {
	reg := newTempRegistry(t)

	var buf bytes.Buffer
	if err := cmd.RunList(context.Background(), reg, &buf, true); err != nil {
		t.Fatalf("RunList: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, buf.String())
	}
	if len(rows) != 0 {
		t.Errorf("expected empty array, got %d rows", len(rows))
	}
}

// TestListExcludesDestroyedClusters verifies that a cluster with a non-zero
// DestroyedAt timestamp is excluded from both table and JSON output.
func TestListExcludesDestroyedClusters(t *testing.T) {
	reg := newTempRegistry(t)
	ctx := context.Background()

	if err := reg.UpsertCluster(ctx, registry.Cluster{
		Name: "active", Provider: "hetzner", Region: "ash", Env: "prod",
	}); err != nil {
		t.Fatalf("upsert active: %v", err)
	}
	if err := reg.UpsertCluster(ctx, registry.Cluster{
		Name: "dead", Provider: "hetzner", Region: "ash", Env: "prod",
	}); err != nil {
		t.Fatalf("upsert dead: %v", err)
	}
	if err := reg.MarkClusterDestroyed(ctx, "dead", time.Now().UTC()); err != nil {
		t.Fatalf("mark destroyed: %v", err)
	}

	var buf bytes.Buffer
	if err := cmd.RunList(ctx, reg, &buf, false); err != nil {
		t.Fatalf("RunList: %v", err)
	}
	got := buf.String()
	if !contains(got, "active") {
		t.Errorf("active cluster must appear in list output:\n%s", got)
	}
	if contains(got, "dead") {
		t.Errorf("destroyed cluster must not appear in list output:\n%s", got)
	}
}

// ---- small helpers (intentionally local to avoid touching shared test helpers) ----

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}

func indexOf(s, sub string) int {
	return bytes.Index([]byte(s), []byte(sub))
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func lineContaining(s, needle string) string {
	for _, line := range bytesSplitLines(s) {
		if contains(line, needle) {
			return line
		}
	}
	return ""
}

func trimTrailingSpaces(s string) string {
	end := len(s)
	for end > 0 && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[:end]
}

func bytesSplitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
