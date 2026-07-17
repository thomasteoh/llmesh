package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	clientPkg "llmesh/client"
	"llmesh/client/internal/llamacpp"
	"llmesh/client/internal/stats"
	"llmesh/pkg/types"
)

var log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// SendFn sends a JSON-encodable message back to the router over WebSocket.
type SendFn func(msg any) error

// Handle processes a single job from the router. Returns a non-nil error only
// when inference itself failed (not when ctx was cancelled).
// ctx is cancelled when the WS connection drops (so in-flight llama.cpp requests abort).
func Handle(ctx context.Context, job types.JobMsg, cfg *clientPkg.Config, send SendFn, st *stats.Stats) error {
	req := job.Request
	endpoint := cfg.EndpointFor(req.Model)
	if endpoint == "" {
		log.Warn("worker: no endpoint", "model", req.Model)
		_ = send(types.ErrorMsg{
			Type:      "error",
			RequestID: req.ID,
			Message:   "model not available on this client",
		})
		return nil
	}

	// Send an empty chunk to the router at a regular interval so its TTFT and
	// activity timers don't fire during long prompt evaluation. The interval is
	// derived from the router's configured activity timeout (half the timeout,
	// capped at 60s). The router ignores chunks with empty deltas.
	keepAliveDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(cfg.KeepAliveInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = send(types.ChunkMsg{Type: "chunk", RequestID: req.ID})
			case <-keepAliveDone:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	defer close(keepAliveDone)

	llmClient := llamacpp.New(endpoint, cfg.HeadersFor(req.Model))
	chatTemplate := cfg.ChatTemplateFor(req.Model)
	err := llmClient.Infer(ctx, req, chatTemplate, func(delta string, toolCallsDelta json.RawMessage, done bool, finishReason string, usage *types.UsageInfo) {
		if done && usage != nil {
			st.TotalTokens.Add(int64(usage.CompletionTokens))
		}
		chunk := types.ChunkMsg{
			Type:           "chunk",
			RequestID:      req.ID,
			Delta:          delta,
			ToolCallsDelta: toolCallsDelta,
			Done:           done,
			FinishReason:   finishReason,
			Usage:          usage,
		}
		if sendErr := send(chunk); sendErr != nil {
			if ctx.Err() == nil {
				log.Error("worker: send error", "error", sendErr)
			}
		}
	})
	if err != nil && ctx.Err() == nil {
		log.Error("worker: infer error", "request_id", req.ID, "error", err)
		_ = send(types.ErrorMsg{
			Type:      "error",
			RequestID: req.ID,
			Message:   err.Error(),
		})
		return err
	}
	return nil
}
