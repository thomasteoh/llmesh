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
	"llmesh/router/internal/hub"
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
	AliasChains   []AliasChainRow     // alias → ordered fallback chain (for the preference UI)
	Clients       []ClientRow
	StatsByModel  []StatRow
	StatsByUser   []StatRow
	QueueLen      int
	QueueItems    []QueuedJobRow // filtered to the requesting user's own items for non-admins
}

// AliasChainRow is one alias and its preference-ordered targets, for the
// fallback-chain UI. Presented alias-first because that is the direction a
// fallback reads; the per-model badges elsewhere show the same data inverted.
type AliasChainRow struct {
	Alias   string
	Targets []AliasTargetRow
}

// AliasTargetRow is one model in an alias's chain, decorated for display.
type AliasTargetRow struct {
	Model string
	Tier  int  // preference tier as stored; lower is preferred
	Live  bool // a connected client currently serves this model
	// Shared marks a target that shares its tier with another target, meaning the
	// two are load-spread rather than ordered. Worth surfacing, since it is the
	// one case where position in the list does not imply preference.
	Shared  bool
	CanUp   bool // not already first
	CanDown bool // not already last
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
	// ID identifies this connection in the rendered markup. Name is what the
	// page shows, but it is only unique within an owner, so the page's script
	// keys everything it patches on ID instead.
	ID            string
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
	CSRFToken       string // for the cancel form when this row is rendered on its own
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
	// Perf is this machine's recent inference performance, or nil when it has
	// served no requests in the window.
	Perf *ClientPerfRow
}

// buildConnRow assembles one live connection and the jobs it is running. Shared
// by the Clients page and the connections API so a connection inserted by the
// page's script carries the same fields, and the same cancel permissions, as one
// the server rendered.
func (a *Admin) buildConnRow(ci hub.ConnectedClientInfo, u User, t ClientToken, csrf string) ConnectedClientRow {
	isAdmin := u.Role == "admin"
	isTokenOwner := t.Owner == u.Username
	var jobs []InFlightJobRow
	for _, rec := range a.hub.InFlightJobsByClientID(ci.ID) {
		canCancel := isAdmin || rec.Req.Owner == u.Username || isTokenOwner
		jobs = append(jobs, buildInFlightJobRow(rec, csrf, canCancel))
	}
	return ConnectedClientRow{
		ID:            ci.ID,
		Name:          ci.Name,
		Version:       ci.Version,
		IsRouter:      strings.HasPrefix(ci.Version, "router/"),
		Models:        strings.Join(ci.Models, ", "),
		InFlight:      ci.InFlight,
		MaxConcurrent: ci.MaxConcurrent,
		Jobs:          jobs,
	}
}

