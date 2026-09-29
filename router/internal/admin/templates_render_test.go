package admin

import (
	"fmt"
	"html/template"
	"io"
	"llmesh/router/internal/authz"
	"strings"
	"testing"
	"time"
)

// testFuncMap mirrors the funcMap in parseTemplates so the render test exercises
// the same template features (notably the dict helper used by partials).
func testFuncMap() template.FuncMap {
	return template.FuncMap{
		"asset": func(name string) string { return "/portal/static/" + name + "?v=test" },
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n]
		},
		"not":       func(b bool) bool { return !b },
		"list":      func(items ...string) []string { return items },
		"hasPrefix": strings.HasPrefix,
		"dict": func(pairs ...any) (map[string]any, error) {
			if len(pairs)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of arguments")
			}
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i < len(pairs); i += 2 {
				k, ok := pairs[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key %d not a string", i)
				}
				m[k] = pairs[i+1]
			}
			return m, nil
		},
	}
}

func renderPage(t *testing.T, page string, data any) {
	t.Helper()
	tmpl, err := template.New("layout.html").Funcs(testFuncMap()).ParseFS(
		adminFS, "templates/layout.html", "templates/partials.html", "templates/"+page+".html",
	)
	if err != nil {
		t.Fatalf("parse %s: %v", page, err)
	}
	if err := tmpl.Execute(io.Discard, data); err != nil {
		t.Fatalf("execute %s: %v", page, err)
	}
}

// renderStandalonePage executes a page that has no layout (the auth pages).
func renderStandalonePage(t *testing.T, page string, data any) {
	t.Helper()
	// Mirrors parseTemplates: the standalone pages get partials.html too.
	tmpl, err := template.New(page+".html").Funcs(testFuncMap()).ParseFS(
		adminFS, "templates/partials.html", "templates/"+page+".html")
	if err != nil {
		t.Fatalf("parse %s: %v", page, err)
	}
	if err := tmpl.Execute(io.Discard, data); err != nil {
		t.Fatalf("execute %s: %v", page, err)
	}
}

// base returns the layout-level fields every page needs.
func base(page string) map[string]any {
	return map[string]any{
		"Page": page, "Name": "llmesh", "Username": "alice", "Can": allCaps(true), "RoleBadge": "admin",
		"CanManageAliases": true,
		"CSRFToken":        "csrf", "RouterVersion": "v1.2.3", "Host": "llm.example.com",
		"Flash": "", "Error": "",
	}
}

