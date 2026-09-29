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

	// authTokens holds outstanding email sign-in and address-verification links.
	authTokens *authTokenStore

	// oauthOverrides replaces a provider's endpoints, and httpClient the client
	// used to reach them. Both are empty/nil in production, where the real
	// endpoints and a default client apply; tests point them at an httptest
	// server.
	oauthOverrides map[string]oauthEndpoints
	httpClient     *http.Client
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
		sessions:      newSessionStore(state),
		authTokens:    newAuthTokenStore(),
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

	layoutPages := []string{"dashboard", "api-keys", "clients", "teams", "settings", "help"}
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
	// The standalone auth pages get partials.html too: the provider marks the
	// login page draws are the same ones the settings page draws, and one
	// definition is better than two that can disagree.
	for _, name := range []string{"login", "setup", "magic-confirm"} {
		t, err := template.New(name+".html").Funcs(funcMap).ParseFS(
			adminFS, "templates/partials.html", "templates/"+name+".html")
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

	// Alternative sign-in methods. Each route is registered unconditionally and
	// each handler refuses when its method is not configured, so turning one off
	// in the portal takes effect immediately rather than at the next restart.
	//
	// Requesting a link is limited harder than a password attempt: each one
	// sends mail to an address the caller chose, and the limit is what stops
	// this endpoint being used to post it at someone.
	mux.HandleFunc("/portal/login/magic", a.requireRateLimit(a.postOnly(a.handleMagicLinkRequest), 3))
	mux.HandleFunc("/portal/login/magic/verify", a.requireRateLimit(a.handleMagicLinkVerify, 10))
	mux.HandleFunc("/portal/settings/email/verify", a.requireRateLimit(a.handleEmailVerify, 10))

	// One set of routes per federated provider, registered from the same list
	// the login and settings pages render from, so a provider cannot appear in
	// the UI without the routes behind it existing.
	for _, key := range oauthProviderOrder {
		mux.HandleFunc(oauthStartPath(key), a.requireRateLimit(a.handleOAuthLogin(key), 10))
		mux.HandleFunc(oauthCallbackPath(key), a.requireRateLimit(a.handleOAuthCallback(key), 10))
		mux.HandleFunc("/portal/settings/"+key+"/link",
			a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleOAuthLink(key))), 10))
		mux.HandleFunc("/portal/settings/"+key+"/unlink",
			a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleOAuthUnlink(key))), 10))
		mux.HandleFunc("/portal/settings/auth/"+key,
			a.requireRateLimit(a.requirePerm("settings.manage", a.postWithCSRF(a.handleOAuthSettingsUpdate(key))), 20))
	}

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
	mux.HandleFunc("/portal/api-keys/priority", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleAPIKeyPriority)), 20))
	mux.HandleFunc("/portal/api-keys/max-concurrent", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleAPIKeyMaxConcurrent)), 20))

	mux.HandleFunc("/portal/clients", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			a.requireRateLimit(a.postWithCSRF(a.handleClientTokenCreate), 20)(w, r)
		} else {
			a.handleClientTokens(w, r)
		}
	}))
	mux.HandleFunc("/portal/clients/revoke", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleClientTokenRevoke)), 20))
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

	mux.HandleFunc("/portal/model-aliases", a.requireRateLimit(a.requirePerm("alias.manage", a.postWithCSRF(a.handleModelAliasCreate)), 20))
	mux.HandleFunc("/portal/model-aliases/delete", a.requireRateLimit(a.requirePerm("alias.manage", a.postWithCSRF(a.handleModelAliasDelete)), 20))
	mux.HandleFunc("/portal/model-aliases/reorder", a.requireRateLimit(a.requirePerm("alias.manage", a.postWithCSRF(a.handleModelAliasReorder)), 30))

	mux.HandleFunc("/portal/jobs/cancel", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleJobCancel)), 20))
	mux.HandleFunc("/portal/queue/cancel", a.requireRateLimit(a.requirePerm("queue.cancel", a.postWithCSRF(a.handleQueueCancel)), 20))

	// Help page.
	mux.HandleFunc("/portal/help", a.requireAuth(a.handleHelp))

	mux.HandleFunc("/portal/settings", a.requireAuth(a.handleSettings))
	mux.HandleFunc("/portal/settings/password", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleChangePassword)), 10))
	mux.HandleFunc("/portal/settings/users", a.requireRateLimit(a.requirePerm("user.manage", a.postWithCSRF(a.handleAddUser)), 20))
	mux.HandleFunc("/portal/settings/users/disable", a.requireRateLimit(a.requirePerm("user.manage", a.postWithCSRF(a.handleUserDisable)), 20))
	mux.HandleFunc("/portal/settings/users/enable", a.requireRateLimit(a.requirePerm("user.manage", a.postWithCSRF(a.handleUserEnable)), 20))
	mux.HandleFunc("/portal/settings/users/promote", a.requireRateLimit(a.requirePerm("user.manage", a.postWithCSRF(a.handleUserPromote)), 20))
	mux.HandleFunc("/portal/settings/users/demote", a.requireRateLimit(a.requirePerm("user.manage", a.postWithCSRF(a.handleUserDemote)), 20))
	mux.HandleFunc("/portal/settings/users/reset-password", a.requireRateLimit(a.requirePerm("user.manage", a.postWithCSRF(a.handleUserResetPassword)), 20))
	mux.HandleFunc("/portal/settings/users/delete", a.requireRateLimit(a.requirePerm("user.manage", a.postWithCSRF(a.handleUserDelete)), 20))
	mux.HandleFunc("/portal/settings/users/roles/add", a.requireRateLimit(a.requirePerm("user.manage", a.postWithCSRF(a.handleUserRoleAdd)), 20))
	mux.HandleFunc("/portal/settings/users/roles/remove", a.requireRateLimit(a.requirePerm("user.manage", a.postWithCSRF(a.handleUserRoleRemove)), 20))
	mux.HandleFunc("/portal/settings/users/sign-out", a.requireRateLimit(a.requirePerm("user.manage", a.postWithCSRF(a.handleUserSignOut)), 20))
	mux.HandleFunc("/portal/settings/sessions/revoke", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleSessionRevoke)), 20))
	mux.HandleFunc("/portal/settings/roles", a.requireRateLimit(a.requirePerm("role.manage", a.postWithCSRF(a.handleRoleSave)), 20))
	mux.HandleFunc("/portal/settings/roles/delete", a.requireRateLimit(a.requirePerm("role.manage", a.postWithCSRF(a.handleRoleDelete)), 20))
	mux.HandleFunc("/portal/teams", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if !a.canDo(r, "team.create") {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			a.requireRateLimit(a.postWithCSRF(a.handleTeamCreate), 10)(w, r)
			return
		}
		a.handleTeams(w, r)
	}))
	mux.HandleFunc("/portal/teams/members/add", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleTeamMemberAdd)), 20))
	mux.HandleFunc("/portal/teams/members/remove", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleTeamMemberRemove)), 20))
	mux.HandleFunc("/portal/teams/state", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleTeamState)), 20))
	mux.HandleFunc("/portal/settings/users/isolation", a.requireRateLimit(a.requirePerm("user.manage", a.postWithCSRF(a.handleUserIsolation)), 20))
	mux.HandleFunc("/portal/settings/upstream/add", a.requireRateLimit(a.requirePerm("upstream.manage", a.postWithCSRF(a.handleUpstreamAdd)), 20))
	mux.HandleFunc("/portal/settings/upstream/remove", a.requireRateLimit(a.requirePerm("upstream.manage", a.postWithCSRF(a.handleUpstreamRemove)), 20))
	mux.HandleFunc("/portal/settings/optimization", a.requireRateLimit(a.requirePerm("settings.manage", a.postWithCSRF(a.handleOptimizationUpdate)), 20))
	mux.HandleFunc("/portal/settings/host", a.requireRateLimit(a.requirePerm("settings.manage", a.postWithCSRF(a.handleHostUpdate)), 20))
	mux.HandleFunc("/portal/settings/pricing", a.requireRateLimit(a.requirePerm("pricing.manage", a.postWithCSRF(a.handleModelPricingUpdate)), 30))
	mux.HandleFunc("/portal/settings/pricing/delete", a.requireRateLimit(a.requirePerm("pricing.manage", a.postWithCSRF(a.handleModelPricingDelete)), 30))
	mux.HandleFunc("/portal/settings/currency", a.requireRateLimit(a.requirePerm("settings.manage", a.postWithCSRF(a.handleCostCurrencyUpdate)), 20))

	// Email sign-in configuration (admin) and the address a user sets on their
	// own account. The per-provider equivalents are registered above.
	mux.HandleFunc("/portal/settings/auth/smtp", a.requireRateLimit(a.requirePerm("settings.manage", a.postWithCSRF(a.handleSMTPUpdate)), 20))
	mux.HandleFunc("/portal/settings/auth/smtp/test", a.requireRateLimit(a.requirePerm("settings.manage", a.postWithCSRF(a.handleSMTPTest)), 5))
	mux.HandleFunc("/portal/settings/email", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleEmailUpdate)), 5))
	mux.HandleFunc("/portal/settings/email/resend", a.requireRateLimit(a.requireAuth(a.postWithCSRF(a.handleEmailResend)), 3))

	// Dashboard JSON API
	mux.HandleFunc("/portal/api/dashboard", a.requireAuth(a.handleDashboardJSON))

	// Jobs JSON API — live stats for in-flight jobs
	mux.HandleFunc("/portal/api/jobs", a.requireAuth(a.handleJobsJSON))
	mux.HandleFunc("/portal/api/connections", a.requireAuth(a.handleConnectionsJSON))

	// Usage JSON API — time-series token usage (members see their own only)
	mux.HandleFunc("/portal/api/usage", a.requireAuth(a.handleUsageJSON))

	// Performance JSON API — time-series inference speed (members see their own only)
	mux.HandleFunc("/portal/api/perf", a.requireAuth(a.handlePerfJSON))

	// Logs JSON API (admin-only)
	mux.HandleFunc("/portal/api/logs", a.requirePerm("settings.view", a.handleLogsJSON))

	// Audit log JSON API (admin-only)
	mux.HandleFunc("/portal/api/audit", a.requirePerm("audit.view", a.handleAuditLogJSON))

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

// portalFetchHeader marks a portal action submitted by the page's own script
// rather than by a browser form navigation.
const portalFetchHeader = "X-Portal-Fetch"

// redirectOrRefresh completes a portal action.
//
// A browser form post gets the redirect it has always got, so every page keeps
// working with scripting disabled — the forms stay real forms and this stays a
// real POST-redirect-GET. A script-submitted post instead gets 204 and the
// destination in a header, and the page pulls just the content it needs rather
// than reloading the document, its stylesheet, and its scripts to show one new
// table row.
//
// The destination matters: several actions finish somewhere other than where
// they started, and some carry a #tab the page must re-select.
func redirectOrRefresh(w http.ResponseWriter, r *http.Request, url string) {
	if r.Header.Get(portalFetchHeader) == "" {
		http.Redirect(w, r, url, http.StatusFound)
		return
	}
	w.Header().Set("X-Portal-Location", url)
	w.WriteHeader(http.StatusNoContent)
}

// postOnly rejects anything but a POST. Used for the unauthenticated form posts
// that have no session to carry a CSRF token, where postWithCSRF cannot apply.
func (a *Admin) postOnly(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handler(w, r)
	}
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