// buildInFlightJobRow renders one live job for display. Shared by the Clients
// page and the jobs API so a row inserted by the page's script is built from the
// same fields, and by the same rules, as one the server rendered — the API hands
// back this row run through the job-row template rather than letting the script
// assemble its own markup.
func buildInFlightJobRow(rec hub.InFlightRecord, csrf string, canCancel bool) InFlightJobRow {
	phase := "processing"
	var firstChunkAtISO string
	var ttftMs int
	if fc := rec.FirstChunkAt(); fc != nil {
		phase = "generating"
		firstChunkAtISO = fc.UTC().Format(time.RFC3339)
	}
	// Output means generating either way, but only a streaming request
	// has a TTFT to show; see hub.InFlightRecord.FirstTokenAt.
	if ft := rec.FirstTokenAt(); ft != nil {
		ttftMs = int(ft.Sub(rec.DispatchedAt).Milliseconds())
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
	return InFlightJobRow{
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
		CanCancel:       canCancel,
		CSRFToken:       csrf,
	}
}

// clientPerfWindow is how far back the Clients page summarises each machine's
// performance. Short enough to reflect the machine's current state (model loaded,
// thermal state, competing load) rather than its history.
const clientPerfWindow = 24 * time.Hour

// ClientPerfRow is a client machine's recent inference performance, pre-formatted
// for display. Empty strings render as an em-dash rather than a misleading zero.
type ClientPerfRow struct {
	Requests   int64
	GenTPS     string // token generation speed, e.g. "38.4 tok/s"
	PromptTPS  string // prompt evaluation speed
	AvgTTFT    string // mean time to first token
	MaxTTFT    string // worst time to first token
	AvgTotal   string // mean end-to-end duration
	MaxTotal   string // worst end-to-end duration
	AvgQueue   string // mean wait for a free slot on this machine
	ByModel    []ModelPerfRow
	Est        bool   // true when some samples were router-observed, not backend-reported
	WindowDesc string // human label for the measurement window, e.g. "24h"
}

// ModelPerfRow is one model's performance on a particular client machine.
type ModelPerfRow struct {
	Name      string
	Requests  int64
	GenTPS    string
	PromptTPS string
	AvgTTFT   string
}

// formatTPS renders a tokens-per-second figure, or "" when unmeasured. Prompt
// evaluation routinely runs into the thousands, so large values are abbreviated.
func formatTPS(v float64) string {
	switch {
	case v <= 0:
		return ""
	case v >= 1000:
		return fmt.Sprintf("%.1fk tok/s", v/1000)
	case v >= 100:
		return fmt.Sprintf("%.0f tok/s", v)
	default:
		return fmt.Sprintf("%.1f tok/s", v)
	}
}

// formatMS renders a millisecond duration at a sensible precision, or "" when
// unmeasured. Sub-second values stay in ms; longer ones read better as seconds.
func formatMS(v float64) string {
	switch {
	case v <= 0:
		return ""
	case v < 1000:
		return fmt.Sprintf("%.0f ms", v)
	case v < 60000:
		return fmt.Sprintf("%.1f s", v/1000)
	default:
		return fmt.Sprintf("%.1f min", v/60000)
	}
}

// newClientPerfRow formats a client's counters for display, returning nil when the
// machine served nothing in the window.
func newClientPerfRow(p PerfStats, byModel map[string]PerfStats) *ClientPerfRow {
	if p.Samples == 0 {
		return nil
	}
	row := &ClientPerfRow{
		Requests:   p.Samples,
		GenTPS:     formatTPS(p.GenTokensPerSec()),
		PromptTPS:  formatTPS(p.PromptTokensPerSec()),
		AvgTTFT:    formatMS(p.AvgTTFTMS()),
		MaxTTFT:    formatMS(p.TTFTMSMax),
		AvgTotal:   formatMS(p.AvgTotalMS()),
		MaxTotal:   formatMS(p.TotalMSMax),
		AvgQueue:   formatMS(p.AvgQueueMS()),
		Est:        p.BackendSamples < p.Samples,
		WindowDesc: "24h",
	}
	for name, mp := range byModel {
		if mp.Samples == 0 {
			continue
		}
		row.ByModel = append(row.ByModel, ModelPerfRow{
			Name:      name,
			Requests:  mp.Samples,
			GenTPS:    formatTPS(mp.GenTokensPerSec()),
			PromptTPS: formatTPS(mp.PromptTokensPerSec()),
			AvgTTFT:   formatMS(mp.AvgTTFTMS()),
		})
	}
	sort.Slice(row.ByModel, func(i, j int) bool { return row.ByModel[i].Name < row.ByModel[j].Name })
	return row
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
	Pricing    []ModelPricingRow
	Currency   string
}

// ModelPricingRow is one model's token rate, as displayed and edited.
type ModelPricingRow struct {
	Model string
	// InputRate and OutputRate are the stored rates formatted for a text input
	// (blank when zero), so the form round-trips what the admin typed.
	InputRate  string
	OutputRate string
	Basis      string // BasisActual | BasisEstimated
	IsActual   bool   // precomputed; templates cannot compare strings inline
	Configured bool   // a pricing row exists, even if it sets a zero rate
	Live       bool   // a connected client currently serves this model
}

// modelPricingRows lists every model worth pricing: those with a rate already,
// those being served right now, and those that merely appear in usage history.
// All three matter. A live-but-unpriced model is silently missing from every cost
// total until someone prices it, and a model only present in history still owns
// its share of a past bill — since rates apply retroactively, leaving it out
// would make that share unreachable from the portal.
func modelPricingRows(pricing map[string]ModelPricing, liveModels, usageModels []string) []ModelPricingRow {
	live := make(map[string]bool, len(liveModels))
	for _, m := range liveModels {
		live[m] = true
	}
	capacity := len(pricing) + len(liveModels) + len(usageModels)
	seen := make(map[string]bool, capacity)
	rows := make([]ModelPricingRow, 0, capacity)
	add := func(model string) {
		if seen[model] {
			return
		}
		seen[model] = true
		p, configured := pricing[model]
		basis := validBasis(p.Basis)
		rows = append(rows, ModelPricingRow{
			Model:      model,
			InputRate:  FormatRatePerMtok(p.InputPPM),
			OutputRate: FormatRatePerMtok(p.OutputPPM),
			Basis:      basis,
			IsActual:   basis == BasisActual,
			Configured: configured,
			Live:       live[model],
		})
	}
	for m := range pricing {
		add(m)
	}
	for _, m := range liveModels {
		add(m)
	}
	for _, m := range usageModels {
		add(m)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Model < rows[j].Model })
	return rows
}

