package admin

import (
	"embed"
	"html/template"
	"log"
	"net/http"
	"strings"

	"llmesh/router/internal/hub"
)

//go:embed templates static
var adminFS embed.FS

// Admin is the management console HTTP handler.
type Admin struct {
	state    *State
	hub      *hub.Hub
	reqCount func() int64
	sessions *sessionStore
	tmpls    map[string]*template.Template
	mux      *http.ServeMux
}

// New creates an Admin handler. statePath is the path to state.json.
func New(statePath string, h *hub.Hub, reqCount func() int64) (*Admin, error) {
	state, err := LoadState(statePath)
	if err != nil {
		return nil, err
	}
	a := &Admin{
		state:    state,
		hub:      h,
		reqCount: reqCount,
		sessions: newSessionStore(),
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

	layoutPages := []string{"dashboard", "api-keys", "client-tokens", "docs", "settings"}
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
	mux.Handle("/admin/static/", http.FileServer(http.FS(adminFS)))

	// Auth (no session required)
	mux.HandleFunc("/admin/login", a.handleLogin)
	mux.HandleFunc("/admin/logout", a.handleLogout)
	mux.HandleFunc("/admin/setup", a.handleSetup)

	// Protected pages
	mux.HandleFunc("/admin/", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/" {
			http.NotFound(w, r)
			return
		}
		// Redirect to setup if no users yet
		if a.state.NeedsSetup() {
			http.Redirect(w, r, "/admin/setup", http.StatusFound)
			return
		}
		a.handleDashboard(w, r)
	}))

	mux.HandleFunc("/admin/api-keys", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			a.handleAPIKeyCreate(w, r)
		} else {
			a.handleAPIKeys(w, r)
		}
	}))
	mux.HandleFunc("/admin/api-keys/revoke", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleAPIKeyRevoke(w, r)
	}))

	mux.HandleFunc("/admin/client-tokens", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			a.handleClientTokenCreate(w, r)
		} else {
			a.handleClientTokens(w, r)
		}
	}))
	mux.HandleFunc("/admin/client-tokens/revoke", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleClientTokenRevoke(w, r)
	}))

	mux.HandleFunc("/admin/docs", a.requireAuth(a.handleDocs))

	mux.HandleFunc("/admin/settings", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		} else {
			a.handleSettings(w, r)
		}
	}))
	mux.HandleFunc("/admin/settings/password", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleChangePassword(w, r)
	}))
	mux.HandleFunc("/admin/settings/users", a.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleAddUser(w, r)
	}))
	mux.HandleFunc("/admin/settings/users/disable", a.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleUserDisable(w, r)
	}))
	mux.HandleFunc("/admin/settings/users/enable", a.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleUserEnable(w, r)
	}))
	mux.HandleFunc("/admin/settings/users/promote", a.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleUserPromote(w, r)
	}))
	mux.HandleFunc("/admin/settings/users/demote", a.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleUserDemote(w, r)
	}))

	// Dashboard JSON API
	mux.HandleFunc("/admin/api/dashboard", a.requireAuth(a.handleDashboardJSON))

	a.mux = mux
}

func (a *Admin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Redirect bare /admin to /admin/
	if r.URL.Path == "/admin" {
		http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
		return
	}
	// First-run redirect
	if a.state.NeedsSetup() &&
		!strings.HasPrefix(r.URL.Path, "/admin/setup") &&
		!strings.HasPrefix(r.URL.Path, "/admin/static") {
		http.Redirect(w, r, "/admin/setup", http.StatusFound)
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
		log.Printf("admin: render %s: %v", name, err)
	}
}

func (a *Admin) renderStandalone(w http.ResponseWriter, name string, data any) {
	a.render(w, name, data)
}
