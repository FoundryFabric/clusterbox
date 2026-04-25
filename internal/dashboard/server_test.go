package dashboard_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/dashboard"
	"github.com/foundryfabric/clusterbox/internal/registry"
	"github.com/foundryfabric/clusterbox/internal/registry/sqlite"
)

// stamp returns a deterministic UTC timestamp so rendered output is stable.
func stamp(seconds int) time.Time {
	return time.Unix(int64(seconds), 0).UTC()
}

// seedRegistry populates reg with two clusters so the list page has rows.
// It returns the cluster names in the order they were inserted.
func seedRegistry(t *testing.T, reg registry.Registry) []string {
	t.Helper()
	ctx := context.Background()

	type seed struct {
		cluster registry.Cluster
		nodes   []registry.Node
		deps    []registry.Deployment
	}

	seeds := []seed{
		{
			cluster: registry.Cluster{
				Name:           "alpha",
				Provider:       "kind",
				Region:         "local",
				Env:            "dev",
				CreatedAt:      stamp(1_700_000_000),
				KubeconfigPath: "/tmp/kubeconfig-alpha",
				LastSynced:     stamp(1_700_000_500),
			},
			nodes: []registry.Node{
				{ClusterName: "alpha", Hostname: "alpha-control", Role: "control-plane", JoinedAt: stamp(1_700_000_010)},
				{ClusterName: "alpha", Hostname: "alpha-worker-1", Role: "worker", JoinedAt: stamp(1_700_000_020)},
			},
			deps: []registry.Deployment{
				{ClusterName: "alpha", Service: "api", Version: "v1.2.3", DeployedAt: stamp(1_700_000_030), DeployedBy: "tester", Status: registry.StatusRolledOut},
				{ClusterName: "alpha", Service: "worker", Version: "v0.4.0", DeployedAt: stamp(1_700_000_040), DeployedBy: "tester", Status: registry.StatusRolledOut},
			},
		},
		{
			cluster: registry.Cluster{
				Name:           "beta",
				Provider:       "kind",
				Region:         "local",
				Env:            "staging",
				CreatedAt:      stamp(1_700_000_100),
				KubeconfigPath: "/tmp/kubeconfig-beta",
				// LastSynced left as zero — should render as "never".
			},
			nodes: []registry.Node{
				{ClusterName: "beta", Hostname: "beta-control", Role: "control-plane", JoinedAt: stamp(1_700_000_110)},
			},
			// no deployments
		},
	}

	for _, s := range seeds {
		if err := reg.UpsertCluster(ctx, s.cluster); err != nil {
			t.Fatalf("UpsertCluster %s: %v", s.cluster.Name, err)
		}
		for _, n := range s.nodes {
			if err := reg.UpsertNode(ctx, n); err != nil {
				t.Fatalf("UpsertNode %s/%s: %v", n.ClusterName, n.Hostname, err)
			}
		}
		for _, d := range s.deps {
			if err := reg.UpsertDeployment(ctx, d); err != nil {
				t.Fatalf("UpsertDeployment %s/%s: %v", d.ClusterName, d.Service, err)
			}
		}
	}

	return []string{"alpha", "beta"}
}

// newTempRegistry returns a real sqlite-backed registry rooted in t.TempDir.
// The dashboard scaffold doesn't query it yet, but holding a real Registry
// matches production wiring and exercises the same lifecycle.
func newTempRegistry(t *testing.T) registry.Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.db")
	p, err := sqlite.New(path)
	if err != nil {
		t.Fatalf("open sqlite registry: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// startServer constructs a dashboard server backed by a fresh empty
// registry and binds it to a random port on 127.0.0.1. It returns the base
// URL and registers a Cleanup that shuts the server down within 1s.
func startServer(t *testing.T) (string, *dashboard.Server) {
	t.Helper()
	return startServerWithRegistry(t, newTempRegistry(t))
}

// startServerWithRegistry is the seeded variant of startServer. Tests that
// need to populate the registry up front (e.g. history rows) build the
// registry, write to it, and then hand it to this function.
func startServerWithRegistry(t *testing.T, reg registry.Registry) (string, *dashboard.Server) {
	t.Helper()

	srv, err := dashboard.NewServer(reg, dashboard.Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.HTTPServer().Serve(ln)
	}()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case err := <-serveErr:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("serve returned unexpected error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("server did not shut down within 2s")
		}
	})

	return "http://" + ln.Addr().String(), srv
}

