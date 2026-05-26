package admin

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"llmesh/pkg/types"
	"llmesh/router/internal/stats"
)

// --- Shared page data types ---

type basePage struct {
	Page          string
	Username      string
	IsAdmin       bool
	Flash         string
	Error         string
	CSRFToken     string
	RouterVersion string
	Name          string
	Host          string
}

// StatRow is a named row in the token usage panel.
type StatRow struct {
	Name             string
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
}

type DashboardPage struct {
	basePage
	TotalRequests int64
	ActiveClients int
	APIKeyCount   int
	TokenCount    int
	ActiveModels  []string
	ActiveAliases map[string][]string // alias → []target models
	ModelAliases  map[string][]string // model → []aliases pointing to it (inverted, for per-model UI)
	Clients       []ClientRow
	StatsByModel  []StatRow
	StatsByUser   []StatRow
	QueueLen      int
	QueueItems    []QueuedJobRow // filtered to the requesting user's own items for non-admins
}

type ClientRow struct {
	Name        string
	Token       string
	Status      string // "connected" | "offline" | "never_connected"
	StatusClass string // CSS badge class
	StatusLabel string // display label with symbol
	LastSeen    string
	Models      string
	Version     string
}

type APIKeysPage struct {
	basePage
	Keys      []APIKey
	NewKey    string
	FormError string
}

// ClientUserGroup groups a user's client tokens for the admin view.
type ClientUserGroup struct {
	Username string
	HasLive  bool // true if any token has a live connection
	Tokens   []ClientTokenRow
}

type ClientTokensPage struct {
	basePage
	Groups    []ClientUserGroup // admin view: tokens grouped by owner
	Tokens    []ClientTokenRow  // non-admin view: own tokens only
	NewToken  string
	FormError string
}

type ConnectedClientRow struct {
	Name          string
	Version       string
	Models        string
	InFlight      int
	MaxConcurrent int
	Jobs          []InFlightJobRow
}

// InFlightJobRow is a single in-flight job for display on the clients page.
type InFlightJobRow struct {
	ID              string
	Owner           string
	APIKeyLabel     string
	Model           string
	EnqueuedAt      string
	DispatchedAtISO string // RFC3339, for JS elapsed computation
	WordCount       int
	CanCancel       bool
}

// QueuedJobRow is a single queued (waiting) job for display on the dashboard.
type QueuedJobRow struct {
	ID            string
	Owner         string
	APIKeyLabel   string
	Model         string
	Priority      string
	EnqueuedAt    string
	EnqueuedAtISO string // RFC3339, for JS elapsed computation
	WordCount     int
	CanCancel     bool // true only for admins
}

type ClientTokenRow struct {
	ClientToken
	Status      string
	StatusClass string // CSS badge class
	StatusLabel string // display label with symbol
	LastSeen    string
	Models      []ModelWithAliases
	Connections []ConnectedClientRow
	CSRFToken   string // for use in named sub-templates
}

type ModelWithAliases struct {
	Name    string
	Aliases []string
}

type UpstreamRouterRow struct {
	UpstreamRouter
	Connected bool
}

type SettingsPage struct {
	basePage
	Users     []UserRow
	Upstreams []UpstreamRouterRow
}

type UserRow struct {
	User
	IsSelf bool
}

// invertAliasMap returns model→[]aliases from an alias→[]models map, with each alias list sorted.
func invertAliasMap(aliasMap map[string][]string) map[string][]string {
	inv := make(map[string][]string, len(aliasMap))
	for alias, targets := range aliasMap {
		for _, model := range targets {
			inv[model] = append(inv[model], alias)
		}
	}
	for m := range inv {
		sort.Strings(inv[m])
	}
	return inv
}

func (a *Admin) newBasePage(page string, u User) basePage {
	bp := basePage{
		Page:          page,
		Username:      u.Username,
		IsAdmin:       u.Role == "admin",
		RouterVersion: a.routerVersion,
		Name:          a.name,
		Host:          a.host,
	}
	// Generate a fresh CSRF token for each page render (one-time use).
	csrfToken, err := a.state.RefreshCSRFToken(u.Username)
	if err == nil && csrfToken != "" {
		bp.CSRFToken = csrfToken
	}
	return bp
}

// --- Dashboard ---

