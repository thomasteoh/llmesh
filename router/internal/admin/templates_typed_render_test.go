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
		Page: "clients", Username: "alice", Can: allCaps(true), RoleBadge: "admin", CSRFToken: "csrf",
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
		memberBase.Can, memberBase.RoleBadge = allCaps(false), ""
		renderPage(t, "clients", ClientTokensPage{basePage: memberBase, Tokens: tokens})
	})

	// The settings page grew a section per sign-in method, each rendered only
	// in some states. Every combination is executed here, since the template is
	// the only place several of these fields are read.
	t.Run("settings", func(t *testing.T) {
		sb := bp
		sb.Page = "settings"
		users := []UserRow{
			{User: User{Username: "alice", Role: "admin"}, IsSelf: true},
			{User: User{Username: "bob", Role: "member", Disabled: true}},
		}
		base := SettingsPage{
			basePage:  sb,
			Users:     users,
			Upstreams: []UpstreamRouterRow{{UpstreamRouter: UpstreamRouter{Name: "orch", URL: "https://orch.example.com", Priority: "high"}, Connected: true}},
			Currency:  "AUD",
			Pricing:   []ModelPricingRow{{Model: "llama3", InputRate: "1", OutputRate: "2", Basis: "estimated", Live: true, Configured: true}},
		}

		// One provider card per real provider, so adding a provider puts it
		// through this page's every state without anyone remembering to.
		configuredProviders := func(linked, hasSecret bool) []OAuthProviderSettings {
			out := make([]OAuthProviderSettings, 0, len(oauthProviderOrder))
			for _, key := range oauthProviderOrder {
				p := oauthProviders[key]
				label := ""
				if linked {
					label = "someone@" + key
				}
				out = append(out, OAuthProviderSettings{
					Key: key, Name: p.name, Enabled: true, ClientID: "cid",
					HasSecret: hasSecret, Configured: true,
					CallbackURL: "https://llm.example.com/portal/auth/" + key + "/callback",
					Scope:       p.scope, ConsoleHint: oauthConsoleHints[key],
					Linked: linked, Label: label,
				})
			}
			return out
		}
		unconfiguredProviders := func() []OAuthProviderSettings {
			out := make([]OAuthProviderSettings, 0, len(oauthProviderOrder))
			for _, key := range oauthProviderOrder {
				p := oauthProviders[key]
				out = append(out, OAuthProviderSettings{
					Key: key, Name: p.name, Scope: p.scope,
					CallbackURL: "https://llm.example.com/portal/auth/" + key + "/callback",
					ConsoleHint: oauthConsoleHints[key],
				})
			}
			return out
		}

		fullyConfigured := AuthSettings{
			Providers:   configuredProviders(true, true),
			SMTPEnabled: true, SMTPHost: "smtp.example.com", SMTPPort: 587,
			SMTPUsername: "llmesh", SMTPHasPassword: true, SMTPFrom: "llmesh@example.com",
			SMTPSecurity: "starttls", SMTPConfigured: true,
			Email: "alice@example.com", EmailVerified: true,
		}
		// Configured but with nothing linked and an address still unverified,
		// which is what a user sees between claiming one and confirming it.
		pending := fullyConfigured
		pending.Providers = configuredProviders(false, false)
		pending.EmailVerified = false
		pending.SMTPHasPassword = false
		// Nothing configured at all: the page must fall back to the password
		// card alone and never reach a sign-in-method field.
		off := AuthSettings{Providers: unconfiguredProviders(), SMTPSecurity: "starttls"}
		// And a router with no providers compiled in at all, which is what the
		// page sees if the provider list is ever empty.
		none := AuthSettings{SMTPSecurity: "starttls"}

		for name, auth := range map[string]AuthSettings{
			"configured": fullyConfigured, "pending": pending, "off": off, "none": none,
		} {
			page := base
			page.Auth = auth
			t.Run(name, func(t *testing.T) { renderPage(t, "settings", page) })

			member := page
			member.Can, member.RoleBadge = allCaps(false), ""
			t.Run(name+"/member", func(t *testing.T) { renderPage(t, "settings", member) })
		}
	})

	t.Run("login", func(t *testing.T) {
		all := make([]loginProvider, 0, len(oauthProviderOrder))
		for _, key := range oauthProviderOrder {
			all = append(all, loginProvider{Key: key, Name: oauthProviders[key].name, Path: oauthStartPath(key)})
		}
		pages := []loginPage{
			{},
			{Error: "Invalid credentials."},
			{Notice: "Check your mail.", Providers: all, EmailEnabled: true, EmailSubmitted: "a@b.com"},
			{Providers: all},
			{EmailEnabled: true},
			// A provider with no mark of its own must still render.
			{Providers: []loginProvider{{Key: "nonesuch", Name: "Nonesuch", Path: "/portal/auth/nonesuch"}}},
		}
		// And each provider alone, since only one button is drawn at a time in
		// the common case.
		for _, p := range all {
			pages = append(pages, loginPage{Providers: []loginProvider{p}})
		}
		for _, p := range pages {
			renderStandalonePage(t, "login", p)
		}
	})

	t.Run("magic-confirm", func(t *testing.T) {
		renderStandalonePage(t, "magic-confirm", magicConfirmPage{Token: "tok"})
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