type UserRow struct {
	User
	IsSelf bool
}

// aliasChainRows builds the preference-ordered view of every alias. liveModels is
// the set of models a connected client currently serves, used to mark targets
// that are reachable right now — the whole point of a fallback chain is knowing
// which tier traffic is actually landing on.
func aliasChainRows(targets map[string][]types.AliasTarget, liveModels []string) []AliasChainRow {
	live := make(map[string]bool, len(liveModels))
	for _, m := range liveModels {
		live[m] = true
	}
	rows := make([]AliasChainRow, 0, len(targets))
	for alias, ts := range targets {
		out := make([]AliasTargetRow, 0, len(ts))
		for i, t := range ts {
			shared := (i > 0 && ts[i-1].Priority == t.Priority) ||
				(i+1 < len(ts) && ts[i+1].Priority == t.Priority)
			out = append(out, AliasTargetRow{
				Model:   t.Model,
				Tier:    t.Priority,
				Live:    live[t.Model],
				Shared:  shared,
				CanUp:   i > 0,
				CanDown: i+1 < len(ts),
			})
		}
		rows = append(rows, AliasChainRow{Alias: alias, Targets: out})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Alias < rows[j].Alias })
	return rows
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
		AliasChains:   aliasChainRows(a.state.AliasTargets(), activeModels),
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
	redirectOrRefresh(w, r, "/portal/api-keys")
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

	// Recent performance per machine, fetched once for the whole page rather than
	// per row. Members see only the speed of their own requests; admins see the
	// machine's aggregate across every caller. A query failure degrades to a page
	// without the perf columns instead of a page that won't render.
	perfOwner := ""
	if u.Role != "admin" {
		perfOwner = u.Username
	}
	perfUntil := time.Now()
	perfSince := perfUntil.Add(-clientPerfWindow)
	perfByClient, err := a.state.PerfByClient(perfSince, perfUntil, perfOwner)
	if err != nil {
		a.log.Error("admin: client perf query", "error", err)
	}
	perfByClientModel, err := a.state.PerfByClientModel(perfSince, perfUntil, perfOwner)
	if err != nil {
		a.log.Error("admin: client model perf query", "error", err)
	}

	rows := make([]ClientTokenRow, 0, len(rawTokens))
	for _, t := range rawTokens {
		row := ClientTokenRow{ClientToken: t, CSRFToken: bp.CSRFToken}
		// Performance is keyed by the client's "owner/name", the same identity the
		// hub stamps onto each sample it records.
		clientLabel := t.Owner + "/" + t.Name
		row.Perf = newClientPerfRow(perfByClient[clientLabel], perfByClientModel[clientLabel])
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
			for _, ci := range connInfos {
				conn := a.buildConnRow(ci, u, t, bp.CSRFToken)
				if conn.IsRouter {
					row.IsRouter = true
				}
				row.Connections = append(row.Connections, conn)
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
	redirectOrRefresh(w, r, "/portal/clients")
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
	redirectOrRefresh(w, r, "/portal/clients")
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
		redirectOrRefresh(w, r, "/portal/clients")
		return
	}
	a.hub.SetClientOwnerSlots(tokenHash, model, slots)
	redirectOrRefresh(w, r, "/portal/clients")
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
	// Blank tier means tier 0, so adding a second target to an alias load-spreads
	// by default rather than silently demoting it behind the first.
	tier := 0
	if v := strings.TrimSpace(r.FormValue("tier")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "tier must be a non-negative whole number", http.StatusBadRequest)
			return
		}
		tier = n
	}
	if alias != "" && model != "" {
		if err := a.state.AddAliasWithPriority(alias, model, tier); err != nil {
			http.Error(w, "could not add alias: "+err.Error(), http.StatusBadRequest)
			return
		}
		a.state.RecordAudit(u.Username, "alias.create",
			fmt.Sprintf("%s=%s@%d", alias, model, tier), a.clientIP(r))
	}
	redirectOrRefresh(w, r, "/portal/")
}