// TestTemplatesRender executes each portal page with representative data so the
// shared partials (action-button, status-badge, endpoint-display), which only
// run inside row loops, are exercised at execute time — parse success alone does
// not catch dict arity bugs or fields a partial expects but a row omits.
func TestTemplatesRender(t *testing.T) {
	now := time.Now()

	t.Run("api-keys", func(t *testing.T) {
		d := base("api-keys")
		d["NewKey"] = "sk-alice-abc123"
		d["Users"] = []any{"alice", "bob"}
		d["Keys"] = []any{map[string]any{
			"Owner": "alice", "Label": "prod", "KeyHash": "deadbeef", "KeyPrefix": "sk-alice-1a2b…",
			"Priority": "high", "CreatedAt": now, "ExpiresAt": time.Time{}, "LastUsedAt": now, "Scope": KeyScope{},
		}, map[string]any{
			"Owner": "bob", "Label": "ci", "KeyHash": "cafe", "KeyPrefix": "sk-bob-9f8e…",
			"Priority": "normal", "CreatedAt": now, "ExpiresAt": now.Add(24 * time.Hour), "LastUsedAt": time.Time{},
			"Scope": KeyScope{Models: []string{"qwen3-*"}, Endpoints: []string{"/v1/messages"}},
		}}
		renderPage(t, "api-keys", d)
	})

	t.Run("clients", func(t *testing.T) {
		job := map[string]any{
			"ID": "job-1", "Model": "llama3", "Priority": "high", "Attempts": 2,
			"APIKeyLabel": "prod", "Owner": "alice", "CanCancel": true,
			"DispatchedAtISO": now.Format(time.RFC3339), "EnqueuedAt": "1m ago",
			"FirstChunkAtISO": now.Format(time.RFC3339), "WordCount": 12,
			"StatsStr": " · 12 tok", "Phase": "generating",
		}
		conn := map[string]any{
			"Name": "gpu-box", "Version": "v1.2.3", "IsRouter": false,
			"InFlight": 1, "MaxConcurrent": 4, "Jobs": []any{job},
		}
		// Performance carries pre-formatted strings; an empty one must render as an
		// em-dash rather than a blank cell, so leave PromptTPS and MaxTotal unset.
		perf := &ClientPerfRow{
			Requests: 42, GenTPS: "38.4 tok/s", PromptTPS: "",
			AvgTTFT: "412 ms", MaxTTFT: "9.1 s", AvgTotal: "8.3 s", MaxTotal: "",
			AvgQueue: "18 ms", Est: true, WindowDesc: "24h",
		}
		row := map[string]any{
			"Name": "macbook", "TokenHash": "aabbcc", "TokenPrefix": "ct-alice-1a2b…", "StatusClass": "connected",
			"StatusLabel": "● connected", "LastSeen": "", "IsRouter": false,
			"CSRFToken":   "csrf",
			"Connections": []any{conn},
			"Models": []any{
				map[string]any{"Name": "llama3", "Live": true, "OwnerSlots": 2,
					"Requests": 40, "GenTPS": "38.5 tok/s", "PromptTPS": "1.2k tok/s", "AvgTTFT": "410 ms"},
				map[string]any{"Name": "qwen", "Live": false, "OwnerSlots": 0, "Requests": 0},
			},
			"Perf": perf,
		}
		// A single-request machine exercises the plural-suffix branch, and a machine
		// with no traffic exercises the nil-Perf path (routerRow, below).
		singleRow := map[string]any{
			"Name": "mini", "TokenHash": "112233", "TokenPrefix": "ct-alice-9z8y…",
			"StatusClass": "connected", "StatusLabel": "● connected", "CSRFToken": "csrf",
			"Perf": &ClientPerfRow{Requests: 1, GenTPS: "12.0 tok/s", WindowDesc: "24h"},
		}
		routerRow := map[string]any{
			"Name": "downstream", "TokenHash": "ddeeff", "TokenPrefix": "ct-alice-3c4d…", "StatusClass": "connected",
			"StatusLabel": "● connected", "IsRouter": true, "CSRFToken": "csrf",
		}
		d := base("clients")
		d["NewToken"] = "ct-alice-xyz"
		d["Users"] = []any{"alice"}
		d["Groups"] = []any{map[string]any{
			"Username": "alice", "HasLive": true, "Tokens": []any{row, singleRow, routerRow},
		}}
		// also exercise the non-admin flat view
		dFlat := base("clients")
		dFlat["Can"] = allCaps(false)
		dFlat["Tokens"] = []any{row, singleRow, routerRow}
		renderPage(t, "clients", d)
		renderPage(t, "clients", dFlat)
	})

	t.Run("dashboard", func(t *testing.T) {
		d := base("dashboard")
		d["TotalRequests"] = 10
		d["ActiveClients"] = 1
		d["APIKeyCount"] = 2
		d["TokenCount"] = 3
		d["ActiveModels"] = []any{"llama3"}
		d["ModelAliases"] = map[string]any{"llama3": []any{"small"}}
		// A chain covering every decorated state: a tied pair (shared tier, one
		// reachable and one not) plus a lone fallback at the end.
		d["AliasChains"] = []any{map[string]any{
			"Alias": "chat",
			"Targets": []any{
				map[string]any{"Model": "llama3", "Tier": 0, "Live": true, "Shared": true, "CanUp": false, "CanDown": true},
				map[string]any{"Model": "mistral", "Tier": 0, "Live": false, "Shared": true, "CanUp": true, "CanDown": true},
				map[string]any{"Model": "gpt-4o", "Tier": 1, "Live": true, "Shared": false, "CanUp": true, "CanDown": false},
			},
		}}
		d["StatsByModel"] = []any{map[string]any{"Name": "llama3", "Requests": 5, "PromptTokens": 100, "CompletionTokens": 50}}
		d["StatsByUser"] = []any{map[string]any{"Name": "alice", "Requests": 5, "PromptTokens": 100, "CompletionTokens": 50}}
		d["Clients"] = []any{map[string]any{
			"Name": "alice/macbook", "StatusClass": "connected", "StatusLabel": "● connected",
			"LastSeen": "", "Models": "llama3", "Version": "v1.2.3",
		}}
		d["QueueLen"] = 1
		d["QueueItems"] = []any{map[string]any{
			"Owner": "alice", "APIKeyLabel": "prod", "Model": "llama3", "Priority": "high",
			"WordCount": 8, "EnqueuedAtISO": now.Format(time.RFC3339), "EnqueuedAt": "now",
			"CanCancel": true, "ID": "req-1",
		}}
		renderPage(t, "dashboard", d)
	})

	t.Run("teams", func(t *testing.T) {
		d := base("teams")
		d["CanCreate"] = true
		d["Users"] = []string{"alice", "bob"}
		d["Teams"] = []any{
			map[string]any{"ID": "research", "Name": "Research", "Description": "ML", "Disabled": false, "ManagedBy": "",
				"CanManage": true, "IsMember": true, "CanControl": true, "Members": []any{
					map[string]any{"Username": "alice", "Maintainer": true},
					map[string]any{"Username": "bob", "Maintainer": false},
				}},
			map[string]any{"ID": "ops", "Name": "Ops", "Description": "", "Disabled": true, "ManagedBy": "oidc",
				"CanManage": false, "IsMember": false, "Members": []any{}},
		}
		renderPage(t, "teams", d)
		d["Teams"] = []any{}
		d["CanCreate"] = false
		renderPage(t, "teams", d)
	})

	t.Run("settings", func(t *testing.T) {
		d := base("settings")
		d["Users"] = []any{
			map[string]any{"Username": "alice", "IsSelf": true, "Role": "admin", "Disabled": false, "ManagedBy": "", "Roles": []string{"owner"}},
			map[string]any{"Username": "bob", "IsSelf": false, "Role": "member", "Disabled": false, "ManagedBy": "", "Roles": []string{"member", "billing"}},
			map[string]any{"Username": "carol", "IsSelf": false, "Role": "admin", "Disabled": true, "ManagedBy": "", "Roles": []string{"admin"}},
			map[string]any{"Username": "dave", "IsSelf": false, "Role": "member", "Disabled": false, "ManagedBy": "oidc", "Roles": []string{}},
		}
		d["Roles"] = []any{
			map[string]any{"ID": "owner", "Name": "Owner", "Description": "Everything", "Builtin": true, "TeamRole": false, "Permissions": []string{"audit.view"}},
			map[string]any{"ID": "team-member", "Name": "Team member", "Description": "", "Builtin": true, "TeamRole": true, "Permissions": []string{}},
			map[string]any{"ID": "billing", "Name": "Billing", "Description": "Pricing", "Builtin": false, "TeamRole": false, "Permissions": []string{"pricing.manage", "usage.view.any"}},
		}
		d["PermissionGroups"] = permissionGroups()
		d["MySessions"] = []any{
			map[string]any{"IDHash": "h1", "IP": "10.0.0.1", "UserAgent": "Firefox", "CreatedAt": now, "LastSeenAt": now},
			map[string]any{"IDHash": "h2", "IP": "10.0.0.2", "UserAgent": "", "CreatedAt": now, "LastSeenAt": now},
		}
		d["CurrentSession"] = "h1"
		d["MyTeams"] = []string{"research"}
		d["Policies"] = []any{map[string]any{"ID": "p1", "Name": "P1", "Effect": "deny", "Actions": "model.use",
			"Summary": "who: everyone", "Enabled": true, "JSON": `{"id":"p1"}`}}
		d["PolicyTemplate"] = policyTemplate
		d["Actions"] = []string{"model.use", "client.use"}
		d["SimForm"] = SimForm{SubjectKind: "user", Subject: "alice", Action: "model.use", ResType: "model"}
		d["Sim"] = &SimResult{Current: authz.Decision{By: "no-gpt", Reason: "denied"},
			WithDraft: &authz.Decision{Allowed: true, By: "draft"}}
		d["Denials"] = []Denial{{At: now, Subject: "user:bob", Action: "model.use", Resource: "gpt-4o", Reason: "denied by policy"}}
		d["ModelRules"] = []any{
			map[string]any{"ID": "models-default", "Name": "Everyone", "Effect": "allow", "Models": []string{"*"}, "Who": "everyone", "Enabled": true, "Advanced": false},
			map[string]any{"ID": "no-gpt", "Name": "No GPT", "Effect": "deny", "Models": []string{"gpt-*"}, "Who": "teams interns", "Enabled": false, "Advanced": true},
		}
		d["Currency"] = "AUD"
		d["Auth"] = AuthSettings{
			Providers: []OAuthProviderSettings{{
				Key: "github", Name: "GitHub", Enabled: true, ClientID: "iv1.abc",
				HasSecret: true, Configured: true, Scope: "read:user",
				CallbackURL: "https://llm.example.com/portal/auth/github/callback",
				Linked:      true, Label: "octocat",
			}, {
				Key: "google", Name: "Google", Enabled: true, ClientID: "goog.apps",
				HasSecret: true, Configured: true, Scope: "openid email",
				CallbackURL: "https://llm.example.com/portal/auth/google/callback",
			}, {
				Key: "oidc", Name: "Zitadel", Enabled: true, ClientID: "123@llmesh",
				HasSecret: true, Configured: true, Scope: "openid email profile",
				CallbackURL: "https://llm.example.com/portal/auth/oidc/callback",
				OIDC:        true, OIDCIssuer: "https://acme.zitadel.cloud", OIDCName: "Zitadel",
				OIDCAuthMethod: "client_secret_basic", OIDCDiscovered: true,
				OIDCRolesClaim: "urn:zitadel:iam:org:project:roles",
				OIDCProvision:  true,
			}},
			SMTPEnabled: true, SMTPHost: "smtp.example.com", SMTPPort: 587,
			SMTPUsername: "llmesh", SMTPHasPassword: true, SMTPFrom: "llmesh@example.com",
			SMTPSecurity: "starttls", SMTPConfigured: true,
			Email: "alice@example.com", EmailVerified: true,
		}
		// Covers each pricing state: charged and live, estimated and live,
		// and a configured rate whose model no longer has a worker.
		d["Pricing"] = []any{
			map[string]any{"Model": "gpt-4o", "InputRate": "2.5", "OutputRate": "10",
				"Basis": "actual", "IsActual": true, "Configured": true, "Live": true},
			map[string]any{"Model": "qwen3-30b", "InputRate": "", "OutputRate": "",
				"Basis": "estimated", "IsActual": false, "Configured": false, "Live": true},
			map[string]any{"Model": "retired", "InputRate": "0.01", "OutputRate": "0.02",
				"Basis": "estimated", "IsActual": false, "Configured": true, "Live": false},
		}
		renderPage(t, "settings", d)
		d["Sim"] = &SimResult{Replay: &ReplayResult{Window: "24h0m0s", Requests: 10, NewlyDenied: 4,
			Changes: []ReplayChange{{Owner: "bob", Model: "gpt-4o", Requests: 4, Before: true, By: "draft"}}}}
		renderPage(t, "settings", d)
		d["Sim"] = &SimResult{Error: "bad draft"}
		renderPage(t, "settings", d)
	})

	t.Run("help", func(t *testing.T) {
		renderPage(t, "help", base("help"))
	})
}

// allCaps returns a capability map with every flag the templates read set to
// on, as an owner sees it, or off, as a viewer does.
func allCaps(on bool) map[string]bool {
	out := map[string]bool{}
	for _, a := range capabilityActions {
		out[strings.ReplaceAll(a, ".", "_")] = on
	}
	for _, k := range []string{"key_limits_any", "key_create_any", "client_create_any", "job_cancel_any", "policy_simulate"} {
		out[k] = on
	}
	return out
}
