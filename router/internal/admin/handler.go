package admin

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"llmesh/router/internal/hub"
	"llmesh/router/internal/logring"
	"llmesh/router/internal/queue"
	"llmesh/router/internal/stats"
)

//go:embed templates static
var adminFS embed.FS

// Admin is the management console HTTP handler.
type Admin struct {
	state         *State
	hub           *hub.Hub
	queue         *queue.Queue
	reqCount      func() int64
	stats         *stats.Stats
	routerVersion string
	name          string
	host          string
	sessions      *sessionStore
	tmpls         map[string]*template.Template
	mux           *http.ServeMux
	log           *slog.Logger
	sink          *logring.Sink
	rateLimiter   *rateLimiter
	// assetVersion is a short content hash of the embedded static assets. It is
	// appended to asset URLs (?v=) so a redeploy that changes the CSS/JS forces
	// browsers to fetch the new file instead of serving a stale cached copy.
	assetVersion string
	// trustProxy enables honouring X-Forwarded-For/Proto. Off by default so a
	// direct client cannot spoof its IP to bypass rate limiting.
	trustProxy bool

	// upstreamReload is called after any upstream router add/remove.
	// Wired by main.go to connector.Reload after the connector is created.
	upstreamReload func()
	// upstreamConnected reports whether the given upstream URL is currently connected.
	// Wired by main.go to connector.Connected.
	upstreamConnected func(url string) bool
}

// SetUpstreamReloader registers the callback invoked after upstream router config changes.
func (a *Admin) SetUpstreamReloader(fn func()) { a.upstreamReload = fn }

// SetConnectorStatus registers the function used to query per-upstream connection status.
func (a *Admin) SetConnectorStatus(fn func(url string) bool) { a.upstreamConnected = fn }

// SetTrustProxy configures whether proxy headers (X-Forwarded-For/Proto) are
// honoured. Enable only when the router is behind a trusted reverse proxy.
func (a *Admin) SetTrustProxy(v bool) { a.trustProxy = v }

// defaultConfiguredHost mirrors the fallback assigned in router/config.go when
// no host is set in the config file. When a.host still equals this sentinel the
// operator never configured a real host, so the portal prefers an auto-detected
// or admin-set value over showing the placeholder.
const defaultConfiguredHost = "llmesh.example.com"

// effectiveHost resolves the public hostname shown throughout the portal and
// written into downloadable client configs. Precedence, highest first:
//  1. the admin-set override from the settings table (a deliberate choice),
//  2. a real host from the config file (anything other than the placeholder),
//  3. the host the browser actually used to reach the portal (auto-detection),
//  4. the configured value as a last resort (the placeholder).
func (a *Admin) effectiveHost(r *http.Request) string {
	if h := a.state.PortalHost(); h != "" {
		return h
	}
	if a.host != "" && a.host != defaultConfiguredHost {
		return a.host
	}
	if h := requestHost(r, a.trustProxy); h != "" {
		return h
	}
	return a.host
}

// requestHost returns the host the client used to reach the router: the
// X-Forwarded-Host set by a trusted proxy when trustProxy is enabled, otherwise
// the request's own Host header. Returns "" when nothing usable is present.
func requestHost(r *http.Request, trustProxy bool) string {
	if r == nil {
		return ""
	}
	if trustProxy {
		if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
			if i := strings.IndexByte(xfh, ','); i >= 0 {
				xfh = xfh[:i] // take the first hop in a comma-separated chain
			}
			return strings.TrimSpace(xfh)
		}
	}
	return r.Host
}

// New creates an Admin handler. statePath is the path to state.json.
func New(statePath string, h *hub.Hub, q *queue.Queue, reqCount func() int64, s *stats.Stats, routerVersion, name, host string, sink *logring.Sink) (*Admin, error) {
	if reqCount == nil {
		return nil, fmt.Errorf("admin: reqCount must not be nil")
	}
	state, err := LoadState(statePath)
	if err != nil {
		return nil, err
	}
	a := &Admin{
		state:         state,
		hub:           h,
		queue:         q,
		reqCount:      reqCount,
		stats:         s,
		routerVersion: routerVersion,
		name:          name,
		host:          host,
		sessions:      newSessionStore(),
		log:           logring.NewLogger(sink, "admin", slog.LevelInfo),
		sink:          sink,
		rateLimiter:   newRateLimiter(1*time.Minute, 5*time.Minute),
	}
	a.assetVersion = assetVersion()
	if err := a.parseTemplates(); err != nil {
		return nil, err
	}
	a.registerRoutes()
	return a, nil
}

// State returns the loaded State, for use by the API handler.
func (a *Admin) State() *State {
	return a.state
}

