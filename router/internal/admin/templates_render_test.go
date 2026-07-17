package admin

import (
	"fmt"
	"html/template"
	"io"
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
		"not": func(b bool) bool { return !b },
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

// base returns the layout-level fields every page needs.
func base(page string) map[string]any {
	return map[string]any{
		"Page": page, "Name": "llmesh", "Username": "alice", "IsAdmin": true,
		"CSRFToken": "csrf", "RouterVersion": "v1.2.3", "Host": "llm.example.com",
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
			"Priority": "high", "CreatedAt": now,
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
			"Models": "llama3", "InFlight": 1, "MaxConcurrent": 4, "Jobs": []any{job},
		}
		row := map[string]any{
			"Name": "macbook", "TokenHash": "aabbcc", "TokenPrefix": "ct-alice-1a2b…", "StatusClass": "connected",
			"StatusLabel": "● connected", "LastSeen": "", "IsRouter": false,
			"CSRFToken":   "csrf",
			"Connections": []any{conn},
			"ModelSlots":  []any{map[string]any{"Name": "llama3", "OwnerSlots": 2}},
		}
		routerRow := map[string]any{
			"Name": "downstream", "TokenHash": "ddeeff", "TokenPrefix": "ct-alice-3c4d…", "StatusClass": "connected",
			"StatusLabel": "● connected", "IsRouter": true, "CSRFToken": "csrf",
		}
		d := base("clients")
		d["NewToken"] = "ct-alice-xyz"
		d["Users"] = []any{"alice"}
		d["Groups"] = []any{map[string]any{
			"Username": "alice", "HasLive": true, "Tokens": []any{row, routerRow},
		}}
		// also exercise the non-admin flat view
		dFlat := base("clients")
		dFlat["IsAdmin"] = false
		dFlat["Tokens"] = []any{row, routerRow}
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

	t.Run("settings", func(t *testing.T) {
		d := base("settings")
		d["Users"] = []any{
			map[string]any{"Username": "alice", "IsSelf": true, "Role": "admin", "Disabled": false},
			map[string]any{"Username": "bob", "IsSelf": false, "Role": "member", "Disabled": false},
			map[string]any{"Username": "carol", "IsSelf": false, "Role": "admin", "Disabled": true},
		}
		d["Upstreams"] = []any{map[string]any{
			"Name": "orch", "URL": "https://orch.example.com", "Priority": "high", "Connected": true,
		}}
		renderPage(t, "settings", d)
	})

	t.Run("help", func(t *testing.T) {
		renderPage(t, "help", base("help"))
	})
}