// filterQueueForUser returns only the queue items visible to u.
// Admins see all items; members see only their own.
func filterQueueForUser(items []types.InferenceRequest, u User) []types.InferenceRequest {
	if u.Role == "admin" {
		return items
	}
	var out []types.InferenceRequest
	for _, req := range items {
		if req.Owner == u.Username {
			out = append(out, req)
		}
	}
	return out
}

func (a *Admin) handleDashboard(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	tokens := a.state.ClientTokensFor("", true)
	clients := make([]ClientRow, 0, len(tokens))
	for _, t := range tokens {
		row := ClientRow{
			Name:  t.Owner + "/" + t.Name,
			Token: t.Token,
		}
		connCount := a.hub.ConnectedCountByToken(t.Token)
		if connCount > 0 {
			if connCount == 1 {
				row.Status = "connected"
				row.StatusClass = "connected"
				row.StatusLabel = "\u25cf connected"
			} else {
				row.Status = fmt.Sprintf("%d connected", connCount)
				row.StatusClass = "multi_connected"
				row.StatusLabel = fmt.Sprintf("\u25cf %d connected", connCount)
			}
			mods := a.hub.ConnectedModels(t.Token)
			sort.Strings(mods)
			row.Models = strings.Join(mods, ", ")
			row.Version = a.hub.ConnectedVersion(t.Token)
		} else if ls := a.hub.LastSeenTime(t.Token); !ls.IsZero() {
			row.Status = "offline"
			row.StatusClass = "offline"
			row.StatusLabel = "\u25cb offline"
			row.LastSeen = humanTime(ls)
		} else {
			row.Status = "never_connected"
			row.StatusClass = "never_connected"
			row.StatusLabel = "\u25cb never connected"
		}
		clients = append(clients, row)
	}
	activeModels := a.hub.ActiveModels()
	sort.Strings(activeModels)
	activeAliases := a.state.AliasMap()
	modelAliases := invertAliasMap(activeAliases)
	data := DashboardPage{
		basePage:      a.newBasePage("dashboard", u),
		TotalRequests: a.reqCount(),
		ActiveClients: a.hub.ActiveClientCount(),
		APIKeyCount:   a.state.APIKeyCount(),
		TokenCount:    a.state.ClientTokenCount(),
		ActiveModels:  activeModels,
		ActiveAliases: activeAliases,
		ModelAliases:  modelAliases,
		Clients:       clients,
		StatsByModel:  statsRows(a.stats, true),
		StatsByUser:   statsRows(a.stats, false),
	}
	if a.queue != nil {
		snap := a.queue.Snapshot()
		visible := filterQueueForUser(snap, u)
		data.QueueLen = len(snap) // total depth for header badge
		data.QueueItems = make([]QueuedJobRow, 0, len(visible))
		for _, req := range visible {
			data.QueueItems = append(data.QueueItems, QueuedJobRow{
				ID:            req.ID,
				Owner:         req.Owner,
				APIKeyLabel:   req.APIKeyLabel,
				Model:         req.Model,
				Priority:      priorityName(int(req.Priority)),
				EnqueuedAt:    humanTime(req.EnqueuedAt),
				EnqueuedAtISO: req.EnqueuedAt.UTC().Format(time.RFC3339),
				WordCount:     req.WordCount,
				CanCancel:     u.Role == "admin",
			})
		}
	}
	a.render(w, "dashboard", data)
}

// --- API Keys ---

func (a *Admin) handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	a.renderAPIKeys(w, u, "", "")
}

func (a *Admin) renderAPIKeys(w http.ResponseWriter, u User, newKey, formErr string) {
	keys := a.state.APIKeysFor(u.Username, u.Role == "admin")
	a.render(w, "api-keys", APIKeysPage{
		basePage:  a.newBasePage("api-keys", u),
		Keys:      keys,
		NewKey:    newKey,
		FormError: formErr,
	})
}

func (a *Admin) handleAPIKeyCreate(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	priority := r.FormValue("priority")
	if priority == "" {
		priority = "normal"
	}
	if label == "" {
		a.renderAPIKeys(w, u, "", "Label is required.")
		return
	}
	keyVal, err := GenAPIKeyValue(u.Username)
	if err != nil {
		a.renderAPIKeys(w, u, "", "Failed to generate key.")
		return
	}
	k := APIKey{
		Label:     label,
		Owner:     u.Username,
		Key:       keyVal,
		Priority:  priority,
		CreatedAt: time.Now().UTC(),
	}
	if err := a.state.AddAPIKey(k); err != nil {
		a.renderAPIKeys(w, u, "", err.Error())
		return
	}
	a.renderAPIKeys(w, u, keyVal, "")
}

