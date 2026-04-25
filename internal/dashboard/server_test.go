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
// the base template (app name, nav placeholder).
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
		"<title>",          // template was rendered, not raw
		dashboard.AppName,  // chrome reflects the app name
		"Overview",         // page title
		"Clusters (coming", // nav placeholder for T12
		"History (coming",  // nav placeholder for T13
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("response missing marker %q\n--- body ---\n%s", marker, got)
		}
	}

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
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
