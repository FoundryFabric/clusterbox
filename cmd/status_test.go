package cmd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/cmd"
	"github.com/foundryfabric/clusterbox/internal/registry"
)

// hasHeaderLine reports whether out contains a line of the form
// "<key>:<whitespace><value>" — accommodating tabwriter's variable padding.
func hasHeaderLine(out, key, value string) bool {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `:\s+` + regexp.QuoteMeta(value) + `\s*$`)
	return re.MatchString(out)
}

// fixedTime returns a deterministic UTC moment used across the status tests so
// the rendered output is stable regardless of when the test runs.
func fixedTime(min int) time.Time {
	return time.Date(2026, 4, 25, 10, min, 0, 0, time.UTC)
}

// seedStatusFixture creates a "alpha" cluster with two nodes and two
// deployments, all with deterministic timestamps. It is used by the populated
// golden tests for both text and JSON output.
func seedStatusFixture(t *testing.T, reg registry.Registry) {
	t.Helper()
	ctx := context.Background()

	if err := reg.UpsertCluster(ctx, registry.Cluster{
		Name:           "alpha",
		Provider:       "hetzner",
		Region:         "fsn",
		Env:            "dev",
		CreatedAt:      fixedTime(0),
		KubeconfigPath: "/tmp/alpha.kubeconfig",
	}); err != nil {
		t.Fatalf("upsert cluster: %v", err)
	}

	// Insert nodes out of alphabetical order to verify sort.
	if err := reg.UpsertNode(ctx, registry.Node{
		ClusterName: "alpha", Hostname: "alpha-w-1", Role: "agent", JoinedAt: fixedTime(2),
	}); err != nil {
		t.Fatalf("upsert node w-1: %v", err)
	}
	if err := reg.UpsertNode(ctx, registry.Node{
		ClusterName: "alpha", Hostname: "alpha-server", Role: "server", JoinedAt: fixedTime(1),
	}); err != nil {
		t.Fatalf("upsert node server: %v", err)
	}

	// Insert deployments out of alphabetical order to verify sort.
	if err := reg.UpsertDeployment(ctx, registry.Deployment{
		ClusterName: "alpha", Service: "worker", Version: "v0.2.0",
		DeployedAt: fixedTime(4), DeployedBy: "chris",
		Status: registry.StatusRolledOut,
	}); err != nil {
		t.Fatalf("upsert deployment worker: %v", err)
	}
	if err := reg.UpsertDeployment(ctx, registry.Deployment{
		ClusterName: "alpha", Service: "api", Version: "v1.0.0",
		DeployedAt: fixedTime(3), DeployedBy: "chris",
		Status: registry.StatusRolledOut,
	}); err != nil {
		t.Fatalf("upsert deployment api: %v", err)
	}

	if err := reg.MarkSynced(ctx, "alpha", fixedTime(5)); err != nil {
		t.Fatalf("mark synced: %v", err)
	}
}

// TestStatusPopulatedGolden is the primary text-format golden: cluster header
// followed by nodes and deployments tables, each separated by a blank line.
func TestStatusPopulatedGolden(t *testing.T) {
	reg := newTempRegistry(t)
	seedStatusFixture(t, reg)

	var out, errOut bytes.Buffer
	if err := cmd.RunStatus(context.Background(), reg, &out, &errOut, "alpha", false); err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if errOut.Len() != 0 {
		t.Errorf("expected empty stderr, got: %q", errOut.String())
	}

	got := out.String()

	// Header values. tabwriter pads "key:" with variable whitespace before the
	// value, so we match each row with a regex rather than substring.
	for _, kv := range [][2]string{
		{"name", "alpha"},
		{"provider", "hetzner"},
		{"region", "fsn"},
		{"env", "dev"},
		{"created_at", "2026-04-25 10:00 UTC"},
		{"kubeconfig_path", "/tmp/alpha.kubeconfig"},
		{"last_synced", "2026-04-25 10:05 UTC"},
	} {
		if !hasHeaderLine(got, kv[0], kv[1]) {
			t.Errorf("expected header %q: %q in output:\n%s", kv[0], kv[1], got)
		}
	}

	// Section table headers.
	if !strings.Contains(got, "HOSTNAME") || !strings.Contains(got, "JOINED_AT") {
		t.Errorf("expected nodes table header, got:\n%s", got)
	}
	if !strings.Contains(got, "SERVICE") || !strings.Contains(got, "DEPLOYED_BY") {
		t.Errorf("expected deployments table header, got:\n%s", got)
	}

	// Sort: alpha-server before alpha-w-1.
	srvIdx := strings.Index(got, "alpha-server")
	wIdx := strings.Index(got, "alpha-w-1")
	if srvIdx < 0 || wIdx < 0 || srvIdx > wIdx {
		t.Errorf("expected alpha-server before alpha-w-1:\n%s", got)
	}

	// Sort: api before worker. Match start-of-line to avoid colliding with
	// the "api" substring in "kubeconfig_path".
	apiRe := regexp.MustCompile(`(?m)^api\s`)
	workerRe := regexp.MustCompile(`(?m)^worker\s`)
	apiIdx := apiRe.FindStringIndex(got)
	workerIdx := workerRe.FindStringIndex(got)
	if apiIdx == nil || workerIdx == nil || apiIdx[0] > workerIdx[0] {
		t.Errorf("expected api before worker in deployments:\n%s", got)
	}

	// Section separation: exactly two blank lines (one between header/nodes,
	// one between nodes/deployments). We count "\n\n" occurrences.
	if c := strings.Count(got, "\n\n"); c != 2 {
		t.Errorf("expected 2 blank-line separators, got %d:\n%s", c, got)
	}

	// Status value rendered.
	if !strings.Contains(got, "rolled_out") {
		t.Errorf("expected status 'rolled_out' in output:\n%s", got)
	}
}