// assetVersion returns a short content hash over the embedded static assets.
// Any change to a served asset changes the hash, which invalidates the
// versioned URL and any cached copy keyed by it.
func assetVersion() string {
	h := sha256.New()
	for _, name := range []string{"static/admin.css", "static/admin.js"} {
		b, err := adminFS.ReadFile(name)
		if err != nil {
			continue
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func (a *Admin) parseTemplates() error {
	funcMap := template.FuncMap{
		// asset returns a cache-busting URL for an embedded static file, e.g.
		// {{asset "admin.css"}} -> /portal/static/admin.css?v=<hash>.
		"asset": func(name string) string {
			return "/portal/static/" + name + "?v=" + a.assetVersion
		},
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n]
		},
		"not": func(b bool) bool { return !b },
		// dict builds a map from alternating key/value pairs so partials can be
		// invoked with named arguments, e.g. {{template "action-button" dict "Action" "/x" ...}}.
		"dict": func(pairs ...any) (map[string]any, error) {
			if len(pairs)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of arguments")
			}
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i < len(pairs); i += 2 {
				key, ok := pairs[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key %d is not a string", i)
				}
				m[key] = pairs[i+1]
			}
			return m, nil
		},
	}

	layoutPages := []string{"dashboard", "api-keys", "clients", "settings", "help"}
	a.tmpls = make(map[string]*template.Template)
	for _, name := range layoutPages {
		t, err := template.New("layout.html").Funcs(funcMap).ParseFS(
			adminFS,
			"templates/layout.html",
			"templates/partials.html",
			"templates/"+name+".html",
		)
		if err != nil {
			return err
		}
		a.tmpls[name] = t
	}
	for _, name := range []string{"login", "setup"} {
		t, err := template.New(name+".html").Funcs(funcMap).ParseFS(adminFS, "templates/"+name+".html")
		if err != nil {
			return err
		}
		a.tmpls[name] = t
	}
	return nil
}

