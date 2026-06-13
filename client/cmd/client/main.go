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
	"runtime"
	"strings"
	"syscall"
	"time"

	clientPkg "llmesh/client"
	"llmesh/client/internal/stats"
	"llmesh/client/internal/updater"
	"llmesh/client/internal/ws"
)

// version is set at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

var log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

func main() {
	configPath := flag.String("config", "/config.yaml", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: llmesh-client [options]\n\nOptions:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Config file fields (YAML):
  router_url      wss:// URL of the llmesh router  (required)
  router_token    client token from the admin UI   (required)
  max_concurrent  parallel jobs limit              (default: auto from llama.cpp total_slots, min 1)
  auto_update     enable hourly self-update checks (default: false)
  models:
    - endpoint:   llama.cpp base URL (e.g. http://localhost:8080)  (required)
      name:       model identifier (e.g. llama3.2:3b)
                  optional — auto-detected from the endpoint's /v1/models if omitted
`)
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("llmesh-client %s\n", version)
		os.Exit(0)
	}

	cfg, err := clientPkg.LoadConfig(*configPath)
	if err != nil {
		log.Error("config", "error", err)
		os.Exit(1)
	}
	if cfg.RouterURL == "" {
		log.Error("config: router_url must not be empty")
		os.Exit(1)
	}
	if cfg.RouterToken == "" {
		log.Error("config: router_token must not be empty")
		os.Exit(1)
	}

	log.Info("llmesh-client starting", "router", cfg.RouterURL, "models", len(cfg.Models), "max_concurrent", cfg.MaxConcurrent)

	st := stats.New()
	stats.Register(st)

	if cfg.MetricsAddr != "" {
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

	if manifestURL := deriveManifestURL(cfg.RouterURL); manifestURL != "" {
		triggerCh := make(chan struct{}, 1)
		go updater.Run(ctx, manifestURL, version, cfg.AutoUpdate, func() bool {
			return st.ActiveJobs.Load() == 0
		}, triggerCh, log)
		conn.SetOnUpdate(func() {
			select {
			case triggerCh <- struct{}{}:
			default:
			}
		})
	}

	conn.Run(ctx) // blocks until ctx cancelled, reconnects on disconnect
	log.Info("llmesh-client: shut down")
}

// isTerminal reports whether f is an interactive terminal.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// runStatusLine writes a live one-line status to stderr every second until ctx is done.
func runStatusLine(ctx context.Context, st *stats.Stats, maxConcurrent int) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			fmt.Fprint(os.Stderr, statusLine(st, maxConcurrent))
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr) // leave terminal on a clean line
			return
		}
	}
}

func statusLine(st *stats.Stats, maxConcurrent int) string {
	connSym := "○"
	if st.Connected() {
		connSym = "●"
	}
	return fmt.Sprintf("\033[2K\r[llmesh-client] %s | jobs %d/%d | done %d | err %d | tok %d | up %s",
		connSym,
		st.ActiveJobs.Load(), maxConcurrent,
		st.TotalDone.Load(),
		st.TotalErrors.Load(),
		st.TotalTokens.Load(),
		formatUptime(time.Since(st.StartTime)),
	)
}

// formatUptime returns a compact human-readable duration, dropping lower-order
// units once a higher-order unit is reached:
//
//	< 1 min  → "42s"
//	< 1 hour → "5m"   (seconds dropped)
//	< 1 day  → "2h"   (minutes dropped)
//	≥ 1 day  → "3d"   (hours dropped)
func formatUptime(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// deriveManifestURL converts a wss:// or ws:// router URL to the platform-specific
// update manifest URL served by that router. Returns "" on unrecognised scheme.
func deriveManifestURL(routerURL string) string {
	var scheme, rest string
	switch {
	case strings.HasPrefix(routerURL, "wss://"):
		scheme, rest = "https", strings.TrimPrefix(routerURL, "wss://")
	case strings.HasPrefix(routerURL, "ws://"):
		scheme, rest = "http", strings.TrimPrefix(routerURL, "ws://")
	default:
		return ""
	}
	if idx := strings.Index(rest, "/"); idx >= 0 {
		rest = rest[:idx]
	}
	return fmt.Sprintf("%s://%s/downloads/manifest/%s/%s", scheme, rest, runtime.GOOS, runtime.GOARCH)
}