func (a *Admin) handleAPIKeyRevoke(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	key := r.FormValue("key")
	a.state.RevokeAPIKey(u.Username, key, u.Role == "admin")
	http.Redirect(w, r, "/portal/api-keys", http.StatusFound)
}

// --- Clients ---

func (a *Admin) handleClientTokens(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	a.renderClientTokens(w, u, "", "")
}

func (a *Admin) renderClientTokens(w http.ResponseWriter, u User, newToken, formErr string) {
	bp := a.newBasePage("clients", u)

	rawTokens := a.state.ClientTokensFor(u.Username, u.Role == "admin")

	modelAliases := invertAliasMap(a.state.AliasMap())

	rows := make([]ClientTokenRow, 0, len(rawTokens))
	for _, t := range rawTokens {
		row := ClientTokenRow{ClientToken: t, CSRFToken: bp.CSRFToken}
		connInfos := a.hub.ConnectedClientsByToken(t.Token)
		if len(connInfos) > 0 {
			if len(connInfos) == 1 {
				row.Status = "connected"
				row.StatusClass = "connected"
				row.StatusLabel = "\u25cf connected"
			} else {
				row.Status = fmt.Sprintf("%d connected", len(connInfos))
				row.StatusClass = "multi_connected"
				row.StatusLabel = fmt.Sprintf("\u25cf %d connected", len(connInfos))
			}
			mods := a.hub.ConnectedModels(t.Token)
			sort.Strings(mods)
			for _, m := range mods {
				row.Models = append(row.Models, ModelWithAliases{
					Name:    m,
					Aliases: modelAliases[m],
				})
			}
			isAdmin := u.Role == "admin"
			isTokenOwner := t.Owner == u.Username
			for _, ci := range connInfos {
				var jobs []InFlightJobRow
				for _, rec := range a.hub.InFlightJobsByClientID(ci.ID) {
					jobs = append(jobs, InFlightJobRow{
						ID:              rec.Req.ID,
						Owner:           rec.Req.Owner,
						APIKeyLabel:     rec.Req.APIKeyLabel,
						Model:           rec.Req.Model,
						EnqueuedAt:      humanTime(rec.Req.EnqueuedAt),
						DispatchedAtISO: rec.DispatchedAt.UTC().Format(time.RFC3339),
						WordCount:       rec.Req.WordCount,
						CanCancel:       isAdmin || rec.Req.Owner == u.Username || isTokenOwner,
					})
				}
				row.Connections = append(row.Connections, ConnectedClientRow{
					Name:          ci.Name,
					Version:       ci.Version,
					Models:        strings.Join(ci.Models, ", "),
					InFlight:      ci.InFlight,
					MaxConcurrent: ci.MaxConcurrent,
					Jobs:          jobs,
				})
			}
		} else if ls := a.hub.LastSeenTime(t.Token); !ls.IsZero() {
			row.Status = "offline"
			row.StatusClass = "offline"
			row.StatusLabel = "\u25cb offline"
			row.LastSeen = humanTime(ls)
		} else {
			row.Status = "never_connected"
			row.StatusClass = "never_connected"
			row.StatusLabel = "\u25cb never connected"
		}
		rows = append(rows, row)
	}
	page := ClientTokensPage{
		basePage:  bp,
		NewToken:  newToken,
		FormError: formErr,
	}
	if u.Role == "admin" {
		page.Groups = buildClientGroups(rows)
	} else {
		page.Tokens = rows
	}
	a.render(w, "clients", page)
}

func (a *Admin) handleClientTokenCreate(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		a.renderClientTokens(w, u, "", "Name is required.")
		return
	}
	tokVal, err := GenClientTokenValue(u.Username)
	if err != nil {
		a.renderClientTokens(w, u, "", "Failed to generate token.")
		return
	}
	t := ClientToken{
		Name:        name,
		Owner:       u.Username,
		Token:       tokVal,
		CreatedAt:   time.Now().UTC(),
		SharedSlots: -1, // fully shared by default
	}
	if err := a.state.AddClientToken(t); err != nil {
		a.renderClientTokens(w, u, "", err.Error())
		return
	}
	a.renderClientTokens(w, u, tokVal, "")
}

