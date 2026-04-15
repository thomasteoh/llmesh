package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"llmesh/pkg/types"
	routerPkg "llmesh/router"
	"llmesh/router/internal/admin"
	"llmesh/router/internal/api"
	"llmesh/router/internal/correlation"
	"llmesh/router/internal/hub"
	"llmesh/router/internal/queue"
	"llmesh/router/internal/scheduler"
)

func main() {
	configPath := flag.String("config", "/config.yaml", "path to config file")
	statePath := flag.String("state", "/state.json", "path to state.json")
	flag.Parse()

	cfg, err := routerPkg.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
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

	// Wire reqCount after apiHandler is created using a closure that captures the pointer.
	var apiHandler *api.Handler

	adminHandler, err := admin.New(*statePath, h, func() int64 {
		if apiHandler == nil {
			return 0
		}
		return apiHandler.Count()
	})
	if err != nil {
		log.Fatalf("admin: %v", err)
	}

	apiHandler = &api.Handler{
		Keys:        adminHandler.State(),
		Queue:       q,
		Correlation: store,
		Scheduler:   sched,
	}

	mux := http.NewServeMux()
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
		h.ServeWS(w, r, ct.Name, ct.Owner, token)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"status":"ok"}`)
	})
	mux.Handle("/admin/", adminHandler)
	mux.Handle("/admin", adminHandler)

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	log.Printf("llm-router listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
