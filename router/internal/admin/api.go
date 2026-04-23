package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
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
