package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"llmesh/router/internal/logring"
)

type statRowJSON struct {
	Name             string `json:"name"`
	Requests         int64  `json:"requests"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
}

type dashboardJSON struct {
	TotalRequests  int64             `json:"total_requests"`
	ActiveClients  int               `json:"active_clients"`
	APIKeyCount    int               `json:"api_key_count"`
	TokenCount     int               `json:"token_count"`
	ActiveModels   []string            `json:"active_models"`
	ActiveAliases  map[string][]string `json:"active_aliases"`
	Clients        []clientJSON      `json:"clients"`
	StatsByModel   []statRowJSON     `json:"stats_by_model,omitempty"`
	StatsByUser    []statRowJSON     `json:"stats_by_user,omitempty"`
}

type clientJSON struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	LastSeen string `json:"last_seen,omitempty"`
	Models   string `json:"models,omitempty"`
	Version  string `json:"version,omitempty"`
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
		connCount := a.hub.ConnectedCountByToken(t.Token)
		if connCount > 0 {
			if connCount == 1 {
				c.Status = "connected"
			} else {
				c.Status = fmt.Sprintf("%d connected", connCount)
			}
			mods := a.hub.ConnectedModels(t.Token)
			sort.Strings(mods)
			c.Models = strings.Join(mods, ", ")
			c.Version = a.hub.ConnectedVersion(t.Token)
		} else if ls := a.hub.LastSeenTime(t.Token); !ls.IsZero() {
			c.Status = "offline"
			c.LastSeen = humanTime(ls)
		} else {
			c.Status = "never_connected"
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
	ID             string `json:"id"`
	Phase          string `json:"phase"`            // "processing" | "generating"
	DeltaCount     int64  `json:"delta_count"`      // tokens generated so far
	TTFTMs         int64  `json:"ttft_ms,omitempty"` // time-to-first-token in ms
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