// TestHealthz_Returns200OK verifies GET /healthz responds 200 with body "ok".
func TestHealthz_Returns200OK(t *testing.T) {
	t.Parallel()
	base, _ := startServer(t)

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}
}

// TestIndex_RendersBaseTemplate verifies GET / returns 200 with markers from
// the base template (app name, nav links, meta refresh, page heading).
func TestIndex_RendersBaseTemplate(t *testing.T) {
	t.Parallel()
	base, _ := startServer(t)

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)

	for _, marker := range []string{
		"<title>",         // template was rendered, not raw
		dashboard.AppName, // chrome reflects the app name
		`<meta http-equiv="refresh" content="30">`, // 30s auto-refresh
		`<a href="/">Clusters</a>`,                 // primary nav links to clusters page
		"History (coming",                          // nav placeholder for T13
		"<h2>Clusters</h2>",                        // list page heading
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("response missing marker %q\n--- body ---\n%s", marker, got)
		}
	}

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
}

// TestClustersList_IncludesEveryName verifies that GET / renders one row
// per cluster recorded in the registry, with each name linking to the
// detail page.
func TestClustersList_IncludesEveryName(t *testing.T) {
	t.Parallel()
	reg := newTempRegistry(t)
	names := seedRegistry(t, reg)
	base, _ := startServerWithRegistry(t, reg)

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)

	for _, n := range names {
		if !strings.Contains(got, n) {
			t.Errorf("list missing cluster name %q\n--- body ---\n%s", n, got)
		}
		linkMarker := `href="/clusters/` + n + `"`
		if !strings.Contains(got, linkMarker) {
			t.Errorf("list missing detail link for %q (%q)", n, linkMarker)
		}
	}

	// Beta has no LastSynced — should render as "never" per formatTime.
	if !strings.Contains(got, "never") {
		t.Errorf("expected 'never' marker for beta's missing LastSynced\n%s", got)
	}
}

// TestClustersList_EmptyRegistry verifies GET / on an empty registry
// returns 200 with the empty-state message rather than a server error.
func TestClustersList_EmptyRegistry(t *testing.T) {
	t.Parallel()
	base, _ := startServer(t)

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "No clusters recorded") {
		t.Errorf("expected empty-state message\n--- body ---\n%s", body)
	}
}

