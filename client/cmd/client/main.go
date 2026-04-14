package main

import (
	"flag"
	"log"

	clientPkg "llmesh/client"
	"llmesh/client/internal/ws"
)

func main() {
	configPath := flag.String("config", "/config.yaml", "path to config file")
	flag.Parse()

	cfg, err := clientPkg.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.RouterURL == "" {
		log.Fatal("config: router_url must not be empty")
	}
	if cfg.RouterToken == "" {
		log.Fatal("config: router_token must not be empty")
	}

	log.Printf("llm-client starting, router=%s models=%d max_concurrent=%d",
		cfg.RouterURL, len(cfg.Models), cfg.MaxConcurrent)

	conn := ws.New(cfg)
	conn.Run() // blocks forever, reconnects on disconnect
}
