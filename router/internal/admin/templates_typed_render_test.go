package admin

import (
	"testing"
	"time"
)

// TestTemplatesRenderAgainstRealStructs executes the two model-heavy pages with
// the page structs the handlers actually pass, rather than the map fixtures
// TestTemplatesRender uses.
//
// The distinction matters: html/template resolves a missing map key to nil and
// renders it, but a missing struct field is an execution error. So a template
// referring to a field that has been renamed or removed passes the map-based
// test and fails only in the browser. These pages carry the most field churn,
// so they are the ones worth pinning to the real types.
func TestTemplatesRenderAgainstRealStructs(t *testing.T) {
	bp := basePage{
		Page: "clients", Username: "alice", IsAdmin: true, CSRFToken: "csrf",
		RouterVersion: "v1.2.3", Name: "llmesh", Host: "llm.example.com",
	}
	now := time.Now()

	t.Run("clients", func(t *testing.T) {
		conn := ConnectedClientRow{
			ID: "conn-1", Name: "gpu-box", Version: "v1.2.3",
			InFlight: 1, MaxConcurrent: 4,
			Jobs: []InFlightJobRow{{
				ID: "job-1", Model: "llama3", Owner: "alice", APIKeyLabel: "prod",
				Priority: "high", Attempts: 2, Phase: "generating", CanCancel: true,
				CSRFToken: "csrf", StatsStr: " · 12 tok",
				DispatchedAtISO: now.Format(time.RFC3339), EnqueuedAt: "1m ago",
			}},
		}
		// Two connections, so the models sub-row renders its "Served by" column.
		conn2 := ConnectedClientRow{
			ID: "conn-2", Name: "gpu-box-2", Version: "v1.2.3",
			InFlight: 0, MaxConcurrent: 2,
		}
		row := ClientTokenRow{
			Status: "connected", StatusClass: "connected", StatusLabel: "● connected",
			CSRFToken:   "csrf",
			Connections: []ConnectedClientRow{conn, conn2},
			Models: []ClientModelRow{
				{Name: "llama3", Live: true, ServedBy: "gpu-box, gpu-box-2", OwnerSlots: 2,
					Requests: 40, GenTPS: "38.5 tok/s", PromptTPS: "1.2k tok/s", AvgTTFT: "410 ms"},
				{Name: "qwen", Live: true, ServedBy: "gpu-box", Requests: 2},
			},
			Perf: &ClientPerfRow{
				Requests: 42, GenTPS: "38.4 tok/s", AvgTTFT: "412 ms", WindowDesc: "24h", Est: true,
			},
		}
		// An offline token whose only model rows come from a slot limit and past
		// traffic exercises the not-served branch and the single-connection
		// layout, where the "Served by" column is suppressed.
		offline := ClientTokenRow{
			Status: "offline", StatusClass: "offline", StatusLabel: "○ offline",
			LastSeen: "3m ago", CSRFToken: "csrf",
			Models: []ClientModelRow{{Name: "retired-model", OwnerSlots: 1, Requests: 3}},
		}
		router := ClientTokenRow{
			Status: "connected", StatusClass: "connected", StatusLabel: "● connected",
			IsRouter: true, CSRFToken: "csrf",
		}
		tokens := []ClientTokenRow{row, offline, router}

		adminPage := ClientTokensPage{
			basePage: bp,
			Groups:   []ClientUserGroup{{Username: "alice", HasLive: true, Tokens: tokens}},
			NewToken: "ct-alice-secret",
			Users:    []string{"alice", "bob"},
		}
		renderPage(t, "clients", adminPage)

		memberBase := bp
		memberBase.IsAdmin = false
		renderPage(t, "clients", ClientTokensPage{basePage: memberBase, Tokens: tokens})
	})

	t.Run("dashboard", func(t *testing.T) {
		db := bp
		db.Page = "dashboard"
		page := DashboardPage{
			basePage:      db,
			TotalRequests: 1234,
			ActiveClients: 2,
			APIKeyCount:   3,
			TokenCount:    4,
			ActiveModels:  []string{"llama3", "qwen"},
			ActiveAliases: map[string][]string{"fast": {"llama3"}},
			ModelAliases:  map[string][]string{"llama3": {"fast"}},
			AliasChains: []AliasChainRow{{
				Alias: "fast",
				Targets: []AliasTargetRow{
					{Model: "llama3", Tier: 0, Live: true, Shared: true, CanDown: true},
					{Model: "qwen", Tier: 1, Live: false, CanUp: true},
				},
			}},
			Clients: []ClientRow{
				{Name: "alice/gpu-box", StatusClass: "connected", StatusLabel: "● connected",
					Models: "llama3, qwen", Version: "v1.2.3"},
				{Name: "alice/mini", StatusClass: "offline", StatusLabel: "○ offline", LastSeen: "3m ago"},
			},
			StatsByModel: []StatRow{{Name: "llama3", Requests: 40, PromptTokens: 100, CompletionTokens: 200}},
			StatsByUser:  []StatRow{{Name: "alice", Requests: 40, PromptTokens: 100, CompletionTokens: 200}},
			QueueLen:     1,
			QueueItems: []QueuedJobRow{{
				ID: "q-1", Model: "llama3", Owner: "alice", APIKeyLabel: "prod",
				Priority: "high", EnqueuedAt: "2s ago", EnqueuedAtISO: now.Format(time.RFC3339),
				WordCount: 12, CanCancel: true,
			}},
		}
		renderPage(t, "dashboard", page)
	})
}