// handleModelAliasReorder promotes or demotes one target within its alias's
// preference chain.
func (a *Admin) handleModelAliasReorder(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	alias := strings.TrimSpace(r.FormValue("alias"))
	model := strings.TrimSpace(r.FormValue("model"))
	dir := r.FormValue("dir")
	delta := 0
	switch dir {
	case "up":
		delta = -1
	case "down":
		delta = 1
	default:
		http.Error(w, "dir must be up or down", http.StatusBadRequest)
		return
	}
	if alias == "" || model == "" {
		http.Error(w, "alias and model are required", http.StatusBadRequest)
		return
	}
	if err := a.state.MoveAliasTarget(alias, model, delta); err != nil {
		http.Error(w, "could not reorder alias: "+err.Error(), http.StatusBadRequest)
		return
	}
	a.state.RecordAudit(u.Username, "alias.reorder", alias+"="+model+" "+dir, a.clientIP(r))
	redirectOrRefresh(w, r, "/portal/")
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
		redirectOrRefresh(w, r, "/portal/clients")
		return
	}
	redirectOrRefresh(w, r, "/portal/")
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
	pricing, err := a.state.ModelPricingAll()
	if err != nil {
		a.log.Error("admin: model pricing query", "error", err)
		pricing = map[string]ModelPricing{}
	}
	activeModels := a.hub.ActiveModels()
	sort.Strings(activeModels)
	usageModels, err := a.state.UsageModels()
	if err != nil {
		a.log.Error("admin: usage models query", "error", err)
	}
	a.render(w, "settings", SettingsPage{
		basePage:   bp,
		Users:      rows,
		Upstreams:  upstreamRows,
		Opt:        a.state.RequestOpts(),
		PortalHost: a.state.PortalHost(),
		Pricing:    modelPricingRows(pricing, activeModels, usageModels),
		Currency:   a.state.CostCurrency(),
	})
}

