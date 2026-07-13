package main

import (
	"context"
	"expvar"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	shimPkg "llmesh/shim"
	"llmesh/shim/internal/stats"
	"llmesh/shim/internal/ws"
)

// version is set at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

var log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

func main() {
	configPath := flag.String("config", "/config.yaml", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: llmesh-shim [options]\n\nOptions:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Config file fields (YAML):
  router_url      wss:// URL of the llmesh router         (required)
  router_token    client token from the admin UI           (required)
  max_concurrent  parallel jobs limit                      (default: 4)
  models:
    - name:       model identifier (e.g. gpt-4o)
      context_size: 128000                                 (optional)
      backend:
        type:     "http" or "command"
        url:      base URL for type=http
        format:   "openai" or "anthropic" for type=http
        auth_type:  "bearer", "header", or "none"
        auth_value: token (${VAR} expanded from environment)
        command:  shell command for type=command (${VAR} expanded)
`)
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("llmesh-shim %s\n", version)
		os.Exit(0)
	}

	cfg, err := shimPkg.LoadConfig(*configPath)
	if err != nil {
		log.Error("config", "error", err)
		os.Exit(1)
	}

	log.Info("llmesh-shim starting", "router", cfg.RouterURL, "models", len(cfg.Models), "max_concurrent", cfg.MaxConcurrent)

	st := stats.New()

	if cfg.MetricsAddr != "" {
		stats.Register(st)
		mux := http.NewServeMux()
		mux.Handle("/debug/vars", expvar.Handler())
		srv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux}
		go func() {
			log.Info("metrics listening", "addr", cfg.MetricsAddr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Error("metrics server error", "error", err)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	if isTerminal(os.Stderr) {
		go runStatusLine(ctx, st, cfg.MaxConcurrent)
	}

	conn := ws.New(cfg, version, st)
	conn.Run(ctx)
	log.Info("llmesh-shim: shut down")
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func runStatusLine(ctx context.Context, st *stats.Stats, maxConcurrent int) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			fmt.Fprint(os.Stderr, statusLine(st, maxConcurrent))
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr)
			return
		}
	}
}

func statusLine(st *stats.Stats, maxConcurrent int) string {
	connSym := "○"
	if st.Connected() {
		connSym = "●"
	}
	uptime := time.Since(st.StartTime).Round(time.Second)
	return fmt.Sprintf("\033[2K\r[llmesh-shim] %s | jobs %d/%d | done %d | err %d | tok %d | up %s",
		connSym,
		st.ActiveJobs.Load(), maxConcurrent,
		st.TotalDone.Load(),
		st.TotalErrors.Load(),
		st.TotalTokens.Load(),
		uptime,
	)
}
