package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"llmesh/pkg/types"
	routerPkg "llmesh/router"
	"llmesh/router/internal/api"
	"llmesh/router/internal/correlation"
	"llmesh/router/internal/hub"
	"llmesh/router/internal/queue"
	"llmesh/router/internal/scheduler"
)

func main() {
	configPath := flag.String("config", "/config.yaml", "path to config file")
	flag.Parse()

	cfg, err := routerPkg.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.Server.ClientToken == "" {
		log.Fatal("config: server.client_token must not be empty")
	}

	q := queue.New()
	store := correlation.New()
	h := hub.New()

	h.OnChunk = func(msg types.ChunkMsg) {
		store.Send(msg)
	}
	h.OnError = func(msg types.ErrorMsg) {
		log.Printf("client error for request %s: %s", msg.RequestID, msg.Message)
		store.Send(types.ChunkMsg{
			Type:         "chunk",
			RequestID:    msg.RequestID,
			Done:         true,
			FinishReason: "error",
		})
	}

	sched := scheduler.New(q, h)
	sched.Start()

	handler := &api.Handler{
		Config:      cfg,
		Queue:       q,
		Correlation: store,
		Scheduler:   sched,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handler.OpenAI())
	mux.HandleFunc("/v1/messages", handler.Anthropic())
	mux.HandleFunc("/v1/responses", handler.Responses())
	mux.HandleFunc("/ws/client", func(w http.ResponseWriter, r *http.Request) {
		token := api.ExtractBearer(r)
		if token != cfg.Server.ClientToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeWS(w, r)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"status":"ok"}`)
	})

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	log.Printf("llm-router listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
