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

// startServerWithRegistry mirrors startServer but lets the caller seed the
// registry first.
func startServerWithRegistry(t *testing.T, reg registry.Registry) string {
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

	return "http://" + ln.Addr().String()
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

// startServer constructs a dashboard server and binds it to a random port on
// 127.0.0.1. It returns the base URL and registers a Cleanup that shuts the
// server down within 1s.
func startServer(t *testing.T) (string, *dashboard.Server) {
	t.Helper()
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
	base := startServerWithRegistry(t, reg)

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
	base := startServerWithRegistry(t, reg)

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
	base := startServerWithRegistry(t, reg)

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
