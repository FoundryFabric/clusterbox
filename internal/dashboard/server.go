// Package dashboard provides an HTTP server that renders a local web UI for
// inspecting clusterbox state.
//
// This package is the scaffolding used by the `clusterbox dashboard` command.
// Subsequent tasks layer cluster pages and deployment-history views on top.
// The server uses only the standard library — net/http + html/template — and
// embeds its templates and static assets via go:embed.
package dashboard

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/foundryfabric/clusterbox/internal/registry"
)

// AppName is the human-readable name displayed in the dashboard chrome.
const AppName = "clusterbox"

// DefaultAddr is the listen address used when the caller does not override it.
const DefaultAddr = "127.0.0.1:7777"

// readHeaderTimeout caps how long the server waits for the request headers.
// Mitigates Slowloris-style attacks even on a loopback listener.
const readHeaderTimeout = 5 * time.Second

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Options configures NewServer. The zero value is valid and yields a server
// listening on DefaultAddr.
type Options struct {
	// Addr is the listen address. When empty, DefaultAddr is used.
	Addr string
}

// Server bundles the http.Server with the per-request dependencies the
// handlers close over. Use NewServer to construct one.
type Server struct {
	httpSrv *http.Server
	reg     registry.Registry
	tpl     *template.Template
}

// NewServer wires templates, static assets, and handlers into an
// *http.Server. The server is not started; the caller invokes ListenAndServe
// (or hands the underlying *http.Server to httptest).
//
// reg is held by the server for use by handlers added in future tasks. /
// and /healthz do not currently read from it. The server does NOT take
// ownership of reg — the caller is responsible for closing it.
func NewServer(reg registry.Registry, opts Options) (*Server, error) {
	if opts.Addr == "" {
		opts.Addr = DefaultAddr
	}

	tpl, err := parseTemplates()
	if err != nil {
		return nil, fmt.Errorf("dashboard: parse templates: %w", err)
	}

	mux := http.NewServeMux()

	s := &Server{
		reg: reg,
		tpl: tpl,
	}

	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.Handle("/static/", http.StripPrefix("/static/", staticFileHandler()))
	mux.HandleFunc("/", s.handleIndex)

	s.httpSrv = &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	return s, nil
}

// HTTPServer exposes the underlying *http.Server for callers that want to
// drive it directly (notably httptest in unit tests).
func (s *Server) HTTPServer() *http.Server { return s.httpSrv }

// Addr returns the configured listen address.
func (s *Server) Addr() string { return s.httpSrv.Addr }

// ListenAndServe starts the HTTP server. It returns http.ErrServerClosed
// after a successful Shutdown; callers should treat that as a clean exit.
func (s *Server) ListenAndServe() error {
	return s.httpSrv.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server, waiting for in-flight requests
// up to the deadline carried by ctx. It does NOT close the registry — that
// belongs to whoever opened it.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}

// handleHealthz is a process-liveness probe. It does not consult the
// registry so that a degraded backend still reports the server as alive.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// indexData is the template payload for the base page.
type indexData struct {
	AppName   string
	PageTitle string
}

// handleIndex renders the base scaffolding page. The catch-all "/" route
// 404s any path that does not exactly match "/", matching net/http's default
// ServeMux semantics.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data := indexData{
		AppName:   AppName,
		PageTitle: "Overview",
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, "base.html", data); err != nil {
		// At this point headers may already be flushed; logging the error
		// without trying to overwrite the status keeps the response well
		// formed.
		fmt.Fprintf(errOut, "dashboard: render base.html: %v\n", err)
	}
}

// errOut is the destination used for handler-side error logging. It is a
// package variable rather than a hard-coded os.Stderr reference so tests
// can capture output deterministically.
var errOut io.Writer = os.Stderr

// parseTemplates parses every embedded *.html file once, at server start.
// html/template templates are safe for concurrent execution.
func parseTemplates() (*template.Template, error) {
	sub, err := fs.Sub(templatesFS, "templates")
	if err != nil {
		return nil, fmt.Errorf("locate templates dir: %w", err)
	}
	tpl, err := template.New("").ParseFS(sub, "*.html")
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return tpl, nil
}

// staticFileHandler serves the embedded static/ subtree. Returning a single
// http.Handler means we parse the embedded FS once and reuse it.
var (
	staticHandlerOnce sync.Once
	staticHandler     http.Handler
	staticHandlerErr  error
)

func staticFileHandler() http.Handler {
	staticHandlerOnce.Do(func() {
		sub, err := fs.Sub(staticFS, "static")
		if err != nil {
			staticHandlerErr = err
			return
		}
		staticHandler = http.FileServer(http.FS(sub))
	})
	if staticHandlerErr != nil {
		// This branch is unreachable in practice — fs.Sub on an embedded
		// path that exists at compile time cannot fail at runtime — but a
		// defensive 500 keeps the handler total.
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, staticHandlerErr.Error(), http.StatusInternalServerError)
		})
	}
	return staticHandler
}

// IsClosed reports whether err is the benign http.ErrServerClosed value
// returned by ListenAndServe after Shutdown.
func IsClosed(err error) bool {
	return errors.Is(err, http.ErrServerClosed)
}
