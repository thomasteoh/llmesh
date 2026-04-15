package admin

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

type dashboardJSON struct {
	TotalRequests int64        `json:"total_requests"`
	ActiveClients int          `json:"active_clients"`
	APIKeyCount   int          `json:"api_key_count"`
	TokenCount    int          `json:"token_count"`
	Clients       []clientJSON `json:"clients"`
}

type clientJSON struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	LastSeen string `json:"last_seen,omitempty"`
	Models   string `json:"models,omitempty"`
}

func (a *Admin) handleDashboardJSON(w http.ResponseWriter, r *http.Request) {
	tokens := a.state.ClientTokensFor("", true)
	clients := make([]clientJSON, 0, len(tokens))
	for _, t := range tokens {
		c := clientJSON{Name: t.Owner + "/" + t.Name}
		if a.hub.IsConnected(t.Token) {
			c.Status = "connected"
			mods := a.hub.ConnectedModels(t.Token)
			sort.Strings(mods)
			c.Models = strings.Join(mods, ", ")
		} else if ls := a.hub.LastSeenTime(t.Token); !ls.IsZero() {
			c.Status = "offline"
			c.LastSeen = humanTime(ls)
		} else {
			c.Status = "never_connected"
		}
		clients = append(clients, c)
	}
	resp := dashboardJSON{
		TotalRequests: a.reqCount(),
		ActiveClients: a.hub.ActiveClientCount(),
		APIKeyCount:   a.state.APIKeyCount(),
		TokenCount:    a.state.ClientTokenCount(),
		Clients:       clients,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