func (a *Admin) handleClientTokenRevoke(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	token := r.FormValue("token")
	a.state.RevokeClientToken(u.Username, token, u.Role == "admin")
	a.hub.CloseByToken(token)
	http.Redirect(w, r, "/portal/clients", http.StatusFound)
}

func (a *Admin) handleClientTokenSharedSlots(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	token := r.FormValue("token")
	slotsStr := strings.TrimSpace(r.FormValue("slots"))
	slots := -1 // default: fully shared
	if slotsStr != "" {
		n, err := strconv.Atoi(slotsStr)
		if err != nil || n < -1 {
			http.Error(w, "invalid slots value", http.StatusBadRequest)
			return
		}
		slots = n
	}
	if err := a.state.SetClientTokenSharedSlots(u.Username, token, slots, u.Role == "admin"); err != nil {
		a.log.Warn("admin: shared slots update rejected", "actor", u.Username, "error", err)
		http.Redirect(w, r, "/portal/clients", http.StatusFound)
		return
	}
	a.hub.SetClientSharedSlots(token, slots)
	http.Redirect(w, r, "/portal/clients", http.StatusFound)
}

// handleClientTokenConfig serves a pre-filled config.yaml for the given token.
func (a *Admin) handleClientTokenConfig(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}
	ct, ok := a.state.LookupClientToken(token)
	if !ok {
		http.Error(w, "token not found", http.StatusNotFound)
		return
	}
	if ct.Owner != u.Username && u.Role != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	yaml := fmt.Sprintf("router_url: \"wss://%s/ws/client\"\nrouter_token: \"%s\"\nmax_concurrent: 4\nmodels:\n  - name: \"llama3.2:3b\"\n    endpoint: \"http://localhost:8080\"\n", a.host, token)
	w.Header().Set("Content-Type", "application/x-yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="config.yaml"`)
	fmt.Fprint(w, yaml)
}

// handleShimConfig serves a pre-filled shim config.yaml for the given client token.
func (a *Admin) handleShimConfig(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}
	ct, ok := a.state.LookupClientToken(token)
	if !ok {
		http.Error(w, "token not found", http.StatusNotFound)
		return
	}
	if ct.Owner != u.Username && u.Role != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	yaml := fmt.Sprintf(`router_url: "wss://%s/ws/client"
router_token: "%s"
max_concurrent: 4

models:
  # OpenAI example
  - name: "gpt-4o"
    context_size: 128000
    backend:
      type: http
      url: "https://api.openai.com"
      format: openai
      auth_type: bearer
      auth_value: "${OPENAI_API_KEY}"

  # Anthropic example
  - name: "claude-sonnet-4-5"
    context_size: 200000
    backend:
      type: http
      url: "https://api.anthropic.com"
      format: anthropic
      auth_type: bearer
      auth_value: "${ANTHROPIC_API_KEY}"

  # Command adapter example (uncomment and edit)
  # - name: "my-model"
  #   backend:
  #     type: command
  #     command: "/path/to/adapter.sh"
`, a.host, token)
	w.Header().Set("Content-Type", "application/x-yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="shim-config.yaml"`)
	fmt.Fprint(w, yaml)
}

// --- Model Aliases ---

func (a *Admin) handleModelAliasCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	alias := strings.TrimSpace(r.FormValue("alias"))
	model := strings.TrimSpace(r.FormValue("model"))
	if alias != "" && model != "" {
		a.state.AddAlias(alias, model) // duplicate errors silently ignored
	}
	http.Redirect(w, r, "/portal/", http.StatusFound)
}