// TestClusterDetail_IncludesNodesAndServices verifies that
// GET /clusters/{name} renders the cluster header, every node hostname,
// and every deployment service.
func TestClusterDetail_IncludesNodesAndServices(t *testing.T) {
	t.Parallel()
	reg := newTempRegistry(t)
	seedRegistry(t, reg)
	base, _ := startServerWithRegistry(t, reg)

	resp, err := http.Get(base + "/clusters/alpha")
	if err != nil {
		t.Fatalf("GET /clusters/alpha: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)

	for _, marker := range []string{
		"alpha",                           // header
		"kind",                            // provider
		"local",                           // region
		"dev",                             // env
		"/tmp/kubeconfig-alpha",           // kubeconfig path
		"alpha-control",                   // node hostname
		"alpha-worker-1",                  // node hostname
		"control-plane",                   // node role
		"api",                             // service
		"worker",                          // service
		"v1.2.3",                          // deployed version
		"href=\"/history?cluster=alpha\"", // T13 link pre-exists
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("detail page missing marker %q\n--- body ---\n%s", marker, got)
		}
	}

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
}

// TestClusterDetail_NeverSyncedRendersNever verifies that a cluster with
// no LastSynced renders "never" rather than a year-0001 timestamp.
func TestClusterDetail_NeverSyncedRendersNever(t *testing.T) {
	t.Parallel()
	reg := newTempRegistry(t)
	seedRegistry(t, reg)
	base, _ := startServerWithRegistry(t, reg)

	resp, err := http.Get(base + "/clusters/beta")
	if err != nil {
		t.Fatalf("GET /clusters/beta: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)

	if !strings.Contains(got, "never") {
		t.Errorf("expected 'never' for unset LastSynced\n%s", got)
	}
	// beta has no deployments — empty-state copy must surface.
	if !strings.Contains(got, "No deployments recorded") {
		t.Errorf("expected deployments empty-state\n%s", got)
	}
	if strings.Contains(got, "0001-01-01") {
		t.Errorf("zero time leaked into rendered output\n%s", got)
	}
}

// TestClusterDetail_UnknownReturns404 verifies that requesting an unknown
// cluster name yields 404, not 500 or a partially-rendered page.
func TestClusterDetail_UnknownReturns404(t *testing.T) {
	t.Parallel()
	base, _ := startServer(t)

	resp, err := http.Get(base + "/clusters/unknown")
	if err != nil {
		t.Fatalf("GET /clusters/unknown: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestIndex_UnknownPath404 verifies that paths other than "/" 404 instead of
// silently rendering the base page.
func TestIndex_UnknownPath404(t *testing.T) {
	t.Parallel()
	base, _ := startServer(t)

	resp, err := http.Get(base + "/does-not-exist")
	if err != nil {
		t.Fatalf("GET /does-not-exist: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestStatic_StyleCSSServed verifies the embedded /static/style.css is
// reachable. T12/T13 will rely on this, so failing now beats failing later.
func TestStatic_StyleCSSServed(t *testing.T) {
	t.Parallel()
	base, _ := startServer(t)

	resp, err := http.Get(base + "/static/style.css")
	if err != nil {
		t.Fatalf("GET /static/style.css: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "cb-header") {
		t.Fatalf("style.css missing expected selector")
	}
}

// TestShutdown_GracefulOnContextCancel verifies that a context-driven
// shutdown returns within the deadline, that ListenAndServe returns
// http.ErrServerClosed, and that IsClosed identifies that error.
func TestShutdown_GracefulOnContextCancel(t *testing.T) {
	t.Parallel()
	reg := newTempRegistry(t)

	srv, err := dashboard.NewServer(reg, dashboard.Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.HTTPServer().Serve(ln)
	}()

	// Give the goroutine a moment to actually start serving.
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case err := <-serveErr:
		if !dashboard.IsClosed(err) {
			t.Fatalf("serve returned %v, want http.ErrServerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Serve did not return after Shutdown")
	}
}

// TestNewServer_DefaultAddr verifies that an empty Options.Addr falls back
// to dashboard.DefaultAddr.
func TestNewServer_DefaultAddr(t *testing.T) {
	t.Parallel()
	reg := newTempRegistry(t)

	srv, err := dashboard.NewServer(reg, dashboard.Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if got, want := srv.Addr(), dashboard.DefaultAddr; got != want {
		t.Fatalf("Addr = %q, want %q", got, want)
	}
}

// historySeed describes one row to insert into a tempdir registry for the
// /history tests.
type historySeed struct {
	cluster  string
	service  string
	version  string
	at       time.Time
	status   registry.DeploymentStatus
	duration int64
	errMsg   string
}

// seedHistory writes the given entries to reg using AppendHistory. The base
// time anchors the AttemptedAt values so most-recent-first ordering is
// deterministic across test runs.
func seedHistory(t *testing.T, reg registry.Registry, entries []historySeed) {
	t.Helper()
	ctx := context.Background()
	for _, e := range entries {
		err := reg.AppendHistory(ctx, registry.DeploymentHistoryEntry{
			ClusterName:       e.cluster,
			Service:           e.service,
			Version:           e.version,
			AttemptedAt:       e.at,
			Status:            e.status,
			RolloutDurationMs: e.duration,
			Error:             e.errMsg,
		})
		if err != nil {
			t.Fatalf("AppendHistory(%+v): %v", e, err)
		}
	}
}

// fixedHistory returns a deterministic mix of 8 history rows: 6 successful,
// 2 failed, spread across two clusters and two services. The AttemptedAt
// values are spaced one minute apart so most-recent-first ordering is
// stable.
func fixedHistory() []historySeed {
	base := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	at := func(n int) time.Time { return base.Add(time.Duration(n) * time.Minute) }
	return []historySeed{
		{"alpha", "api", "v1.0.0", at(0), registry.StatusRolledOut, 1500, ""},
		{"alpha", "api", "v1.0.1", at(1), registry.StatusFailed, 800, "image pull backoff"},
		{"alpha", "web", "v2.0.0", at(2), registry.StatusRolledOut, 2200, ""},
		{"alpha", "web", "v2.0.1", at(3), registry.StatusRolledOut, 1900, ""},
		{"beta", "api", "v1.0.0", at(4), registry.StatusRolledOut, 1300, ""},
		{"beta", "api", "v1.0.2", at(5), registry.StatusFailed, 600, "readiness probe timeout"},
		{"beta", "web", "v2.1.0", at(6), registry.StatusRolledOut, 2500, ""},
		{"beta", "web", "v2.2.0", at(7), registry.StatusRolledOut, 2100, ""},
	}
}

// fetch is a small GET helper that reads the body and returns it together
// with the response so tests can assert on both.
func fetch(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(body)
}

// TestHistory_RendersAllEntriesUnfiltered verifies GET /history with no
// query string returns every seeded row, most-recent-first, with the
// expected page chrome (table, auto-refresh meta tag).
func TestHistory_RendersAllEntriesUnfiltered(t *testing.T) {
	t.Parallel()
	reg := newTempRegistry(t)
	seedHistory(t, reg, fixedHistory())
	base, _ := startServerWithRegistry(t, reg)

	resp, body := fetch(t, base+"/history")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}

	// Every cluster/service/version we seeded should appear.
	for _, want := range []string{
		"Deploy history",
		"v1.0.0", "v1.0.1", "v2.0.0", "v2.0.1",
		"v1.0.2", "v2.1.0", "v2.2.0",
		"image pull backoff",
		"readiness probe timeout",
		`href="/clusters/alpha"`,
		`href="/clusters/beta"`,
		`http-equiv="refresh"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}

	// Most-recent-first: the latest seed (offset 7) should appear before
	// the oldest (offset 0). We check by looking at version order.
	idxLatest := strings.Index(body, "v2.2.0")   // at(7)
	idxEarliest := strings.Index(body, "v1.0.0") // at(0)
	if idxLatest < 0 || idxEarliest < 0 {
		t.Fatalf("expected both v2.2.0 and v1.0.0 in body")
	}
	if idxLatest > idxEarliest {
		t.Errorf("expected v2.2.0 (most recent) before v1.0.0 (oldest); got latest=%d earliest=%d", idxLatest, idxEarliest)
	}
}

// TestHistory_FailedRowsCarryFailedClass verifies failed entries are the
// only rows whose <tr> has the .failed CSS class. The success rows must
// not carry it.
func TestHistory_FailedRowsCarryFailedClass(t *testing.T) {
	t.Parallel()
	reg := newTempRegistry(t)
	seedHistory(t, reg, fixedHistory())
	base, _ := startServerWithRegistry(t, reg)

	_, body := fetch(t, base+"/history")

	// Two failed rows were seeded; expect exactly two <tr class="failed">.
	got := strings.Count(body, `<tr class="failed">`)
	if got != 2 {
		t.Errorf("found %d <tr class=\"failed\"> rows, want 2\n--- body ---\n%s", got, body)
	}

	// And the .failed class itself must be defined in the stylesheet so
	// the rule actually styles anything. We fetch style.css and grep.
	_, css := fetch(t, base+"/static/style.css")
	if !strings.Contains(css, ".failed") {
		t.Errorf("style.css missing .failed selector")
	}
}

// TestHistory_FilterByCluster verifies ?cluster=X narrows the result set
// and excludes other clusters.
func TestHistory_FilterByCluster(t *testing.T) {
	t.Parallel()
	reg := newTempRegistry(t)
	seedHistory(t, reg, fixedHistory())
	base, _ := startServerWithRegistry(t, reg)

	_, body := fetch(t, base+"/history?cluster=alpha")

	if !strings.Contains(body, "v1.0.1") { // alpha row
		t.Errorf("expected alpha row v1.0.1 in body")
	}
	if strings.Contains(body, "v2.1.0") { // beta row
		t.Errorf("did not expect beta row v2.1.0 in body when filtering cluster=alpha")
	}
}

// TestHistory_FilterByClusterAndService verifies ?cluster=X&service=Y
// narrows further and the conjunction is AND, not OR.
func TestHistory_FilterByClusterAndService(t *testing.T) {
	t.Parallel()
	reg := newTempRegistry(t)
	seedHistory(t, reg, fixedHistory())
	base, _ := startServerWithRegistry(t, reg)

	_, body := fetch(t, base+"/history?cluster=alpha&service=api")

	// Should include alpha/api rows.
	for _, want := range []string{"v1.0.0", "v1.0.1"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in alpha/api results", want)
		}
	}
	// Should exclude alpha/web rows.
	for _, blocked := range []string{"v2.0.0", "v2.0.1"} {
		if strings.Contains(body, blocked) {
			t.Errorf("did not expect alpha/web row %q in alpha/api results", blocked)
		}
	}
	// Should exclude beta rows entirely.
	if strings.Contains(body, "v2.1.0") || strings.Contains(body, "v2.2.0") {
		t.Errorf("did not expect any beta rows in alpha/api results")
	}
}

// TestHistory_LimitClampsToCeiling verifies that an absurdly large ?limit
// is clamped to maxHistoryLimit (no DB-side OOM, no goroutine pinned for
// minutes). We can't observe the clamp directly without exposing internals,
// but we can confirm the request still succeeds and renders.
func TestHistory_LimitHonoured(t *testing.T) {
	t.Parallel()
	reg := newTempRegistry(t)
	seedHistory(t, reg, fixedHistory())
	base, _ := startServerWithRegistry(t, reg)

	// limit=2 should drop most rows. The two newest in our fixed seed are
	// v2.2.0 (offset 7) and v2.1.0 (offset 6).
	_, body := fetch(t, base+"/history?limit=2")

	for _, want := range []string{"v2.2.0", "v2.1.0"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q with limit=2", want)
		}
	}
	for _, blocked := range []string{"v1.0.0", "v1.0.1", "v1.0.2"} {
		if strings.Contains(body, blocked) {
			t.Errorf("did not expect %q with limit=2", blocked)
		}
	}
}

// TestHistory_LimitInvalidFallsBackToDefault verifies that non-numeric or
// negative ?limit values fall back to the default rather than 500ing.
func TestHistory_LimitInvalidFallsBackToDefault(t *testing.T) {
	t.Parallel()
	reg := newTempRegistry(t)
	seedHistory(t, reg, fixedHistory())
	base, _ := startServerWithRegistry(t, reg)

	for _, raw := range []string{"abc", "-5", "0", "", "9999999999999999999999"} {
		url := base + "/history?limit=" + raw
		resp, body := fetch(t, url)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("limit=%q: status = %d, want 200", raw, resp.StatusCode)
			continue
		}
		// Default behaviour: every seeded row renders (we have only 8).
		if !strings.Contains(body, "v1.0.0") || !strings.Contains(body, "v2.2.0") {
			t.Errorf("limit=%q: expected default to include all 8 seeded rows", raw)
		}
	}
}

// TestHistory_EmptyRegistry verifies the page renders cleanly when there
// are no history rows at all (no panic, no broken HTML, an explanatory
// message).
func TestHistory_EmptyRegistry(t *testing.T) {
	t.Parallel()
	base, _ := startServer(t)

	resp, body := fetch(t, base+"/history")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "No deploy history") {
		t.Errorf("empty body should include explanatory message; got:\n%s", body)
	}
	// Auto-refresh tag must still be present.
	if !strings.Contains(body, `http-equiv="refresh"`) {
		t.Errorf("auto-refresh meta tag missing on empty page")
	}
}

// TestHistory_UnknownSubpath404 verifies that /history/anything-else 404s
// rather than silently rendering the history page.
func TestHistory_UnknownSubpath404(t *testing.T) {
	t.Parallel()
	base, _ := startServer(t)

	resp, _ := fetch(t, base+"/history/extra")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestHistory_TruncatesLongErrorMessage verifies a multi-hundred-character
// error string is shortened on the page so the table stays readable.
func TestHistory_TruncatesLongErrorMessage(t *testing.T) {
	t.Parallel()
	reg := newTempRegistry(t)
	long := strings.Repeat("x", 500)
	seedHistory(t, reg, []historySeed{
		{
			cluster:  "alpha",
			service:  "api",
			version:  "v1.0.0",
			at:       time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC),
			status:   registry.StatusFailed,
			duration: 100,
			errMsg:   long,
		},
	})
	base, _ := startServerWithRegistry(t, reg)

	_, body := fetch(t, base+"/history")
	if strings.Contains(body, long) {
		t.Errorf("expected long error to be truncated, but full %d-char string is present", len(long))
	}
	if !strings.Contains(body, "…") {
		t.Errorf("expected truncation ellipsis in body")
	}
}