func (a *Admin) registerRoutes() {
	mux := http.NewServeMux()

	// Static assets. A content-hash ETag lets browsers revalidate cheaply, and
	// versioned (?v=) requests are marked immutable so a redeploy that changes
	// an asset serves fresh bytes under a new URL rather than a stale cache.
	staticFS := http.StripPrefix("/portal", http.FileServer(http.FS(adminFS)))
	mux.Handle("/portal/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+a.assetVersion+`"`)
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		staticFS.ServeHTTP(w, r)
	}))

	// Auth (no session required)
	mux.HandleFunc("/portal/login", a.requireRateLimit(a.handleLogin, 5))
	mux.HandleFunc("/portal/setup", a.requireRateLimit(a.handleSetup, 5))

	// Logout requires auth + CSRF
	mux.HandleFunc("/portal/logout", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleLogout)), 20))

	// Protected pages
	mux.HandleFunc("/portal/", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/portal/" {
			http.NotFound(w, r)
			return
		}
		// Redirect to setup if no users yet
		if a.state.NeedsSetup() {
			http.Redirect(w, r, "/portal/setup", http.StatusFound)
			return
		}
		a.handleDashboard(w, r)
	}))

	mux.HandleFunc("/portal/api-keys", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			a.requireRateLimit(a.postWithCSRF(a.handleAPIKeyCreate), 20)(w, r)
		} else {
			a.handleAPIKeys(w, r)
		}
	}))
	mux.HandleFunc("/portal/api-keys/revoke", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleAPIKeyRevoke)), 20))
	mux.HandleFunc("/portal/api-keys/priority", a.requireRateLimit(a.requireAdmin(a.postWithCSRF(a.handleAPIKeyPriority)), 20))
	mux.HandleFunc("/portal/api-keys/max-concurrent", a.requireRateLimit(a.requireAdmin(a.postWithCSRF(a.handleAPIKeyMaxConcurrent)), 20))

	mux.HandleFunc("/portal/clients", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			a.requireRateLimit(a.postWithCSRF(a.handleClientTokenCreate), 20)(w, r)
		} else {
			a.handleClientTokens(w, r)
		}
	}))
	mux.HandleFunc("/portal/clients/revoke", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleClientTokenRevoke)), 20))
	mux.HandleFunc("/portal/clients/update", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleClientUpdate)), 20))
	mux.HandleFunc("/portal/clients/owner-slots", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleClientTokenOwnerSlots)), 20))
	mux.HandleFunc("/portal/clients/config", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleClientTokenConfig(w, r)
	}))
	mux.HandleFunc("/portal/clients/shim-config", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleShimConfig(w, r)
	}))

	mux.HandleFunc("/portal/model-aliases", a.requireRateLimit(a.requireAdmin(a.postWithCSRF(a.handleModelAliasCreate)), 20))
	mux.HandleFunc("/portal/model-aliases/delete", a.requireRateLimit(a.requireAdmin(a.postWithCSRF(a.handleModelAliasDelete)), 20))

	mux.HandleFunc("/portal/jobs/cancel", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleJobCancel)), 20))
	mux.HandleFunc("/portal/queue/cancel", a.requireRateLimit(a.requireAdmin(a.postWithCSRF(a.handleQueueCancel)), 20))

	// Help page.
	mux.HandleFunc("/portal/help", a.requireAuth(a.handleHelp))

	mux.HandleFunc("/portal/settings", a.requireAuth(a.handleSettings))
	mux.HandleFunc("/portal/settings/password", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleChangePassword)), 10))
	mux.HandleFunc("/portal/settings/users", a.requireRateLimit(a.requireAdmin(a.postWithCSRF(a.handleAddUser)), 20))
	mux.HandleFunc("/portal/settings/users/disable", a.requireRateLimit(a.requireAdmin(a.postWithCSRF(a.handleUserDisable)), 20))
	mux.HandleFunc("/portal/settings/users/enable", a.requireRateLimit(a.requireAdmin(a.postWithCSRF(a.handleUserEnable)), 20))
	mux.HandleFunc("/portal/settings/users/promote", a.requireRateLimit(a.requireAdmin(a.postWithCSRF(a.handleUserPromote)), 20))
	mux.HandleFunc("/portal/settings/users/demote", a.requireRateLimit(a.requireAdmin(a.postWithCSRF(a.handleUserDemote)), 20))
	mux.HandleFunc("/portal/settings/upstream/add", a.requireRateLimit(a.requireAdmin(a.postWithCSRF(a.handleUpstreamAdd)), 20))
	mux.HandleFunc("/portal/settings/upstream/remove", a.requireRateLimit(a.requireAdmin(a.postWithCSRF(a.handleUpstreamRemove)), 20))
	mux.HandleFunc("/portal/settings/optimization", a.requireRateLimit(a.requireAdmin(a.postWithCSRF(a.handleOptimizationUpdate)), 20))
	mux.HandleFunc("/portal/settings/host", a.requireRateLimit(a.requireAdmin(a.postWithCSRF(a.handleHostUpdate)), 20))

	// Dashboard JSON API
	mux.HandleFunc("/portal/api/dashboard", a.requireAuth(a.handleDashboardJSON))

	// Jobs JSON API — live stats for in-flight jobs
	mux.HandleFunc("/portal/api/jobs", a.requireAuth(a.handleJobsJSON))

	// Usage JSON API — time-series token usage (members see their own only)
	mux.HandleFunc("/portal/api/usage", a.requireAuth(a.handleUsageJSON))

	// Logs JSON API (admin-only)
	mux.HandleFunc("/portal/api/logs", a.requireAdmin(a.handleLogsJSON))

	// Audit log JSON API (admin-only)
	mux.HandleFunc("/portal/api/audit", a.requireAdmin(a.handleAuditLogJSON))

	a.mux = mux
}

func (a *Admin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Redirect bare /portal to /portal/
	if r.URL.Path == "/portal" {
		http.Redirect(w, r, "/portal/", http.StatusMovedPermanently)
		return
	}
	// First-run redirect
	if a.state.NeedsSetup() &&
		!strings.HasPrefix(r.URL.Path, "/portal/setup") &&
		!strings.HasPrefix(r.URL.Path, "/portal/static") {
		http.Redirect(w, r, "/portal/setup", http.StatusFound)
		return
	}
	a.mux.ServeHTTP(w, r)
}

func (a *Admin) render(w http.ResponseWriter, name string, data any) {
	t, ok := a.tmpls[name]
	if !ok {
		http.Error(w, "template not found: "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, data); err != nil {
		a.log.Error("admin: render", "template", name, "error", err)
	}
}

func (a *Admin) renderStandalone(w http.ResponseWriter, name string, data any) {
	a.render(w, name, data)
}

// postWithCSRF returns an http.HandlerFunc that only accepts POST requests
// and validates the CSRF token against the session. Each session carries its
// own CSRF token (set at login and refreshed on each page render) so
// concurrent tabs for the same user don't invalidate each other.
func (a *Admin) postWithCSRF(handler func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		token := r.PostFormValue("_csrf")
		if token == "" {
			token = r.Header.Get("X-CSRF-Token")
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		stored, ok := a.sessions.getCSRF(c.Value)
		if !ok || stored == "" || token != stored {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		handler(w, r)
	}
}
