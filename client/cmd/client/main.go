package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	clientPkg "llmesh/client"
	"llmesh/client/internal/ws"
)

// version is set at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

var log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

func main() {
	configPath := flag.String("config", "/config.yaml", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: llm-client [options]\n\nOptions:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Config file fields (YAML):
  router_url      wss:// URL of the llmesh router  (required)
  router_token    client token from the admin UI   (required)
  max_concurrent  parallel jobs limit              (default: 4)
  models:
    - name:       model identifier (e.g. llama3.2:3b)
      endpoint:   llama.cpp base URL (e.g. http://localhost:8080)
`)
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("llm-client %s\n", version)
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

	log.Info("llm-client starting", "router", cfg.RouterURL, "models", len(cfg.Models), "max_concurrent", cfg.MaxConcurrent)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	conn := ws.New(cfg, version)
	conn.Run(ctx) // blocks until ctx cancelled, reconnects on disconnect
	log.Info("llm-client: shut down")
}
