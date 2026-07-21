package admin

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"llmesh/pkg/types"
	"llmesh/router/internal/stats"
)

// clientStatusBadge is the single source of truth for how a client token's
// connection state maps to a status key, CSS badge class, and display label.
// Used by both the server-rendered pages and the dashboard JSON API so the
// badge never has to be reconstructed in JavaScript.
func clientStatusBadge(connCount int, hasLastSeen bool) (status, class, label string) {
	switch {
	case connCount == 1:
		return "connected", "connected", "● connected"
	case connCount > 1:
		s := fmt.Sprintf("%d connected", connCount)
		return s, "multi_connected", "● " + s
	case hasLastSeen:
		return "offline", "offline", "○ offline"
	default:
		return "never_connected", "never_connected", "○ never connected"
	}
}

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
	Users     []string // all usernames, for admin "create on behalf of" datalist
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
	Users     []string // all usernames, for admin "create on behalf of" datalist
}

type ConnectedClientRow struct {
	Name          string
	Version       string
	IsRouter      bool // true when Version starts with "router/" (downstream router, not a genuine client)
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
	FirstChunkAtISO string // RFC3339; empty while still processing
	TTFTMs          int    // time-to-first-token in ms; 0 while processing
	DeltaCount      int    // tokens generated so far
	WordCount       int
	Priority        string // "high" | "low" | "" (normal — no badge)
	Attempts        int    // > 1 means job has been retried
	StatsStr        string // pre-rendered static stats for initial display
	Phase           string // "processing" | "generating"
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

// ModelSlotRow holds per-model owner-slot data for display and the owner-slots form.
type ModelSlotRow struct {
	Name       string
	OwnerSlots int // 0 = fully shared
}

type ClientTokenRow struct {
	ClientToken
	Status      string
	StatusClass string // CSS badge class
	StatusLabel string // display label with symbol
	LastSeen    string
	IsRouter    bool // true when any live connection is a downstream router (version "router/…")
	Models      []ModelWithAliases
	ModelSlots  []ModelSlotRow // per-model owner-slot configuration
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
	Opt       types.RequestOptimization
	// PortalHost is the admin-set host override (empty when unset). The resolved
	// value in effect is basePage.Host; this is the raw stored override so the
	// form shows blank when the host is auto-detected rather than pinned.
	PortalHost string
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

func (a *Admin) newBasePage(page string, u User, r *http.Request) basePage {
	bp := basePage{
		Page:          page,
		Username:      u.Username,
		IsAdmin:       u.Role == "admin",
		RouterVersion: a.routerVersion,
		Name:          a.name,
		Host:          a.effectiveHost(r),
	}
	// Read the session's CSRF token (set once at login, stable for the session).
	// Session-scoped tokens let concurrent tabs for the same user operate independently.
	if c, err := r.Cookie(sessionCookie); err == nil {
		if token, ok := a.sessions.getCSRF(c.Value); ok {
			bp.CSRFToken = token
		}
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
			Name: t.Owner + "/" + t.Name,
		}
		connCount := a.hub.ConnectedCountByToken(t.TokenHash)
		ls := a.hub.LastSeenTime(t.TokenHash)
		row.Status, row.StatusClass, row.StatusLabel = clientStatusBadge(connCount, !ls.IsZero())
		if connCount > 0 {
			mods := a.hub.ConnectedModels(t.TokenHash)
			sort.Strings(mods)
			row.Models = strings.Join(mods, ", ")
			row.Version = a.hub.ConnectedVersion(t.TokenHash)
		} else if !ls.IsZero() {
			row.LastSeen = humanTime(ls)
		}
		clients = append(clients, row)
	}
	activeModels := a.hub.ActiveModels()
	sort.Strings(activeModels)
	activeAliases := a.state.AliasMap()
	modelAliases := invertAliasMap(activeAliases)
	data := DashboardPage{
		basePage:      a.newBasePage("dashboard", u, r),
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
	a.renderAPIKeys(w, r, u, "", "")
}

func (a *Admin) renderAPIKeys(w http.ResponseWriter, r *http.Request, u User, newKey, formErr string) {
	keys := a.state.APIKeysFor(u.Username, u.Role == "admin")
	page := APIKeysPage{
		basePage:  a.newBasePage("api-keys", u, r),
		Keys:      keys,
		NewKey:    newKey,
		FormError: formErr,
	}
	if u.Role == "admin" {
		for _, us := range a.state.Users() {
			page.Users = append(page.Users, us.Username)
		}
	}
	a.render(w, "api-keys", page)
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
		a.renderAPIKeys(w, r, u, "", "Label is required.")
		return
	}
	// Admins may create a key on behalf of another user.
	owner := u.Username
	if u.Role == "admin" {
		if forUser := strings.TrimSpace(r.FormValue("for_user")); forUser != "" && forUser != u.Username {
			if _, ok := a.state.LookupUser(forUser); !ok {
				a.renderAPIKeys(w, r, u, "", fmt.Sprintf("User %q not found.", forUser))
				return
			}
			owner = forUser
		}
	}
	keyVal, err := GenAPIKeyValue(owner)
	if err != nil {
		a.renderAPIKeys(w, r, u, "", "Failed to generate key.")
		return
	}
	k := APIKey{
		Label:     label,
		Owner:     owner,
		KeyHash:   HashSecret(keyVal),
		KeyPrefix: SecretPrefix(keyVal),
		Priority:  priority,
		CreatedAt: time.Now().UTC(),
	}
	if err := a.state.AddAPIKey(k); err != nil {
		a.renderAPIKeys(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "api_key.create", owner+"/"+label, a.clientIP(r))
	a.renderAPIKeys(w, r, u, keyVal, "")
}

func (a *Admin) handleAPIKeyRevoke(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	keyHash := r.FormValue("key_hash")
	// Resolve the label before deletion so the audit entry names the key.
	target := ""
	if k, ok := a.state.LookupAPIKeyByHash(keyHash); ok {
		target = k.Owner + "/" + k.Label
	}
	if err := a.state.RevokeAPIKey(u.Username, keyHash, u.Role == "admin"); err != nil {
		a.log.Warn("admin: api key revoke failed", "actor", u.Username, "error", err)
	} else {
		a.state.RecordAudit(u.Username, "api_key.revoke", target, a.clientIP(r))
	}
	http.Redirect(w, r, "/portal/api-keys", http.StatusFound)
}

// --- Clients ---

func (a *Admin) handleClientTokens(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	a.renderClientTokens(w, r, u, "", "")
}

func (a *Admin) renderClientTokens(w http.ResponseWriter, r *http.Request, u User, newToken, formErr string) {
	bp := a.newBasePage("clients", u, r)

	rawTokens := a.state.ClientTokensFor(u.Username, u.Role == "admin")

	modelAliases := invertAliasMap(a.state.AliasMap())

	rows := make([]ClientTokenRow, 0, len(rawTokens))
	for _, t := range rawTokens {
		row := ClientTokenRow{ClientToken: t, CSRFToken: bp.CSRFToken}
		connInfos := a.hub.ConnectedClientsByToken(t.TokenHash)
		if len(connInfos) > 0 {
			row.Status, row.StatusClass, row.StatusLabel = clientStatusBadge(len(connInfos), false)
			mods := a.hub.ConnectedModels(t.TokenHash)
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
					phase := "processing"
					var firstChunkAtISO string
					var ttftMs int
					if fc := rec.FirstChunkAt(); fc != nil {
						phase = "generating"
						ttftMs = int(fc.Sub(rec.DispatchedAt).Milliseconds())
						firstChunkAtISO = fc.UTC().Format(time.RFC3339)
					}
					var statParts []string
					if ttftMs > 0 {
						statParts = append(statParts, fmt.Sprintf("ttft %.1fs", float64(ttftMs)/1000))
					}
					if dc := rec.DeltaCount(); dc > 0 {
						statParts = append(statParts, fmt.Sprintf("%d tok", dc))
					}
					if rec.Req.WordCount > 0 {
						statParts = append(statParts, fmt.Sprintf("%dw in", rec.Req.WordCount))
					}
					statsStr := ""
					if len(statParts) > 0 {
						statsStr = " · " + strings.Join(statParts, " · ")
					}
					priority := ""
					switch rec.Req.Priority {
					case types.PriorityHigh:
						priority = "high"
					case types.PriorityLow:
						priority = "low"
					}
					jobs = append(jobs, InFlightJobRow{
						ID:              rec.Req.ID,
						Owner:           rec.Req.Owner,
						APIKeyLabel:     rec.Req.APIKeyLabel,
						Model:           rec.Req.Model,
						EnqueuedAt:      humanTime(rec.Req.EnqueuedAt),
						DispatchedAtISO: rec.DispatchedAt.UTC().Format(time.RFC3339),
						FirstChunkAtISO: firstChunkAtISO,
						TTFTMs:          ttftMs,
						DeltaCount:      int(rec.DeltaCount()),
						WordCount:       rec.Req.WordCount,
						Priority:        priority,
						Attempts:        rec.Req.Attempts,
						StatsStr:        statsStr,
						Phase:           phase,
						CanCancel:       isAdmin || rec.Req.Owner == u.Username || isTokenOwner,
					})
				}
				isRouter := strings.HasPrefix(ci.Version, "router/")
				if isRouter {
					row.IsRouter = true
				}
				row.Connections = append(row.Connections, ConnectedClientRow{
					Name:          ci.Name,
					Version:       ci.Version,
					IsRouter:      isRouter,
					Models:        strings.Join(ci.Models, ", "),
					InFlight:      ci.InFlight,
					MaxConcurrent: ci.MaxConcurrent,
					Jobs:          jobs,
				})
			}
		} else if ls := a.hub.LastSeenTime(t.TokenHash); !ls.IsZero() {
			row.Status, row.StatusClass, row.StatusLabel = clientStatusBadge(0, true)
			row.LastSeen = humanTime(ls)
		} else {
			row.Status, row.StatusClass, row.StatusLabel = clientStatusBadge(0, false)
		}
		// Build ModelSlots: union of live model names and OwnerSlots keys (offline
		// tokens may have limits on models they no longer advertise).
		modelSet := make(map[string]bool)
		for _, mwa := range row.Models {
			modelSet[mwa.Name] = true
		}
		for m := range t.OwnerSlots {
			modelSet[m] = true
		}
		for m := range modelSet {
			row.ModelSlots = append(row.ModelSlots, ModelSlotRow{
				Name:       m,
				OwnerSlots: t.OwnerSlots[m],
			})
		}
		sort.Slice(row.ModelSlots, func(i, j int) bool {
			return row.ModelSlots[i].Name < row.ModelSlots[j].Name
		})
		rows = append(rows, row)
	}
	page := ClientTokensPage{
		basePage:  bp,
		NewToken:  newToken,
		FormError: formErr,
	}
	if u.Role == "admin" {
		page.Groups = buildClientGroups(rows)
		for _, us := range a.state.Users() {
			page.Users = append(page.Users, us.Username)
		}
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
		a.renderClientTokens(w, r, u, "", "Name is required.")
		return
	}
	// Admins may create a token on behalf of another user.
	owner := u.Username
	if u.Role == "admin" {
		if forUser := strings.TrimSpace(r.FormValue("for_user")); forUser != "" && forUser != u.Username {
			if _, ok := a.state.LookupUser(forUser); !ok {
				a.renderClientTokens(w, r, u, "", fmt.Sprintf("User %q not found.", forUser))
				return
			}
			owner = forUser
		}
	}
	tokVal, err := GenClientTokenValue(owner)
	if err != nil {
		a.renderClientTokens(w, r, u, "", "Failed to generate token.")
		return
	}
	t := ClientToken{
		Name:        name,
		Owner:       owner,
		TokenHash:   HashSecret(tokVal),
		TokenPrefix: SecretPrefix(tokVal),
		CreatedAt:   time.Now().UTC(),
	}
	if err := a.state.AddClientToken(t); err != nil {
		a.renderClientTokens(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "client_token.create", owner+"/"+name, a.clientIP(r))
	a.renderClientTokens(w, r, u, tokVal, "")
}

func (a *Admin) handleClientTokenRevoke(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tokenHash := r.FormValue("token_hash")
	target := ""
	if t, ok := a.state.LookupClientTokenByHash(tokenHash); ok {
		target = t.Owner + "/" + t.Name
	}
	if err := a.state.RevokeClientToken(u.Username, tokenHash, u.Role == "admin"); err != nil {
		a.log.Warn("admin: client token revoke failed", "actor", u.Username, "error", err)
	} else {
		a.hub.CloseByToken(tokenHash)
		a.state.RecordAudit(u.Username, "client_token.revoke", target, a.clientIP(r))
	}
	http.Redirect(w, r, "/portal/clients", http.StatusFound)
}

func (a *Admin) handleClientUpdate(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tokenHash := r.FormValue("token_hash")
	if tokenHash == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}
	ct, ok := a.state.LookupClientTokenByHash(tokenHash)
	if !ok || (u.Role != "admin" && ct.Owner != u.Username) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	n := a.hub.TriggerClientUpdate(tokenHash)
	if n == 0 {
		a.log.Warn("admin: trigger update - no clients connected", "actor", u.Username)
	} else {
		a.log.Info("admin: triggered client update", "actor", u.Username, "clients", n)
	}
	http.Redirect(w, r, "/portal/clients", http.StatusFound)
}