// handleModelPricingUpdate upserts one model's token rates.
func (a *Admin) handleModelPricingUpdate(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	if model == "" {
		a.renderSettings(w, r, u, "", "model is required")
		return
	}
	inPPM, err := ParseRatePerMtok(r.FormValue("input_rate"))
	if err != nil {
		a.renderSettings(w, r, u, "", "input "+err.Error())
		return
	}
	outPPM, err := ParseRatePerMtok(r.FormValue("output_rate"))
	if err != nil {
		a.renderSettings(w, r, u, "", "output "+err.Error())
		return
	}
	basis := validBasis(r.FormValue("basis"))
	if err := a.state.SetModelPricing(model, inPPM, outPPM, basis); err != nil {
		a.renderSettings(w, r, u, "", "could not save pricing: "+err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "pricing.set",
		fmt.Sprintf("%s in=%d out=%d %s", model, inPPM, outPPM, basis), a.clientIP(r))
	a.renderSettings(w, r, u, "Pricing saved for "+model+".", "")
}

// handleModelPricingDelete returns a model to unpriced.
func (a *Admin) handleModelPricingDelete(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	if err := a.state.DeleteModelPricing(model); err != nil {
		a.renderSettings(w, r, u, "", "could not clear pricing: "+err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "pricing.delete", model, a.clientIP(r))
	a.renderSettings(w, r, u, "Pricing cleared for "+model+".", "")
}

// handleCostCurrencyUpdate sets the display currency label.
func (a *Admin) handleCostCurrencyUpdate(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	code := strings.ToUpper(strings.TrimSpace(r.FormValue("currency")))
	if err := a.state.SetCostCurrency(code); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "pricing.currency", code, a.clientIP(r))
	a.renderSettings(w, r, u, "Currency updated.", "")
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
	redirectOrRefresh(w, r, "/portal/settings#tab-upstreams")
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
	redirectOrRefresh(w, r, "/portal/settings#tab-upstreams")
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
	redirectOrRefresh(w, r, "/portal/settings#tab-optimization")
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
	redirectOrRefresh(w, r, "/portal/settings")
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
	redirectOrRefresh(w, r, "/portal/settings")
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
	redirectOrRefresh(w, r, "/portal/settings")
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
	redirectOrRefresh(w, r, "/portal/settings")
}

func (a *Admin) handleUserResetPassword(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	target := r.FormValue("username")
	if target == u.Username {
		a.renderSettings(w, r, u, "", "Use Change Password to update your own account.")
		return
	}
	if _, ok := a.state.LookupUser(target); !ok {
		a.renderSettings(w, r, u, "", fmt.Sprintf("User %q not found.", target))
		return
	}
	temp, err := generateTempPassword()
	if err != nil {
		a.renderSettings(w, r, u, "", "Internal error.")
		return
	}
	hash, err := HashPassword(temp)
	if err != nil {
		a.renderSettings(w, r, u, "", "Internal error.")
		return
	}
	if err := a.state.UpdateUser(target, func(user *User) { user.PasswordHash = hash }); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "user.password_reset", target, a.clientIP(r))
	a.renderSettings(w, r, u, fmt.Sprintf("Temporary password for %q: %s — copy it now, it will not be shown again. The user should change it after signing in.", target, temp), "")
}

func (a *Admin) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	target := r.FormValue("username")
	if err := a.state.DeleteUser(u.Username, target); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "user.delete", target, a.clientIP(r))
	a.renderSettings(w, r, u, fmt.Sprintf("User %q deleted.", target), "")
}

// handleUserIsolation toggles one of a user's two request-isolation flags. The
// "field" form value selects the direction (send|receive) and "value" the new
// state (1|0); the other flag is preserved.
func (a *Admin) handleUserIsolation(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	target := r.FormValue("username")
	field := r.FormValue("field")
	enabled := r.FormValue("value") == "1"
	cur, ok := a.state.LookupUser(target)
	if !ok {
		a.renderSettings(w, r, u, "", fmt.Sprintf("User %q not found.", target))
		return
	}
	send, receive := cur.SendIsolation, cur.ReceiveIsolation
	switch field {
	case "send":
		send = enabled
	case "receive":
		receive = enabled
	default:
		a.renderSettings(w, r, u, "", "Invalid isolation setting.")
		return
	}
	if err := a.state.SetUserIsolation(target, send, receive); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "user.isolation", fmt.Sprintf("%s %s=%t", target, field, enabled), a.clientIP(r))
	redirectOrRefresh(w, r, "/portal/settings#tab-users")
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
	redirectOrRefresh(w, r, "/portal/api-keys")
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
	redirectOrRefresh(w, r, "/portal/api-keys")
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
	redirectOrRefresh(w, r, "/portal/clients")
}

func (a *Admin) handleQueueCancel(w http.ResponseWriter, r *http.Request) {
	// requireAdmin middleware already enforces admin-only.
	reqID := r.FormValue("request_id")
	if a.queue != nil {
		a.queue.PopByID(reqID)
	}
	redirectOrRefresh(w, r, "/portal/")
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
