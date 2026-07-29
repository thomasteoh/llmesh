package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"llmesh/pkg/types"
	routerPkg "llmesh/router"
	"llmesh/router/internal/admin"
	"llmesh/router/internal/api"
	"llmesh/router/internal/correlation"
	"llmesh/router/internal/dedup"
	"llmesh/router/internal/hub"
	"llmesh/router/internal/latency"
	"llmesh/router/internal/logring"
	"llmesh/router/internal/queue"
	"llmesh/router/internal/scheduler"
	"llmesh/router/internal/stats"
	"llmesh/router/internal/upstream"
)

// version is set at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

var sink = logring.New(logring.DefaultCap)

var log = logring.NewLogger(sink, "router", slog.LevelInfo)

//go:embed web/landing.js
var landingJS string

var landingTmpl = template.Must(template.New("landing").Parse(landingHTML))

const landingHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Name}}</title>
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
:root{
  --bg:#0a0f1e;--surface:#0f1729;--border:#1b2a44;
  --text:#c8d3e8;--muted:#4a6280;
  --accent:#7c86c8;--hi:#9ba7e8;
  --blue:#64a0ff;--green:#4ec89a;
  --mono:ui-monospace,'Cascadia Code','Fira Code',monospace;
  --sans:system-ui,-apple-system,sans-serif
}
html,body{background:var(--bg);color:var(--text);font-family:var(--sans);min-height:100vh}
header{
  display:flex;justify-content:space-between;align-items:center;
  padding:18px 40px;border-bottom:1px solid var(--border)
}
.brand{font:600 15px/1 var(--mono);color:var(--hi);letter-spacing:.04em}
nav{display:flex;gap:20px}
nav a{font:13px var(--mono);color:var(--muted);text-decoration:none;letter-spacing:.04em;transition:color .12s}
nav a:hover{color:var(--text)}
.hero{padding:52px 40px 32px;max-width:860px;margin:0 auto}
.hero h1{font:400 clamp(22px,3.8vw,36px)/1.2 var(--sans);letter-spacing:-.015em}
.meta{margin-top:14px;font:13px var(--mono);color:var(--muted);display:flex;align-items:center;flex-wrap:wrap;gap:6px}
.meta .count{color:var(--hi);font-weight:600}
.meta .sep{color:var(--border)}
.canvas-wrap{padding:0 40px 24px;max-width:860px;margin:0 auto}
#route-canvas{
  display:block;width:100%;height:240px;
  background:var(--surface);border:1px solid var(--border);border-radius:8px
}
.caption{text-align:center;font:11px var(--mono);color:var(--muted);letter-spacing:.05em;margin-top:8px}
.features{
  max-width:860px;margin:0 auto;padding:0 40px 52px;
  display:grid;grid-template-columns:repeat(3,1fr);gap:14px
}
@media(max-width:660px){
  header{padding:16px 20px}
  .hero,.canvas-wrap,.features{padding-left:20px;padding-right:20px}
  .features{grid-template-columns:1fr}
}
.card{
  background:var(--surface);border:1px solid var(--border);border-radius:8px;
  padding:20px;transition:border-color .15s
}
.card:hover{border-color:#2a3e5e}
.clabel{font:11px/1 var(--mono);letter-spacing:.09em;text-transform:uppercase;color:var(--accent);margin-bottom:16px}
.card p{font:13px/1.65 var(--sans);color:var(--muted);margin-top:14px}
.queue{display:flex;flex-direction:column;gap:9px}
.lane{display:flex;align-items:center;gap:9px;font:11px var(--mono)}
.ltag{width:40px;text-align:right;color:var(--muted)}
.ltag.hi{color:var(--blue)}.ltag.md{color:var(--accent)}
.btrack{flex:1;height:6px;background:var(--border);border-radius:3px;overflow:hidden}
.bfill{height:100%;border-radius:3px}
.bfill.hi{background:var(--blue);width:88%;animation:bp 1.1s ease-in-out infinite}
.bfill.md{background:var(--accent);width:58%;animation:bp 1.9s ease-in-out infinite .3s}
.bfill.lo{background:#223;width:32%;animation:bp 3.2s ease-in-out infinite .7s}
@keyframes bp{0%,100%{opacity:.5}50%{opacity:1}}
.lct{font:10px var(--mono);color:var(--muted);width:20px}
.cb{
  background:#080e1c;border:1px solid var(--border);border-radius:5px;
  padding:12px 14px;font:12px/1.75 var(--mono);color:var(--text);
  overflow-x:auto;white-space:pre
}
.hl{color:var(--hi)}.hg{color:var(--green)}.hb{color:var(--blue)}.dm{color:var(--muted)}
footer{
  border-top:1px solid var(--border);padding:18px 40px;
  display:flex;align-items:center;flex-wrap:wrap;gap:12px;
  font:12px var(--mono);color:var(--muted)
}
footer a{color:var(--accent);text-decoration:none}
footer a:hover{color:var(--hi)}
footer .sep{color:var(--border)}
</style>
</head>
<body>
<header>
  <span class="brand">{{.Name}}</span>
  <nav>
    <a href="/portal">portal</a>
    <a href="/health">health</a>
    <a href="/v1/models">models</a>
  </nav>
</header>
<main>
  <section class="hero">
    <h1>distributed local<br>inference router</h1>
    <p class="meta">
      <span class="count">{{.Count}}</span>&nbsp;requests served
      <span class="sep">·</span>
      <code>https://{{.Host}}/v1</code>
    </p>
  </section>
  <section class="canvas-wrap">
    <canvas id="route-canvas"></canvas>
    <p class="caption">live simulation — requests routed across local AI workers</p>
  </section>
  <section class="features">
    <div class="card">
      <div class="clabel">priority queuing</div>
      <div class="queue">
        <div class="lane">
          <span class="ltag hi">HIGH</span>
          <div class="btrack"><div class="bfill hi"></div></div>
          <span class="lct">8</span>
        </div>
        <div class="lane">
          <span class="ltag md">NORM</span>
          <div class="btrack"><div class="bfill md"></div></div>
          <span class="lct">5</span>
        </div>
        <div class="lane">
          <span class="ltag">LOW</span>
          <div class="btrack"><div class="bfill lo"></div></div>
          <span class="lct">3</span>
        </div>
      </div>
      <p>Three priority tiers with FIFO within each lane. High-priority jobs are always dispatched first, with owner-affinity scheduling.</p>
    </div>
    <div class="card">
      <div class="clabel">model aliases</div>
      <pre class="cb"><span class="dm"># one name, many workers</span>
<span class="hl">"qwen"</span>  <span class="dm">&#x2192;</span> <span class="hg">qwen3-4b-instruct</span>
        <span class="dm">&#x2192;</span> <span class="hg">qwen3-14b-instruct</span>

<span class="dm"># owner affinity: prefers</span>
<span class="dm"># the requester's own GPU</span></pre>
      <p>Map one name across multiple models or machines. The scheduler picks the best available worker by affinity and load.</p>
    </div>
    <div class="card">
      <div class="clabel">openai compatible</div>
      <pre class="cb"><span class="hb">POST</span> /v1/chat/completions
<span class="dm">Authorization: Bearer sk-&#x2026;</span>

<span class="hl">{</span>
  <span class="hg">"model"</span><span class="dm">:</span> <span class="dm">"llama-3.2"</span><span class="dm">,</span>
  <span class="hg">"stream"</span><span class="dm">:</span> <span class="hb">true</span>
<span class="hl">}</span></pre>
      <p>Drop-in for the OpenAI API. Works with Claude Code, Open WebUI, and any client that speaks the OpenAI protocol.</p>
    </div>
  </section>
</main>
<footer>
  <a href="/portal">portal</a>
  <span class="sep">·</span>
  <a href="/health">health</a>
  <span class="sep">·</span>
  <a href="/v1/models">models</a>
  <span class="sep">·</span>
  <span>{{.Version}}</span>
</footer>
<script>{{.JS}}</script>
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
	statePath := flag.String("state", "/state.json", "path to the state database (SQLite); a legacy state.json at this path is migrated automatically")
	flag.Parse()

	cfg, err := routerPkg.LoadConfig(*configPath)
	if err != nil {
		log.Error("config", "error", err)
		os.Exit(1)
	}

	timeouts := cfg.ResolvedTimeouts()
	hub.LeaseDuration = timeouts.Lease

	q := queue.New()
	q.MaxDepth = timeouts.QueueMaxDepth
	store := correlation.New(logring.NewLogger(sink, "correlation", slog.LevelInfo))
	h := hub.New(logring.NewLogger(sink, "hub", slog.LevelInfo))
	h.Latency = latency.New()
	reqStats := stats.New()
	api.SetLogger(logring.NewLogger(sink, "api", slog.LevelInfo))

	h.OnChunk = func(msg types.ChunkMsg) {
		switch store.Send(msg) {
		case correlation.SendOK:
			// delivered
		case correlation.SendNotFound:
			if !msg.Done {
				log.Debug("chunk dropped: no handler registered (request timed out or cancelled)",
					"request_id", msg.RequestID)
			}
		case correlation.SendFull:
			// Handler is not consuming fast enough — cancel the job to avoid
			// silently truncating the response stream.
			log.Warn("correlation: handler backpressure, cancelling request",
				"request_id", msg.RequestID)
			h.CancelRequest(msg.RequestID)
		}
	}
	h.OnError = func(msg types.ErrorMsg) {
		log.Error("client error for request", "request_id", msg.RequestID, "message", msg.Message)
		if result := store.Send(types.ChunkMsg{
			Type:         "chunk",
			RequestID:    msg.RequestID,
			Done:         true,
			FinishReason: "error",
		}); result == correlation.SendNotFound {
			log.Debug("error done-chunk dropped, handler already gone", "request_id", msg.RequestID)
		}
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
	adminHandler.SetTrustProxy(cfg.Server.TrustProxyHeaders)

	sched := scheduler.New(q, h, adminHandler.State(), logring.NewLogger(sink, "scheduler", slog.LevelInfo))
	sched.SetOptProvider(adminHandler.State())
	sched.SetIsolationProvider(adminHandler.State())
	sched.Start()
	// Wire hub callbacks that wake the scheduler (moved here from scheduler.New since
	// scheduler now accepts a Dispatcher interface rather than *hub.Hub directly).
	h.OnAvailable = func() { sched.Wake() }
	h.OnRelease = func(req types.InferenceRequest) { q.Push(req); sched.Wake() }
	h.StartLeaseReaper()

	// Upstream connector: connects this router to orchestrator routers as a client.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := upstream.New(h, q, store, sched, version, logring.NewLogger(sink, "upstream", slog.LevelInfo))
	if upstreams := adminHandler.State().GetUpstreamRouters(); len(upstreams) > 0 {
		conn.Reload(ctx, upstreams)
	}
	adminHandler.SetUpstreamReloader(func() { conn.Reload(ctx, adminHandler.State().GetUpstreamRouters()) })
	adminHandler.SetConnectorStatus(conn.Connected)

	// Persistent time-series usage tracking, flushed to the state DB.
	usageRec := admin.NewUsageRecorder(adminHandler.State(), logring.NewLogger(sink, "admin", slog.LevelInfo))

	apiHandler = &api.Handler{
		Keys:              adminHandler.State(),
		Models:            h,
		Aliases:           adminHandler.State(),
		Opts:              adminHandler.State(),
		Stats:             reqStats,
		Usage:             usageRec,
		Queue:             q,
		Correlation:       store,
		Scheduler:         sched,
		Canceller:         h,
		Workers:           h,
		ContextSizes:      h,
		Modalities:        h,
		InFlight:          h,
		Limits:            adminHandler.State(),
		Dedup:             dedup.New(),
		MaxRequestBytes:   cfg.MaxRequestBytes(),
		TTFTTimeout:       timeouts.TTFT,
		ActivityTimeout:   timeouts.Activity,
		BatchTimeout:      timeouts.Batch,
		KeepAliveInterval: timeouts.KeepAlive,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data := struct {
			Count, Name, Host, Version string
			JS                         template.JS
		}{
			Count:   fmtCount(apiHandler.Count()),
			Name:    cfg.Name,
			Host:    cfg.Host,
			Version: version,
			JS:      template.JS(landingJS),
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := landingTmpl.Execute(w, data); err != nil {
			log.Error("landing", "error", err)
		}
	})
	// Dynamic manifest for client auto-update: returns the current version and
	// the platform-specific binary download URL for that version.
	// Path: /downloads/manifest/<GOOS>/<GOARCH>
	mux.HandleFunc("/downloads/manifest/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.TrimPrefix(r.URL.Path, "/downloads/manifest/")
		segs := strings.SplitN(parts, "/", 2)
		if len(segs) != 2 || segs[0] == "" || segs[1] == "" {
			http.NotFound(w, r)
			return
		}
		goos, goarch := segs[0], segs[1]
		if strings.ContainsAny(goos+goarch, "/\\.") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		scheme := "https"
		if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
			scheme = "http"
		}
		baseURL := scheme + "://" + r.Host
		binaryName := fmt.Sprintf("llmesh-client-%s-%s", goos, goarch)
		m := map[string]string{
			"version": version,
			"url":     baseURL + "/downloads/" + binaryName,
		}
		// Include SHA-256 if a sidecar file exists alongside the binary.
		// The file may contain just the hex digest or sha256sum(1) format (<hash>  <name>).
		if raw, err := os.ReadFile("/downloads/" + binaryName + ".sha256"); err == nil {
			hash := strings.TrimSpace(strings.Fields(string(raw))[0])
			if len(hash) == 64 {
				m["sha256"] = hash
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m)
	})
	mux.Handle("/downloads/", http.StripPrefix("/downloads/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", "attachment")
		http.FileServer(http.Dir("/downloads")).ServeHTTP(w, r)
	})))
	mux.HandleFunc("/v1/models", apiHandler.ModelList())
	mux.HandleFunc("/v1/models/slots", apiHandler.ModelSlots())
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
		// The hub is keyed by the token hash so plaintext secrets never sit in
		// the connection registry or in-flight job records.
		h.ServeWS(w, r, ct.Name, ct.Owner, ct.TokenHash, ct.OwnerSlots)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		upstreamRouters := adminHandler.State().GetUpstreamRouters()
		type upstreamStatus struct {
			URL       string `json:"url"`
			Name      string `json:"name,omitempty"`
			Connected bool   `json:"connected"`
		}
		upstreams := make([]upstreamStatus, len(upstreamRouters))
		for i, u := range upstreamRouters {
			upstreams[i] = upstreamStatus{
				URL:       u.URL,
				Name:      u.Name,
				Connected: conn.Connected(u.URL),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":      "ok",
			"version":     version,
			"clients":     h.ActiveClientCount(),
			"queue_depth": q.Len(),
			"active_jobs": len(h.AllInFlightJobs()),
			"upstreams":   upstreams,
		})
	})
	mux.HandleFunc("/metrics", metricsHandler(apiHandler, q, h, reqStats, h.Latency))
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
	log.Info("llmesh-router listening on", "version", version, "addr", addr)
	srv := &http.Server{
		Addr:    addr,
		Handler: secureHeaders(mux),
		// Bound the time to read request headers so a slowloris client cannot
		// hold a connection (and goroutine + fd) open indefinitely. WriteTimeout
		// and IdleTimeout stay 0 because SSE streams are long-lived by design.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       0,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-sigCh
		log.Info("shutting down...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()

		// Stop dispatch so no new jobs are sent to workers.
		sched.Stop()

		// Drain queued (not-yet-dispatched) requests and send terminal error chunks
		// so HTTP handlers unblock immediately rather than waiting for the TTFT timeout.
		if drained := q.Drain(); len(drained) > 0 {
			log.Info("shutdown: draining queued requests", "count", len(drained))
			for _, req := range drained {
				store.Send(types.ChunkMsg{
					Type:         "chunk",
					RequestID:    req.ID,
					Done:         true,
					FinishReason: "error",
				})
			}
		}

		// Drain in-flight requests (already dispatched to workers). This sends a
		// terminal error chunk to every waiting SSE handler so they close their
		// streams immediately rather than blocking srv.Shutdown until the 30s timeout.
		if n := store.DrainAll(); n > 0 {
			log.Info("shutdown: terminating active SSE streams", "count", n)
		}

		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error("shutdown error", "error", err)
		}

		// Flush buffered usage counters after in-flight requests have settled.
		usageRec.Close()
	}()

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Error("server", "error", err)
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