func (a *Admin) handleClientTokenOwnerSlots(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tokenHash := r.FormValue("token_hash")
	model := strings.TrimSpace(r.FormValue("model"))
	if model == "" {
		http.Error(w, "model is required", http.StatusBadRequest)
		return
	}
	slotsStr := strings.TrimSpace(r.FormValue("slots"))
	slots := 0 // default: fully shared (remove reservation)
	if slotsStr != "" {
		n, err := strconv.Atoi(slotsStr)
		if err != nil || n < 0 {
			http.Error(w, "invalid slots value", http.StatusBadRequest)
			return
		}
		slots = n
	}
	if err := a.state.SetClientTokenOwnerSlots(u.Username, tokenHash, model, slots, u.Role == "admin"); err != nil {
		a.log.Warn("admin: owner slots update rejected", "actor", u.Username, "error", err)
		http.Redirect(w, r, "/portal/clients", http.StatusFound)
		return
	}
	a.hub.SetClientOwnerSlots(tokenHash, model, slots)
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
	yaml := fmt.Sprintf("router_url: \"wss://%s/ws/client\"\nrouter_token: \"%s\"\n# max_concurrent: 4        # optional - omit to auto-detect from llama.cpp slot count\nmodels:\n  - endpoint: \"http://localhost:8080\"   # model name auto-detected from this endpoint's /v1/models\n", a.effectiveHost(r), token)
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
`, a.effectiveHost(r), token)
	w.Header().Set("Content-Type", "application/x-yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="shim-config.yaml"`)
	fmt.Fprint(w, yaml)
}