func (a *Admin) handleModelAliasDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	alias := r.FormValue("alias")
	model := r.FormValue("model")
	if model != "" {
		a.state.DeleteAlias(alias, model)
	} else {
		a.state.DeleteAliasGroup(alias)
	}
	// Redirect back to the originating page (dashboard or clients)
	ref := r.FormValue("ref")
	if ref == "clients" {
		http.Redirect(w, r, "/portal/clients", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/portal/", http.StatusFound)
}

// --- Help ---

func (a *Admin) handleHelp(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	a.render(w, "help", a.newBasePage("help", u))
}

// --- Settings ---

func (a *Admin) handleSettings(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	a.renderSettings(w, u, "", "")
}

func (a *Admin) renderSettings(w http.ResponseWriter, u User, flash, errMsg string) {
	users := a.state.Users()
	rows := make([]UserRow, 0, len(users))
	for _, usr := range users {
		rows = append(rows, UserRow{User: usr, IsSelf: usr.Username == u.Username})
	}
	upstream := a.state.GetUpstreamRouters()
	upstreamRows := make([]UpstreamRouterRow, 0, len(upstream))
	for _, r := range upstream {
		connected := a.upstreamConnected != nil && a.upstreamConnected(r.URL)
		upstreamRows = append(upstreamRows, UpstreamRouterRow{UpstreamRouter: r, Connected: connected})
	}
	bp := a.newBasePage("settings", u)
	bp.Flash = flash
	bp.Error = errMsg
	a.render(w, "settings", SettingsPage{
		basePage:  bp,
		Users:     rows,
		Upstreams: upstreamRows,
	})
}

func (a *Admin) handleUpstreamAdd(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	url := strings.TrimSpace(r.FormValue("url"))
	token := strings.TrimSpace(r.FormValue("token"))
	if url == "" || token == "" {
		a.renderSettings(w, u, "", "URL and token are required.")
		return
	}
	if err := a.state.AddUpstreamRouter(UpstreamRouter{Name: name, URL: url, Token: token}); err != nil {
		a.renderSettings(w, u, "", err.Error())
		return
	}
	if a.upstreamReload != nil {
		a.upstreamReload()
	}
	http.Redirect(w, r, "/portal/settings#tab-upstreams", http.StatusFound)
}

func (a *Admin) handleUpstreamRemove(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	upstreamURL := r.FormValue("url")
	if err := a.state.RemoveUpstreamRouter(upstreamURL); err != nil {
		a.renderSettings(w, u, "", err.Error())
		return
	}
	a.log.Info("admin: upstream router removed", "actor", u.Username, "url", upstreamURL)
	if a.upstreamReload != nil {
		a.upstreamReload()
	}
	http.Redirect(w, r, "/portal/settings#tab-upstreams", http.StatusFound)
}

func (a *Admin) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	current := r.FormValue("current")
	newPw := r.FormValue("new")
	confirm := r.FormValue("confirm")
	if newPw != confirm {
		a.renderSettings(w, u, "", "New passwords do not match.")
		return
	}
	// Re-fetch from store to get current hash (context copy may be stale after a prior change).
	fresh, ok := a.state.LookupUser(u.Username)
	if !ok {
		a.renderSettings(w, u, "", "Internal error.")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(fresh.PasswordHash), []byte(current)); err != nil {
		a.renderSettings(w, u, "", "Current password is incorrect.")
		return
	}
	hash, err := HashPassword(newPw)
	if err != nil {
		a.renderSettings(w, u, "", "Internal error.")
		return
	}
	a.state.UpdateUser(u.Username, func(user *User) { user.PasswordHash = hash })
	a.renderSettings(w, u, "Password updated.", "")
}

func (a *Admin) handleAddUser(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	if username == "" || password == "" {
		a.renderSettings(w, u, "", "Username and password are required.")
		return
	}
	if _, exists := a.state.LookupUser(username); exists {
		a.renderSettings(w, u, "", fmt.Sprintf("Username %q already exists.", username))
		return
	}
	hash, err := HashPassword(password)
	if err != nil {
		a.renderSettings(w, u, "", "Internal error.")
		return
	}
	if err := a.state.AddUser(User{Username: username, PasswordHash: hash, Role: "member"}); err != nil {
		a.renderSettings(w, u, "", err.Error())
		return
	}
	a.renderSettings(w, u, fmt.Sprintf("User %q created.", username), "")
}

func (a *Admin) handleUserDisable(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	target := r.FormValue("username")
	if target == u.Username {
		a.renderSettings(w, u, "", "Cannot disable yourself.")
		return
	}
	if err := a.state.UpdateUser(target, func(user *User) { user.Disabled = true }); err != nil {
		a.renderSettings(w, u, "", err.Error())
		return
	}
	http.Redirect(w, r, "/portal/settings", http.StatusFound)
}

