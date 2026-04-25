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
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

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

// clusterListAutoRefreshSeconds is the auto-refresh value applied to every
// page that surfaces cluster state. T11 hard-coded a 30s refresh in
// base.html; T13 made the meta tag conditional on AutoRefreshSeconds, so we
// thread the same 30s through layoutData to preserve behaviour.
const clusterListAutoRefreshSeconds = 30

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
	tplHistory       *template.Template
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
	tplHistory, err := parsePage("history.html")
	if err != nil {
		return nil, fmt.Errorf("dashboard: parse history template: %w", err)
	}

	mux := http.NewServeMux()

	s := &Server{
		reg:              reg,
		tplIndex:         tplIndex,
		tplClusters:      tplClusters,
		tplClusterDetail: tplClusterDetail,
		tplHistory:       tplHistory,
	}

	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.Handle("/static/", http.StripPrefix("/static/", staticFileHandler()))
	mux.HandleFunc("GET /clusters/{name}", s.handleClusterDetail)
	mux.HandleFunc("/history", s.handleHistory)
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

// layoutData is the subset of fields every page-template payload must
// expose so base.html can render the chrome (title, optional auto-refresh
// meta tag, etc.). Page-specific structs embed it.
type layoutData struct {
	AppName            string
	PageTitle          string
	AutoRefreshSeconds int
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
	layoutData
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
	layoutData
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
		layoutData: layoutData{
			AppName:            AppName,
			PageTitle:          "Clusters",
			AutoRefreshSeconds: clusterListAutoRefreshSeconds,
		},
		Rows: rows,
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
		layoutData: layoutData{
			AppName:            AppName,
			PageTitle:          cluster.Name,
			AutoRefreshSeconds: clusterListAutoRefreshSeconds,
		},
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

// defaultHistoryLimit is the page size used when the request omits ?limit.
const defaultHistoryLimit = 50

// maxHistoryLimit clamps user-supplied ?limit values. The dashboard is a
// local-only convenience UI, so this is a defensive ceiling rather than a
// security boundary, but it keeps a runaway query string from materialising
// a million-row table.
const maxHistoryLimit = 1000

// historyAutoRefreshSeconds is the value of the meta http-equiv="refresh"
// tag rendered on the history page.
const historyAutoRefreshSeconds = 30

// errorTruncateLimit is the maximum number of runes from a history entry's
// error string rendered in the table cell. Anything longer is suffixed with
// an ellipsis to keep rows scannable.
const errorTruncateLimit = 120

// historyRow is the per-row template payload for history.html. It pre-formats
// fields that html/template can't easily compute itself.
type historyRow struct {
	registry.DeploymentHistoryEntry

	AttemptedAtRFC3339 string
	AttemptedAtHuman   string
	DurationHuman      string
	ErrorTruncated     string
	IsFailed           bool
}

// historyData is the full template payload for history.html.
type historyData struct {
	layoutData
	Filter  registry.HistoryFilter
	Entries []historyRow
}

// handleHistory renders the deploy-history page. Query parameters (all
// optional):
//
//	cluster — narrow to a single cluster name
//	service — narrow to a single service name
//	limit   — max rows; clamped to [1, maxHistoryLimit]; default 50
//
// The response includes a meta http-equiv="refresh" so the page reloads
// without JS. Failed-status rows render with class="failed" so style.css
// can highlight them.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/history" {
		http.NotFound(w, r)
		return
	}

	q := r.URL.Query()
	limit := parseHistoryLimit(q.Get("limit"))

	filter := registry.HistoryFilter{
		ClusterName: q.Get("cluster"),
		Service:     q.Get("service"),
		Limit:       limit,
	}

	entries, err := s.reg.ListHistory(r.Context(), filter)
	if err != nil {
		fmt.Fprintf(errOut, "dashboard: list history: %v\n", err)
		http.Error(w, "failed to load deploy history", http.StatusInternalServerError)
		return
	}

	rows := make([]historyRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, newHistoryRow(e))
	}

	data := historyData{
		layoutData: layoutData{
			AppName:            AppName,
			PageTitle:          "Deploy history",
			AutoRefreshSeconds: historyAutoRefreshSeconds,
		},
		Filter:  filter,
		Entries: rows,
	}

	render(w, s.tplHistory, data)
}

// parseHistoryLimit converts the raw ?limit value into a clamped, sensible
// integer. Empty / non-numeric / non-positive values fall back to the
// default; values exceeding maxHistoryLimit are clamped down. We do this in
// a helper so it is unit-testable and so the handler stays linear.
func parseHistoryLimit(raw string) int {
	if raw == "" {
		return defaultHistoryLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultHistoryLimit
	}
	if n > maxHistoryLimit {
		return maxHistoryLimit
	}
	return n
}

// newHistoryRow precomputes the rendering-friendly fields for a single
// history entry. Keeping the formatting logic in Go (not in the template)
// makes the template trivially auditable and easier to test.
func newHistoryRow(e registry.DeploymentHistoryEntry) historyRow {
	at := e.AttemptedAt.UTC()
	return historyRow{
		DeploymentHistoryEntry: e,
		AttemptedAtRFC3339:     at.Format(time.RFC3339),
		AttemptedAtHuman:       at.Format("2006-01-02 15:04:05 UTC"),
		DurationHuman:          formatDurationMs(e.RolloutDurationMs),
		ErrorTruncated:         truncateRunes(e.Error, errorTruncateLimit),
		IsFailed:               e.Status == registry.StatusFailed,
	}
}

// formatDurationMs renders a millisecond count as a short human string.
// Negative or zero values render as a dash so the cell remains compact.
func formatDurationMs(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	return (time.Duration(ms) * time.Millisecond).String()
}

// truncateRunes returns s if it is at most max runes long; otherwise it
// returns the first max runes followed by an ellipsis. Counting runes (not
// bytes) keeps multi-byte UTF-8 from being chopped mid-codepoint.
func truncateRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	count := 0
	for i := range s {
		if count == max {
			return s[:i] + "…"
		}
		count++
	}
	return s
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
