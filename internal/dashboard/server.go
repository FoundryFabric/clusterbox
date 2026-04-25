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

// timestampLayout is the UTC format used to render every timestamp the
// dashboard surfaces. Centralising it here keeps the list and detail pages
// visually consistent.
const timestampLayout = "2006-01-02 15:04:05 UTC"

// neverSyncedDisplay is what the UI shows when a cluster has no
// last-synced-at value (zero time).
const neverSyncedDisplay = "never"

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

	// Each page is parsed into its own clone of base.html so that per-page
	// {{define "content"}} blocks do not collide. Storing the per-page
	// template here means parsing happens exactly once, at construction.
	tplIndex         *template.Template
	tplClusters      *template.Template
	tplClusterDetail *template.Template
}

// NewServer wires templates, static assets, and handlers into an
// *http.Server. The server is not started; the caller invokes ListenAndServe
// (or hands the underlying *http.Server to httptest).
//
// reg is read by the cluster pages on every request. The server does NOT
// take ownership of reg — the caller is responsible for closing it.
func NewServer(reg registry.Registry, opts Options) (*Server, error) {
	if opts.Addr == "" {
		opts.Addr = DefaultAddr
	}

	tplIndex, err := parsePage("")
	if err != nil {
		return nil, fmt.Errorf("dashboard: parse base template: %w", err)
	}
	tplClusters, err := parsePage("clusters.html")
	if err != nil {
		return nil, fmt.Errorf("dashboard: parse clusters template: %w", err)
	}
	tplClusterDetail, err := parsePage("cluster_detail.html")
	if err != nil {
		return nil, fmt.Errorf("dashboard: parse cluster_detail template: %w", err)
	}

	mux := http.NewServeMux()

	s := &Server{
		reg:              reg,
		tplIndex:         tplIndex,
		tplClusters:      tplClusters,
		tplClusterDetail: tplClusterDetail,
	}

	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.Handle("/static/", http.StripPrefix("/static/", staticFileHandler()))
	mux.HandleFunc("GET /clusters/{name}", s.handleClusterDetail)
	mux.HandleFunc("/", s.handleClusters)

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

// pageData is the common payload header rendered into base.html.
type pageData struct {
	AppName   string
	PageTitle string
}

// clusterRow is one row of the cluster list page.
type clusterRow struct {
	Cluster           registry.Cluster
	NodeCount         int
	ServiceCount      int
	LastSyncedDisplay string
}

// clustersPage is the payload for the cluster list page.
type clustersPage struct {
	pageData
	Rows []clusterRow
}

// nodeRow is one row in the cluster-detail nodes table.
type nodeRow struct {
	Node            registry.Node
	JoinedAtDisplay string
}

// deploymentRow is one row in the cluster-detail deployments table.
type deploymentRow struct {
	Deployment        registry.Deployment
	DeployedAtDisplay string
}

// clusterDetailPage is the payload for the cluster-detail page.
type clusterDetailPage struct {
	pageData
	Cluster           registry.Cluster
	CreatedAtDisplay  string
	LastSyncedDisplay string
	Nodes             []nodeRow
	Deployments       []deploymentRow
}

// handleClusters renders the cluster list page at "/". Any other path is
// 404'd here so unknown URLs do not silently render an empty page.
func (s *Server) handleClusters(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	clusters, err := s.reg.ListClusters(ctx)
	if err != nil {
		http.Error(w, "list clusters: "+err.Error(), http.StatusInternalServerError)
		return
	}

	rows := make([]clusterRow, 0, len(clusters))
	for _, c := range clusters {
		nodes, err := s.reg.ListNodes(ctx, c.Name)
		if err != nil {
			http.Error(w, "list nodes: "+err.Error(), http.StatusInternalServerError)
			return
		}
		deps, err := s.reg.ListDeployments(ctx, c.Name)
		if err != nil {
			http.Error(w, "list deployments: "+err.Error(), http.StatusInternalServerError)
			return
		}
		rows = append(rows, clusterRow{
			Cluster:           c,
			NodeCount:         len(nodes),
			ServiceCount:      len(deps),
			LastSyncedDisplay: formatTime(c.LastSynced),
		})
	}

	page := clustersPage{
		pageData: pageData{AppName: AppName, PageTitle: "Clusters"},
		Rows:     rows,
	}

	render(w, s.tplClusters, page)
}

// handleClusterDetail renders the cluster-detail page at /clusters/{name}.
// An unknown cluster yields a 404; any other registry error becomes a 500.
func (s *Server) handleClusterDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	cluster, err := s.reg.GetCluster(ctx, name)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "get cluster: "+err.Error(), http.StatusInternalServerError)
		return
	}

	nodes, err := s.reg.ListNodes(ctx, name)
	if err != nil {
		http.Error(w, "list nodes: "+err.Error(), http.StatusInternalServerError)
		return
	}
	deps, err := s.reg.ListDeployments(ctx, name)
	if err != nil {
		http.Error(w, "list deployments: "+err.Error(), http.StatusInternalServerError)
		return
	}

	nodeRows := make([]nodeRow, 0, len(nodes))
	for _, n := range nodes {
		nodeRows = append(nodeRows, nodeRow{
			Node:            n,
			JoinedAtDisplay: formatTime(n.JoinedAt),
		})
	}
	depRows := make([]deploymentRow, 0, len(deps))
	for _, d := range deps {
		depRows = append(depRows, deploymentRow{
			Deployment:        d,
			DeployedAtDisplay: formatTime(d.DeployedAt),
		})
	}

	page := clusterDetailPage{
		pageData:          pageData{AppName: AppName, PageTitle: cluster.Name},
		Cluster:           cluster,
		CreatedAtDisplay:  formatTime(cluster.CreatedAt),
		LastSyncedDisplay: formatTime(cluster.LastSynced),
		Nodes:             nodeRows,
		Deployments:       depRows,
	}

	render(w, s.tplClusterDetail, page)
}

// render writes tpl out as text/html, executing base.html (which all
// page templates extend via the "content" block).
func render(w http.ResponseWriter, tpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, "base.html", data); err != nil {
		// At this point headers may already be flushed; logging the error
		// without trying to overwrite the status keeps the response well
		// formed.
		fmt.Fprintf(errOut, "dashboard: render: %v\n", err)
	}
}

// formatTime returns a UTC-formatted timestamp, or neverSyncedDisplay for
// the zero time. Centralising the rule means a never-deployed cluster reads
// "never" instead of leaking "0001-01-01 ..." into the UI.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return neverSyncedDisplay
	}
	return t.UTC().Format(timestampLayout)
}

// errOut is the destination used for handler-side error logging. It is a
// package variable rather than a hard-coded os.Stderr reference so tests
// can capture output deterministically.
var errOut io.Writer = os.Stderr

// parsePage returns a template that owns base.html plus, optionally, one
// page-specific file. Each page gets its own *template.Template so that
// the {{define "content"}} blocks do not collide across pages.
//
// pageFile may be empty, in which case only base.html is parsed and the
// default {{block "content"}} body is used.
func parsePage(pageFile string) (*template.Template, error) {
	sub, err := fs.Sub(templatesFS, "templates")
	if err != nil {
		return nil, fmt.Errorf("locate templates dir: %w", err)
	}
	tpl, err := template.New("base.html").ParseFS(sub, "base.html")
	if err != nil {
		return nil, fmt.Errorf("parse base.html: %w", err)
	}
	if pageFile != "" {
		tpl, err = tpl.ParseFS(sub, pageFile)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", pageFile, err)
		}
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