func (a *Admin) handleUserEnable(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	target := r.FormValue("username")
	if err := a.state.UpdateUser(target, func(user *User) { user.Disabled = false }); err != nil {
		a.renderSettings(w, u, "", err.Error())
		return
	}
	http.Redirect(w, r, "/portal/settings", http.StatusFound)
}

func (a *Admin) handleUserPromote(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	target := r.FormValue("username")
	if err := a.state.UpdateUser(target, func(user *User) { user.Role = "admin" }); err != nil {
		a.renderSettings(w, u, "", err.Error())
		return
	}
	http.Redirect(w, r, "/portal/settings", http.StatusFound)
}

func (a *Admin) handleUserDemote(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	target := r.FormValue("username")
	if err := a.state.DemoteUser(u.Username, target); err != nil {
		a.renderSettings(w, u, "", err.Error())
		return
	}
	http.Redirect(w, r, "/portal/settings", http.StatusFound)
}

// statsRows converts stats.Stats rows to StatRow slices sorted by total tokens desc.
// byModel=true returns per-model rows; false returns per-user rows.
func statsRows(s *stats.Stats, byModel bool) []StatRow {
	if s == nil {
		return nil
	}
	var rows []stats.Row
	if byModel {
		rows = s.ByModel()
	} else {
		rows = s.ByUser()
	}
	out := make([]StatRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, StatRow{
			Name:             r.Name,
			Requests:         r.Requests,
			PromptTokens:     r.PromptTokens,
			CompletionTokens: r.CompletionTokens,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		ti := out[i].PromptTokens + out[i].CompletionTokens
		tj := out[j].PromptTokens + out[j].CompletionTokens
		return ti > tj
	})
	return out
}

// --- API Key priority ---

func (a *Admin) handleAPIKeyPriority(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	key := r.FormValue("key")
	priority := r.FormValue("priority")
	a.state.UpdateAPIKeyPriority(key, priority) // errors silently ignored; bad input just doesn't save
	http.Redirect(w, r, "/portal/api-keys", http.StatusFound)
}

// buildClientGroups groups ClientTokenRows by owner, sorted: live users first, then alpha.
func buildClientGroups(rows []ClientTokenRow) []ClientUserGroup {
	groupMap := make(map[string]*ClientUserGroup)
	var order []string
	for _, row := range rows {
		if _, exists := groupMap[row.Owner]; !exists {
			groupMap[row.Owner] = &ClientUserGroup{Username: row.Owner}
			order = append(order, row.Owner)
		}
		g := groupMap[row.Owner]
		g.Tokens = append(g.Tokens, row)
		if strings.Contains(row.Status, "connected") {
			g.HasLive = true
		}
	}
	groups := make([]ClientUserGroup, 0, len(groupMap))
	for _, username := range order {
		groups = append(groups, *groupMap[username])
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].HasLive != groups[j].HasLive {
			return groups[i].HasLive
		}
		return groups[i].Username < groups[j].Username
	})
	return groups
}

// --- Job / Queue cancel ---

func (a *Admin) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	reqID := r.FormValue("request_id")
	rec, ok := a.hub.LookupInFlightJob(reqID)
	if ok {
		ct, ctOK := a.state.LookupClientToken(rec.ClientToken)
		isClientOwner := ctOK && ct.Owner == u.Username
		if u.Role != "admin" && rec.Req.Owner != u.Username && !isClientOwner {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		a.hub.CancelRequest(reqID)
	}
	http.Redirect(w, r, "/portal/clients", http.StatusFound)
}

func (a *Admin) handleQueueCancel(w http.ResponseWriter, r *http.Request) {
	// requireAdmin middleware already enforces admin-only.
	reqID := r.FormValue("request_id")
	if a.queue != nil {
		a.queue.PopByID(reqID)
	}
	http.Redirect(w, r, "/portal/", http.StatusFound)
}

// priorityName converts a types.Priority value to its display string.
func priorityName(p int) string {
	switch p {
	case 0:
		return "high"
	case 2:
		return "low"
	default:
		return "normal"
	}
}

// humanTime formats a time as a human-readable relative string.
func humanTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("2006-01-02")
	}
}
