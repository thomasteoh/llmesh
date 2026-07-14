package admin

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"llmesh/router/internal/logring"
)

type statRowJSON struct {
	Name             string `json:"name"`
	Requests         int64  `json:"requests"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
}

type dashboardJSON struct {
	TotalRequests int64               `json:"total_requests"`
	ActiveClients int                 `json:"active_clients"`
	APIKeyCount   int                 `json:"api_key_count"`
	TokenCount    int                 `json:"token_count"`
	ActiveModels  []string            `json:"active_models"`
	ActiveAliases map[string][]string `json:"active_aliases"`
	Clients       []clientJSON        `json:"clients"`
	StatsByModel  []statRowJSON       `json:"stats_by_model,omitempty"`
	StatsByUser   []statRowJSON       `json:"stats_by_user,omitempty"`
}

type clientJSON struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	StatusClass string `json:"status_class"`
	StatusLabel string `json:"status_label"`
	LastSeen    string `json:"last_seen,omitempty"`
	Models      string `json:"models,omitempty"`
	Version     string `json:"version,omitempty"`
}

func toStatRowJSON(rows []StatRow) []statRowJSON {
	if len(rows) == 0 {
		return nil
	}
	out := make([]statRowJSON, len(rows))
	for i, r := range rows {
		out[i] = statRowJSON{
			Name:             r.Name,
			Requests:         r.Requests,
			PromptTokens:     r.PromptTokens,
			CompletionTokens: r.CompletionTokens,
		}
	}
	return out
}

// ─── Logs API ─────────────────────────────────────────────────────────────────

type logEntryJSON struct {
	Time    string            `json:"time"`
	Level   string            `json:"level"`
	Message string            `json:"msg"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

type logsResponseJSON struct {
	Category string         `json:"category"`
	Entries  []logEntryJSON `json:"entries"`
}

func (a *Admin) handleLogsJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	category := r.URL.Query().Get("category")
	valid := false
	for _, c := range logring.Categories() {
		if c == category {
			valid = true
			break
		}
	}
	if !valid {
		http.Error(w, "invalid category", http.StatusBadRequest)
		return
	}
	limit := 200
	if ls := r.URL.Query().Get("limit"); ls != "" {
		if n, err := strconv.Atoi(ls); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	entries := a.sink.Query(category, limit)
	out := make([]logEntryJSON, len(entries))
	for i, e := range entries {
		out[i] = logEntryJSON{
			Time:    e.Time.Format("2006-01-02T15:04:05.000Z07:00"),
			Level:   e.Level,
			Message: e.Message,
			Attrs:   e.Attrs,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(logsResponseJSON{Category: category, Entries: out})
}

// ─── Dashboard API ────────────────────────────────────────────────────────────

func (a *Admin) handleDashboardJSON(w http.ResponseWriter, r *http.Request) {
	tokens := a.state.ClientTokensFor("", true)
	clients := make([]clientJSON, 0, len(tokens))
	for _, t := range tokens {
		c := clientJSON{Name: t.Owner + "/" + t.Name}
		connCount := a.hub.ConnectedCountByToken(t.TokenHash)
		ls := a.hub.LastSeenTime(t.TokenHash)
		c.Status, c.StatusClass, c.StatusLabel = clientStatusBadge(connCount, !ls.IsZero())
		if connCount > 0 {
			mods := a.hub.ConnectedModels(t.TokenHash)
			sort.Strings(mods)
			c.Models = strings.Join(mods, ", ")
			c.Version = a.hub.ConnectedVersion(t.TokenHash)
		} else if !ls.IsZero() {
			c.LastSeen = humanTime(ls)
		}
		clients = append(clients, c)
	}

	activeModels := a.hub.ActiveModels()
	sort.Strings(activeModels)

	resp := dashboardJSON{
		TotalRequests: a.reqCount(),
		ActiveClients: a.hub.ActiveClientCount(),
		APIKeyCount:   a.state.APIKeyCount(),
		TokenCount:    a.state.ClientTokenCount(),
		ActiveModels:  activeModels,
		ActiveAliases: a.state.AliasMap(),
		Clients:       clients,
		StatsByModel:  toStatRowJSON(statsRows(a.stats, true)),
		StatsByUser:   toStatRowJSON(statsRows(a.stats, false)),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ─── Jobs API ─────────────────────────────────────────────────────────────────

type jobStatJSON struct {
	ID              string `json:"id"`
	Phase           string `json:"phase"`                    // "processing" | "generating"
	DeltaCount      int64  `json:"delta_count"`              // tokens generated so far
	TTFTMs          int64  `json:"ttft_ms,omitempty"`        // time-to-first-token in ms
	FirstChunkAtISO string `json:"first_chunk_at,omitempty"` // RFC3339
}

// handleJobsJSON returns live stats for all in-flight jobs visible to the caller.
func (a *Admin) handleJobsJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u := ctxGetUser(r)
	recs := a.hub.AllInFlightJobs()
	out := make([]jobStatJSON, 0, len(recs))
	for _, rec := range recs {
		if u.Role != "admin" && rec.Req.Owner != u.Username && rec.ClientOwner != u.Username {
			continue
		}
		stat := jobStatJSON{
			ID:         rec.Req.ID,
			Phase:      "processing",
			DeltaCount: rec.DeltaCount(),
		}
		if fc := rec.FirstChunkAt(); fc != nil {
			stat.Phase = "generating"
			stat.TTFTMs = fc.Sub(rec.DispatchedAt).Milliseconds()
			stat.FirstChunkAtISO = fc.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		out = append(out, stat)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}

// ─── Usage API ────────────────────────────────────────────────────────────────

type usageSeriesJSON struct {
	Name             string  `json:"name"`
	Requests         []int64 `json:"requests"`
	PromptTokens     []int64 `json:"prompt_tokens"`
	CompletionTokens []int64 `json:"completion_tokens"`
	TotalRequests    int64   `json:"total_requests"`
	TotalTokens      int64   `json:"total_tokens"`
}

type usageResponseJSON struct {
	Range   string            `json:"range"`
	Group   string            `json:"group"`
	Buckets []string          `json:"buckets"`
	Series  []usageSeriesJSON `json:"series"`
	Totals  struct {
		Requests         int64 `json:"requests"`
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"totals"`
}

// usageWindow converts a range name into a dense bucket grid.
func usageWindow(rng string, now time.Time) (since, until time.Time, buckets []string, daily bool, ok bool) {
	now = now.UTC()
	switch rng {
	case "24h", "7d":
		hours := 24
		if rng == "7d" {
			hours = 7 * 24
		}
		until = now.Truncate(time.Hour).Add(time.Hour)
		since = until.Add(-time.Duration(hours) * time.Hour)
		for t := since; t.Before(until); t = t.Add(time.Hour) {
			buckets = append(buckets, t.Format(time.RFC3339))
		}
		return since, until, buckets, false, true
	case "30d", "90d":
		days := 30
		if rng == "90d" {
			days = 90
		}
		until = now.Truncate(24 * time.Hour).Add(24 * time.Hour)
		since = until.AddDate(0, 0, -days)
		for t := since; t.Before(until); t = t.AddDate(0, 0, 1) {
			buckets = append(buckets, t.Format("2006-01-02"))
		}
		return since, until, buckets, true, true
	}
	return time.Time{}, time.Time{}, nil, false, false
}

// maxUsageSeries caps how many named series are returned; the remainder is
// folded into an "other" series so charts stay readable.
const maxUsageSeries = 10

// handleUsageJSON returns time-series usage for the dashboard. Members see
// only their own usage; admins see everything.
func (a *Admin) handleUsageJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u := ctxGetUser(r)

	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "7d"
	}
	group := r.URL.Query().Get("group")
	if group == "" {
		group = "model"
	}
	groupBy := group
	if group == "user" {
		groupBy = "owner"
	}
	since, until, buckets, daily, ok := usageWindow(rng, time.Now())
	if !ok {
		http.Error(w, "invalid range", http.StatusBadRequest)
		return
	}
	ownerFilter := ""
	if u.Role != "admin" {
		ownerFilter = u.Username
	}
	rows, err := a.state.QueryUsage(since, until, groupBy, daily, ownerFilter)
	if err != nil {
		a.log.Error("admin: usage query", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	bucketIdx := make(map[string]int, len(buckets))
	for i, b := range buckets {
		bucketIdx[b] = i
	}
	seriesMap := make(map[string]*usageSeriesJSON)
	var order []string
	resp := usageResponseJSON{Range: rng, Group: group, Buckets: buckets}
	for _, row := range rows {
		i, ok := bucketIdx[row.Bucket]
		if !ok {
			continue
		}
		s, ok := seriesMap[row.Name]
		if !ok {
			s = &usageSeriesJSON{
				Name:             row.Name,
				Requests:         make([]int64, len(buckets)),
				PromptTokens:     make([]int64, len(buckets)),
				CompletionTokens: make([]int64, len(buckets)),
			}
			seriesMap[row.Name] = s
			order = append(order, row.Name)
		}
		s.Requests[i] += row.Requests
		s.PromptTokens[i] += row.PromptTokens
		s.CompletionTokens[i] += row.CompletionTokens
		s.TotalRequests += row.Requests
		s.TotalTokens += row.PromptTokens + row.CompletionTokens
		resp.Totals.Requests += row.Requests
		resp.Totals.PromptTokens += row.PromptTokens
		resp.Totals.CompletionTokens += row.CompletionTokens
	}

	all := make([]usageSeriesJSON, 0, len(order))
	for _, name := range order {
		all = append(all, *seriesMap[name])
	}
	sort.Slice(all, func(i, j int) bool { return all[i].TotalTokens > all[j].TotalTokens })
	if len(all) > maxUsageSeries {
		other := usageSeriesJSON{
			Name:             "other",
			Requests:         make([]int64, len(buckets)),
			PromptTokens:     make([]int64, len(buckets)),
			CompletionTokens: make([]int64, len(buckets)),
		}
		for _, s := range all[maxUsageSeries:] {
			for i := range buckets {
				other.Requests[i] += s.Requests[i]
				other.PromptTokens[i] += s.PromptTokens[i]
				other.CompletionTokens[i] += s.CompletionTokens[i]
			}
			other.TotalRequests += s.TotalRequests
			other.TotalTokens += s.TotalTokens
		}
		all = append(all[:maxUsageSeries], other)
	}
	resp.Series = all
	if resp.Series == nil {
		resp.Series = []usageSeriesJSON{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
}

func (a *Admin) handleAuditLogJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 200
	if ls := r.URL.Query().Get("limit"); ls != "" {
		if n, err := strconv.Atoi(ls); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	entries, err := a.state.GetAuditLog(limit)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []AuditEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(entries)
}