// TestStatusEmptyTables verifies the "(no nodes)" and "(no deployments)"
// markers when the cluster exists but has no children rows.
func TestStatusEmptyTables(t *testing.T) {
	reg := newTempRegistry(t)
	ctx := context.Background()

	if err := reg.UpsertCluster(ctx, registry.Cluster{
		Name: "bare", Provider: "hetzner", Region: "fsn", Env: "dev",
		CreatedAt: fixedTime(0),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var out, errOut bytes.Buffer
	if err := cmd.RunStatus(ctx, reg, &out, &errOut, "bare", false); err != nil {
		t.Fatalf("RunStatus: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "(no nodes)") {
		t.Errorf("expected '(no nodes)' marker, got:\n%s", got)
	}
	if !strings.Contains(got, "(no deployments)") {
		t.Errorf("expected '(no deployments)' marker, got:\n%s", got)
	}
	// Zero LastSynced renders as "-".
	if !hasHeaderLine(got, "last_synced", "-") {
		t.Errorf("expected last_synced '-' for never-synced cluster:\n%s", got)
	}
}

// TestStatusNotFound asserts the stderr message and the returned ErrNotFound
// chain that lets cobra's Execute exit non-zero.
func TestStatusNotFound(t *testing.T) {
	reg := newTempRegistry(t)

	var out, errOut bytes.Buffer
	err := cmd.RunStatus(context.Background(), reg, &out, &errOut, "ghost", false)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("expected ErrNotFound chain, got %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected empty stdout, got: %q", out.String())
	}
	want := "cluster \"ghost\" not found in registry\n"
	if got := errOut.String(); got != want {
		t.Errorf("stderr mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestStatusJSON verifies the {cluster, nodes, deployments} envelope.
func TestStatusJSON(t *testing.T) {
	reg := newTempRegistry(t)
	seedStatusFixture(t, reg)

	var out, errOut bytes.Buffer
	if err := cmd.RunStatus(context.Background(), reg, &out, &errOut, "alpha", true); err != nil {
		t.Fatalf("RunStatus: %v", err)
	}

	var doc struct {
		Cluster struct {
			Name           string `json:"name"`
			Provider       string `json:"provider"`
			Region         string `json:"region"`
			Env            string `json:"env"`
			CreatedAt      string `json:"created_at"`
			KubeconfigPath string `json:"kubeconfig_path"`
			LastSynced     string `json:"last_synced"`
		} `json:"cluster"`
		Nodes []struct {
			Hostname string `json:"hostname"`
			Role     string `json:"role"`
			JoinedAt string `json:"joined_at"`
		} `json:"nodes"`
		Deployments []struct {
			Service    string `json:"service"`
			Version    string `json:"version"`
			DeployedAt string `json:"deployed_at"`
			DeployedBy string `json:"deployed_by"`
			Status     string `json:"status"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, out.String())
	}

	if doc.Cluster.Name != "alpha" || doc.Cluster.Provider != "hetzner" {
		t.Errorf("cluster header mismatch: %+v", doc.Cluster)
	}
	if doc.Cluster.CreatedAt != "2026-04-25 10:00 UTC" {
		t.Errorf("created_at=%q want %q", doc.Cluster.CreatedAt, "2026-04-25 10:00 UTC")
	}
	if doc.Cluster.LastSynced != "2026-04-25 10:05 UTC" {
		t.Errorf("last_synced=%q want %q", doc.Cluster.LastSynced, "2026-04-25 10:05 UTC")
	}

	if len(doc.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(doc.Nodes))
	}
	if doc.Nodes[0].Hostname != "alpha-server" || doc.Nodes[1].Hostname != "alpha-w-1" {
		t.Errorf("node sort order: got %s,%s want alpha-server,alpha-w-1",
			doc.Nodes[0].Hostname, doc.Nodes[1].Hostname)
	}

	if len(doc.Deployments) != 2 {
		t.Fatalf("expected 2 deployments, got %d", len(doc.Deployments))
	}
	if doc.Deployments[0].Service != "api" || doc.Deployments[1].Service != "worker" {
		t.Errorf("deployment sort order: got %s,%s want api,worker",
			doc.Deployments[0].Service, doc.Deployments[1].Service)
	}
	if doc.Deployments[0].Status != "rolled_out" {
		t.Errorf("deployment[0].status=%q want rolled_out", doc.Deployments[0].Status)
	}
}

// TestStatusJSONEmpty verifies that empty nodes and deployments serialise as
// JSON arrays, not null.
func TestStatusJSONEmpty(t *testing.T) {
	reg := newTempRegistry(t)
	ctx := context.Background()
	if err := reg.UpsertCluster(ctx, registry.Cluster{
		Name: "bare", Provider: "hetzner", Region: "fsn", Env: "dev",
		CreatedAt: fixedTime(0),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var out, errOut bytes.Buffer
	if err := cmd.RunStatus(ctx, reg, &out, &errOut, "bare", true); err != nil {
		t.Fatalf("RunStatus: %v", err)
	}

	if !strings.Contains(out.String(), `"nodes": []`) {
		t.Errorf("expected empty JSON array for nodes, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"deployments": []`) {
		t.Errorf("expected empty JSON array for deployments, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"last_synced": "-"`) {
		t.Errorf("expected '-' for last_synced under JSON, got:\n%s", out.String())
	}
}
