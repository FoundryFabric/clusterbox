package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/foundryfabric/clusterbox/internal/dashboard"
	"github.com/foundryfabric/clusterbox/internal/registry"
	_ "github.com/foundryfabric/clusterbox/internal/registry/sqlite"
	"github.com/spf13/cobra"
)

// dashboardFlags holds CLI flags for the dashboard command.
type dashboardFlags struct {
	addr      string
	noBrowser bool
}

var dashboardF dashboardFlags

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Run the local clusterbox dashboard web UI",
	Long: `Dashboard starts a local HTTP server that renders a web UI for
inspecting clusters, nodes, and deployment history tracked by clusterbox.

The server binds to 127.0.0.1:7777 by default and will attempt to open the
default browser at the served URL. Pass --no-browser to skip that.`,
	RunE: runDashboard,
}

func init() {
	dashboardCmd.Flags().StringVar(&dashboardF.addr, "addr", dashboard.DefaultAddr, "Listen address for the dashboard server")
	dashboardCmd.Flags().BoolVar(&dashboardF.noBrowser, "no-browser", false, "Do not attempt to open the dashboard URL in a browser")
}

// runDashboard is the cobra RunE handler for `clusterbox dashboard`.
//
// The handler is deliberately thin: it opens the registry, constructs the
// dashboard server, optionally opens a browser, then runs until SIGINT or
// SIGTERM trigger a graceful shutdown.
func runDashboard(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	reg, err := registry.NewRegistry(ctx)
	if err != nil {
		return fmt.Errorf("dashboard: open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	srv, err := dashboard.NewServer(reg, dashboard.Options{Addr: dashboardF.addr})
	if err != nil {
		return fmt.Errorf("dashboard: build server: %w", err)
	}

	// Listen up front so that if the address is in use we report it before
	// spawning a browser, and so that we know the actual port when :0 is
	// requested.
	ln, err := net.Listen("tcp", srv.Addr())
	if err != nil {
		return fmt.Errorf("dashboard: listen on %s: %w", srv.Addr(), err)
	}

	url := dashboardURL(ln.Addr().String())
	fmt.Fprintf(cmd.OutOrStdout(), "clusterbox dashboard listening on %s\n", url)

	if !dashboardF.noBrowser {
		openBrowser(cmd.ErrOrStderr(), url)
	}

	// Trap SIGINT/SIGTERM and translate to a context cancellation. We use
	// signal.NotifyContext so the standard library handles the channel
	// plumbing for us.
	sigCtx, stopSigs := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSigs()

	// Run the HTTP server in a goroutine so the main goroutine can wait
	// on the signal context.
	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- srv.HTTPServer().Serve(ln)
	}()

	select {
	case <-sigCtx.Done():
		// Fallthrough to shutdown.
	case err := <-serveErrCh:
		if err != nil && !dashboard.IsClosed(err) {
			return fmt.Errorf("dashboard: serve: %w", err)
		}
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("dashboard: shutdown: %w", err)
	}

	// Wait for Serve to actually return so we don't drop a useful error.
	if err := <-serveErrCh; err != nil && !dashboard.IsClosed(err) {
		return fmt.Errorf("dashboard: serve: %w", err)
	}
	return nil
}

// dashboardURL builds the user-facing URL given a net.Listener address. If
// the listener is bound to 0.0.0.0 we substitute 127.0.0.1 so the printed
// URL is something the user can actually click.
func dashboardURL(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "http://" + listenAddr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s", net.JoinHostPort(host, port))
}

// openBrowser tries to open url in the user's default browser. Failures are
// reported on errOut as warnings and never bubble up; the dashboard remains
// usable via copy-paste.
func openBrowser(errOut io.Writer, url string) {
	cmd, err := browserOpenCmd(url)
	if err != nil {
		fmt.Fprintf(errOut, "warning: cannot auto-open browser (%v); visit %s manually\n", err, url)
		return
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(errOut, "warning: failed to launch browser (%v); visit %s manually\n", err, url)
		return
	}
	// Detach: we don't care about the exit status of `open`/`xdg-open`.
	go func() { _ = cmd.Wait() }()
}

// browserOpenCmd returns an *exec.Cmd that, when started, opens url in the
// user's default browser on the current OS. Returning an error is a clean
// way to handle unsupported platforms.
func browserOpenCmd(url string) (*exec.Cmd, error) {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url), nil
	case "linux":
		return exec.Command("xdg-open", url), nil
	case "windows":
		// `start` is a cmd.exe builtin, hence the explicit shell.
		return exec.Command("cmd", "/c", "start", "", url), nil
	default:
		return nil, errors.New("unsupported platform")
	}
}
