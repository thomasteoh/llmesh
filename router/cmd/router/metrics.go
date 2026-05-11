package main

import (
	"fmt"
	"net/http"
	"strings"

	"llmesh/router/internal/api"
	"llmesh/router/internal/hub"
	"llmesh/router/internal/queue"
	"llmesh/router/internal/stats"
)

// metricsHandler returns a Prometheus text-format handler at /metrics.
// It exposes router-level counters and gauges with no authentication so that
// Prometheus scrapers can reach it without credentials. Mount it on the main
// mux so no additional port needs to be exposed in the container.
func metricsHandler(ah *api.Handler, q *queue.Queue, h *hub.Hub, s *stats.Stats) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		var b strings.Builder

		// --- Scalar gauges/counters ---
		writeGauge(&b, "llmrouter_requests_total",
			"Total inference requests served since process start",
			float64(ah.Count()))

		writeGauge(&b, "llmrouter_queue_depth",
			"Number of requests currently waiting in the queue",
			float64(q.Len()))

		writeGauge(&b, "llmrouter_clients_connected",
			"Number of llmesh-client instances currently connected",
			float64(h.ActiveClientCount()))

		writeGauge(&b, "llmrouter_slots_total",
			"Total concurrent inference slots across all connected clients",
			float64(h.TotalSlots()))

		writeGauge(&b, "llmrouter_jobs_inflight",
			"Number of inference jobs currently dispatched to clients",
			float64(len(h.AllInFlightJobs())))

		// --- Per-model counters ---
		fmt.Fprintf(&b, "# HELP llmrouter_model_requests_total Total requests per model since process start.\n")
		fmt.Fprintf(&b, "# TYPE llmrouter_model_requests_total counter\n")
		fmt.Fprintf(&b, "# HELP llmrouter_model_prompt_tokens_total Total prompt tokens per model since process start.\n")
		fmt.Fprintf(&b, "# TYPE llmrouter_model_prompt_tokens_total counter\n")
		fmt.Fprintf(&b, "# HELP llmrouter_model_completion_tokens_total Total completion tokens per model since process start.\n")
		fmt.Fprintf(&b, "# TYPE llmrouter_model_completion_tokens_total counter\n")
		for _, row := range s.ByModel() {
			fmt.Fprintf(&b, "llmrouter_model_requests_total{model=%q} %d\n", row.Name, row.Requests)
			fmt.Fprintf(&b, "llmrouter_model_prompt_tokens_total{model=%q} %d\n", row.Name, row.PromptTokens)
			fmt.Fprintf(&b, "llmrouter_model_completion_tokens_total{model=%q} %d\n", row.Name, row.CompletionTokens)
		}

		// --- Per-user counters ---
		fmt.Fprintf(&b, "# HELP llmrouter_user_requests_total Total requests per user since process start.\n")
		fmt.Fprintf(&b, "# TYPE llmrouter_user_requests_total counter\n")
		fmt.Fprintf(&b, "# HELP llmrouter_user_prompt_tokens_total Total prompt tokens per user since process start.\n")
		fmt.Fprintf(&b, "# TYPE llmrouter_user_prompt_tokens_total counter\n")
		fmt.Fprintf(&b, "# HELP llmrouter_user_completion_tokens_total Total completion tokens per user since process start.\n")
		fmt.Fprintf(&b, "# TYPE llmrouter_user_completion_tokens_total counter\n")
		for _, row := range s.ByUser() {
			fmt.Fprintf(&b, "llmrouter_user_requests_total{user=%q} %d\n", row.Name, row.Requests)
			fmt.Fprintf(&b, "llmrouter_user_prompt_tokens_total{user=%q} %d\n", row.Name, row.PromptTokens)
			fmt.Fprintf(&b, "llmrouter_user_completion_tokens_total{user=%q} %d\n", row.Name, row.CompletionTokens)
		}

		fmt.Fprint(w, b.String())
	}
}

func writeGauge(b *strings.Builder, name, help string, val float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, val)
}