// --- Model Aliases ---

func (a *Admin) handleModelAliasCreate(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	alias := strings.TrimSpace(r.FormValue("alias"))
	model := strings.TrimSpace(r.FormValue("model"))
	if alias != "" && model != "" {
		if err := a.state.AddAlias(alias, model); err != nil {
			http.Error(w, "could not add alias: "+err.Error(), http.StatusBadRequest)
			return
		}
		a.state.RecordAudit(u.Username, "alias.create", alias+"="+model, a.clientIP(r))
	}
	http.Redirect(w, r, "/portal/", http.StatusFound)
}

func (a *Admin) handleModelAliasDelete(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	alias := r.FormValue("alias")
	model := r.FormValue("model")
	if model != "" {
		if err := a.state.DeleteAlias(alias, model); err != nil {
			http.Error(w, "could not delete alias: "+err.Error(), http.StatusBadRequest)
			return
		}
		a.state.RecordAudit(u.Username, "alias.delete", alias+"="+model, a.clientIP(r))
	} else {
		if err := a.state.DeleteAliasGroup(alias); err != nil {
			http.Error(w, "could not delete alias group: "+err.Error(), http.StatusBadRequest)
			return
		}
		a.state.RecordAudit(u.Username, "alias.delete_group", alias, a.clientIP(r))
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
	a.render(w, "help", a.newBasePage("help", u, r))
}

// --- Settings ---

func (a *Admin) handleSettings(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	a.renderSettings(w, r, u, "", "")
}

func (a *Admin) renderSettings(w http.ResponseWriter, r *http.Request, u User, flash, errMsg string) {
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
	bp := a.newBasePage("settings", u, r)
	bp.Flash = flash
	bp.Error = errMsg
	a.render(w, "settings", SettingsPage{
		basePage:   bp,
		Users:      rows,
		Upstreams:  upstreamRows,
		Opt:        a.state.RequestOpts(),
		PortalHost: a.state.PortalHost(),
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
	priority := r.FormValue("priority")
	if priority == "" {
		priority = "normal"
	}
	if url == "" || token == "" {
		a.renderSettings(w, r, u, "", "URL and token are required.")
		return
	}
	if err := a.state.AddUpstreamRouter(UpstreamRouter{Name: name, URL: url, Token: token, Priority: priority}); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "upstream.add", url, a.clientIP(r))
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
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "upstream.remove", upstreamURL, a.clientIP(r))
	a.log.Info("admin: upstream router removed", "actor", u.Username, "url", upstreamURL)
	if a.upstreamReload != nil {
		a.upstreamReload()
	}
	http.Redirect(w, r, "/portal/settings#tab-upstreams", http.StatusFound)
}

// optFormKeys lists the request-optimization settings keys in the order they
// appear on the form. Each maps to a checkbox whose form field name is the key.
var optFormKeys = []string{
	"reqopt.coalesce_normalize",
	"reqopt.prefix_affinity",
	"reqopt.clean_requests",
	"reqopt.clean_aggressive",
	"reqopt.clamp_params",
}

func (a *Admin) handleOptimizationUpdate(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for _, key := range optFormKeys {
		enabled := r.FormValue(key) != ""
		if err := a.state.SetRequestOpt(key, enabled); err != nil {
			a.renderSettings(w, r, u, "", err.Error())
			return
		}
	}
	a.state.RecordAudit(u.Username, "settings.optimization", "", a.clientIP(r))
	a.log.Info("admin: request-optimization settings updated", "actor", u.Username)
	http.Redirect(w, r, "/portal/settings#tab-optimization", http.StatusFound)
}

// hostPattern matches a bare hostname or IP with an optional port. It rejects a
// scheme, path, whitespace, or quotes, all of which would corrupt the URLs and
// downloadable YAML the host is interpolated into.
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]+)?$`)

// normalizePortalHost cleans an admin-supplied host. An empty input is valid and
// means "clear the override" (revert to configured/auto-detected). A non-empty
// value has any scheme and trailing path stripped, then must be a plain
// host[:port]; otherwise an error is returned.
func normalizePortalHost(in string) (string, error) {
	h := strings.TrimSpace(in)
	if h == "" {
		return "", nil
	}
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	h = strings.TrimSuffix(h, "/")
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	if !hostPattern.MatchString(h) {
		return "", fmt.Errorf("invalid host %q: use a hostname or host:port, without scheme or path", in)
	}
	return h, nil
}

func (a *Admin) handleHostUpdate(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	host, err := normalizePortalHost(r.FormValue("host"))
	if err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	if err := a.state.SetPortalHost(host); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "settings.host", host, a.clientIP(r))
	if host == "" {
		a.renderSettings(w, r, u, "Public host cleared; it will be detected automatically.", "")
		return
	}
	a.renderSettings(w, r, u, fmt.Sprintf("Public host set to %q.", host), "")
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
		a.renderSettings(w, r, u, "", "New passwords do not match.")
		return
	}
	// Re-fetch from store to get current hash (context copy may be stale after a prior change).
	fresh, ok := a.state.LookupUser(u.Username)
	if !ok {
		a.renderSettings(w, r, u, "", "Internal error.")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(fresh.PasswordHash), []byte(current)); err != nil {
		a.renderSettings(w, r, u, "", "Current password is incorrect.")
		return
	}
	hash, err := HashPassword(newPw)
	if err != nil {
		a.renderSettings(w, r, u, "", "Internal error.")
		return
	}
	a.state.UpdateUser(u.Username, func(user *User) { user.PasswordHash = hash })
	a.state.RecordAudit(u.Username, "user.password_change", u.Username, a.clientIP(r))
	a.renderSettings(w, r, u, "Password updated.", "")
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
		a.renderSettings(w, r, u, "", "Username and password are required.")
		return
	}
	if _, exists := a.state.LookupUser(username); exists {
		a.renderSettings(w, r, u, "", fmt.Sprintf("Username %q already exists.", username))
		return
	}
	hash, err := HashPassword(password)
	if err != nil {
		a.renderSettings(w, r, u, "", "Internal error.")
		return
	}
	if err := a.state.AddUser(User{Username: username, PasswordHash: hash, Role: "member"}); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "user.add", username, a.clientIP(r))
	a.renderSettings(w, r, u, fmt.Sprintf("User %q created.", username), "")
}

func (a *Admin) handleUserDisable(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	target := r.FormValue("username")
	if target == u.Username {
		a.renderSettings(w, r, u, "", "Cannot disable yourself.")
		return
	}
	if err := a.state.UpdateUser(target, func(user *User) { user.Disabled = true }); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "user.disable", target, a.clientIP(r))
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
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "user.enable", target, a.clientIP(r))
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
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "user.promote", target, a.clientIP(r))
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
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "user.demote", target, a.clientIP(r))
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
	keyHash := r.FormValue("key_hash")
	priority := r.FormValue("priority")
	if err := a.state.UpdateAPIKeyPriority(keyHash, priority); err != nil {
		http.Error(w, "could not update priority: "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/portal/api-keys", http.StatusFound)
}

// --- API Key max concurrent ---

func (a *Admin) handleAPIKeyMaxConcurrent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	keyHash := r.FormValue("key_hash")
	limitStr := strings.TrimSpace(r.FormValue("max_concurrent"))
	var limit int
	if limitStr != "" {
		n, err := strconv.Atoi(limitStr)
		if err != nil || n < 0 {
			http.Error(w, "max_concurrent must be a non-negative integer", http.StatusBadRequest)
			return
		}
		limit = n
	}
	if err := a.state.UpdateAPIKeyMaxConcurrent(keyHash, limit); err != nil {
		http.Error(w, "could not update max_concurrent: "+err.Error(), http.StatusBadRequest)
		return
	}
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
		// rec.ClientToken holds the token hash (the hub never sees plaintext).
		ct, ctOK := a.state.LookupClientTokenByHash(rec.ClientToken)
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
