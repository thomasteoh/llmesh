package admin

import (
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

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
	}
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

func (a *Admin) parseTemplates() error {
	funcMap := template.FuncMap{
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n]
		},
		"not": func(b bool) bool { return !b },
	}

	layoutPages := []string{"dashboard", "api-keys", "clients", "settings", "help"}
	a.tmpls = make(map[string]*template.Template)
	for _, name := range layoutPages {
		t, err := template.New("layout.html").Funcs(funcMap).ParseFS(
			adminFS,
			"templates/layout.html",
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

	// Static assets
	mux.Handle("/portal/static/", http.StripPrefix("/portal", http.FileServer(http.FS(adminFS))))

	// Auth (no session required)
	mux.HandleFunc("/portal/login", a.handleLogin)
	mux.HandleFunc("/portal/setup", a.handleSetup)

	// Logout requires auth + CSRF
	mux.HandleFunc("/portal/logout", a.requireAuth(a.postWithCSRF(a.handleLogout)))

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
			a.postWithCSRF(a.handleAPIKeyCreate)(w, r)
		} else {
			a.handleAPIKeys(w, r)
		}
	}))
	mux.HandleFunc("/portal/api-keys/revoke", a.requireAuth(a.postWithCSRF(a.handleAPIKeyRevoke)))
	mux.HandleFunc("/portal/api-keys/priority", a.requireAdmin(a.postWithCSRF(a.handleAPIKeyPriority)))

	mux.HandleFunc("/portal/clients", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			a.postWithCSRF(a.handleClientTokenCreate)(w, r)
		} else {
			a.handleClientTokens(w, r)
		}
	}))
	mux.HandleFunc("/portal/clients/revoke", a.requireAuth(a.postWithCSRF(a.handleClientTokenRevoke)))
	mux.HandleFunc("/portal/clients/exclusive", a.requireAuth(a.postWithCSRF(a.handleClientTokenExclusive)))
	mux.HandleFunc("/portal/clients/config", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleClientTokenConfig(w, r)
	}))

	mux.HandleFunc("/portal/model-aliases", a.requireAdmin(a.postWithCSRF(a.handleModelAliasCreate)))
	mux.HandleFunc("/portal/model-aliases/delete", a.requireAdmin(a.postWithCSRF(a.handleModelAliasDelete)))

	mux.HandleFunc("/portal/jobs/cancel", a.requireAuth(a.postWithCSRF(a.handleJobCancel)))
	mux.HandleFunc("/portal/queue/cancel", a.requireAdmin(a.postWithCSRF(a.handleQueueCancel)))

	// Help page.
	mux.HandleFunc("/portal/help", a.requireAuth(a.handleHelp))

	mux.HandleFunc("/portal/settings", a.requireAuth(a.handleSettings))
	mux.HandleFunc("/portal/settings/password", a.requireAuth(a.postWithCSRF(a.handleChangePassword)))
	mux.HandleFunc("/portal/settings/users", a.requireAdmin(a.postWithCSRF(a.handleAddUser)))
	mux.HandleFunc("/portal/settings/users/disable", a.requireAdmin(a.postWithCSRF(a.handleUserDisable)))
	mux.HandleFunc("/portal/settings/users/enable", a.requireAdmin(a.postWithCSRF(a.handleUserEnable)))
	mux.HandleFunc("/portal/settings/users/promote", a.requireAdmin(a.postWithCSRF(a.handleUserPromote)))
	mux.HandleFunc("/portal/settings/users/demote", a.requireAdmin(a.postWithCSRF(a.handleUserDemote)))

	// Dashboard JSON API
	mux.HandleFunc("/portal/api/dashboard", a.requireAuth(a.handleDashboardJSON))

	// Logs JSON API (admin-only)
	mux.HandleFunc("/portal/api/logs", a.requireAdmin(a.handleLogsJSON))

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
// and validates the CSRF token. It expects the user to already be in context
// (from requireAuth or requireAdmin). The token is consumed (one-time use).
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
		uRaw, ok := r.Context().Value(ctxUser).(User)
		if !ok || !a.state.ConsumeCSRF(uRaw.Username, token) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		handler(w, r)
	}
}
