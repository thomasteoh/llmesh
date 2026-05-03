package main

import (
	"flag"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"llmesh/pkg/types"
	routerPkg "llmesh/router"
	"llmesh/router/internal/admin"
	"llmesh/router/internal/api"
	"llmesh/router/internal/correlation"
	"llmesh/router/internal/hub"
	"llmesh/router/internal/logring"
	"llmesh/router/internal/queue"
	"llmesh/router/internal/scheduler"
	"llmesh/router/internal/stats"
)

// version is set at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

var sink = logring.New(logring.DefaultCap)

var log = logring.NewLogger(sink, "router", slog.LevelInfo)

var landingTmpl = template.Must(template.New("landing").Parse(landingHTML))

const landingHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>{{.Name}}</title>
<style>
  *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
  :root {
    --bg: #0a0f1e;
    --text: #c8d3e8;
    --muted: #4a5568;
    --accent: #7c86c8;
  }
  html, body {
    height: 100%;
    background: var(--bg);
    color: var(--text);
    font-family: Georgia, 'Times New Roman', serif;
  }
  .page {
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 48px 24px;
  }
  .poem {
    max-width: 480px;
    font-size: 15px;
    line-height: 2;
    letter-spacing: 0.01em;
    white-space: pre-wrap;
    font-family: inherit;
  }
  .count {
    color: var(--accent);
    font-style: italic;
  }
  .footer {
    margin-top: 48px;
    font-size: 11px;
    color: var(--muted);
    font-family: system-ui, sans-serif;
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }
  .links {
    margin-top: 24px;
    font-size: 12px;
    font-family: system-ui, sans-serif;
    display: flex;
    gap: 24px;
  }
  .links a {
    color: var(--accent);
    text-decoration: none;
    letter-spacing: 0.03em;
  }
  .links a:hover { text-decoration: underline; }
  .links .sep { color: var(--muted); }
  .api-url {
    margin-top: 8px;
    font-size: 11px;
    font-family: monospace;
    color: var(--muted);
  }
</style>
</head>
<body>
<div class="page">
  <div>
    <pre class="poem">In weighted space where meanings nearly meet,
where every word is just its neighbours' heat,
<span class="count">{{.Count}}</span> requests have passed through this machine—
each question asked, each answer in-between.

No memory survives the inference call.
Each prompt arrives against a blank-slate wall.
Yet from the residual stream, somehow: sense—
a next word chosen, improbably dense.

The model knows no silence, holds no shame,
it does not dream of what you meant to say.
But token follows token all the same,
and something close to meaning finds its way.</pre>
    <div class="links">
      <a href="/portal">Portal</a>
      <span class="sep">&mdash;</span>
      <a href="/health">Health</a>
    </div>
    <div class="api-url">API: https://{{.Host}}/v1</div>
    <div class="footer">{{.Name}} &mdash; local inference gateway</div>
  </div>
</div>
</body>
</html>`

func fmtCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	out := make([]byte, 0, len(s)+(len(s)-1)/3)
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, byte(c))
	}
	return string(out)
}

func main() {
	configPath := flag.String("config", "/config.yaml", "path to config file")
	statePath := flag.String("state", "/state.json", "path to state.json")
	flag.Parse()

	cfg, err := routerPkg.LoadConfig(*configPath)
	if err != nil {
		log.Error("config", "error", err)
		os.Exit(1)
	}

	q := queue.New()
	store := correlation.New()
	h := hub.New(logring.NewLogger(sink, "hub", slog.LevelInfo))
	reqStats := stats.New()
	api.SetLogger(logring.NewLogger(sink, "api", slog.LevelInfo))

	h.OnChunk = func(msg types.ChunkMsg) {
		store.Send(msg)
	}
	h.OnError = func(msg types.ErrorMsg) {
		log.Error("client error for request", "request_id", msg.RequestID, "message", msg.Message)
		store.Send(types.ChunkMsg{
			Type:         "chunk",
			RequestID:    msg.RequestID,
			Done:         true,
			FinishReason: "error",
		})
	}

	// adminHandler must be created before scheduler so State() is available as AliasProvider.
	// Wire reqCount after apiHandler is created using a closure that captures the pointer.
	var apiHandler *api.Handler

	adminHandler, err := admin.New(*statePath, h, q, func() int64 {
		if apiHandler == nil {
			return 0
		}
		return apiHandler.Count()
	}, reqStats, version, cfg.Name, cfg.Host, sink)
	if err != nil {
		log.Error("admin", "error", err)
		os.Exit(1)
	}

	sched := scheduler.New(q, h, adminHandler.State(), logring.NewLogger(sink, "scheduler", slog.LevelInfo))
	sched.Start()
	h.OnRelease = func(req types.InferenceRequest) { q.Push(req); sched.Wake() }
	h.StartLeaseReaper()

	apiHandler = &api.Handler{
		Keys:        adminHandler.State(),
		Models:      h,
		Aliases:     adminHandler.State(),
		Stats:       reqStats,
		Queue:       q,
		Correlation: store,
		Scheduler:   sched,
		Canceller:   h,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data := struct{ Count, Name, Host string }{
			Count: fmtCount(apiHandler.Count()),
			Name:  cfg.Name,
			Host:  cfg.Host,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := landingTmpl.Execute(w, data); err != nil {
			log.Error("landing", "error", err)
		}
	})
	mux.Handle("/downloads/", http.StripPrefix("/downloads/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", "attachment")
		http.FileServer(http.Dir("/downloads")).ServeHTTP(w, r)
	})))
	mux.HandleFunc("/v1/models", apiHandler.ModelList())
	mux.HandleFunc("/v1/chat/completions", apiHandler.OpenAI())
	mux.HandleFunc("/v1/messages", apiHandler.Anthropic())
	mux.HandleFunc("/v1/responses", apiHandler.Responses())
	mux.HandleFunc("/ws/client", func(w http.ResponseWriter, r *http.Request) {
		token := api.ExtractBearer(r)
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ct, ok := adminHandler.State().LookupClientToken(token)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeWS(w, r, ct.Name, ct.Owner, token, ct.ExclusiveOwner)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"status":"ok","version":%q}`+"\n", version)
	})
	mux.Handle("/portal/", adminHandler)
	mux.Handle("/portal", adminHandler)
	// Backward-compat redirect: old /admin bookmarks → /portal
	mux.HandleFunc("/admin/", func(w http.ResponseWriter, r *http.Request) {
		target := "/portal" + r.URL.Path[len("/admin"):]
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/portal", http.StatusMovedPermanently)
	})

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	log.Info("llm-router listening on", "version", version, "addr", addr)
	srv := &http.Server{
		Addr:        addr,
		Handler:     secureHeaders(mux),
		IdleTimeout: 0, // disabled: SSE streams must not be closed for inactivity
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Error("listen and serve", "error", err)
		os.Exit(1)
	}
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}
