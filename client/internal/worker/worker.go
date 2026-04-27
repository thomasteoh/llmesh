package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	clientPkg "llmesh/client"
	"llmesh/client/internal/llamacpp"
	"llmesh/pkg/types"
)

var log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// keepAliveInterval is how often to send an empty chunk to the router to
// prevent its TTFT and activity timers from firing during long prompt evaluation.
// Must be shorter than the router's 2-minute activityTimer.
const keepAliveInterval = 60 * time.Second

// SendFn sends a JSON-encodable message back to the router over WebSocket.
type SendFn func(msg any) error

// Handle processes a single job from the router.
// ctx is cancelled when the WS connection drops (so in-flight llama.cpp requests abort).
func Handle(ctx context.Context, job types.JobMsg, cfg *clientPkg.Config, send SendFn) {
	req := job.Request
	endpoint := cfg.EndpointFor(req.Model)
	if endpoint == "" {
		log.Warn("worker: no endpoint", "model", req.Model)
		_ = send(types.ErrorMsg{
			Type:      "error",
			RequestID: req.ID,
			Message:   "model not available on this client",
		})
		return
	}

	// Send an empty chunk to the router every minute so its TTFT and activity
	// timers don't fire during long prompt evaluation. The router ignores
	// chunks with empty deltas when writing the HTTP response.
	keepAliveDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(keepAliveInterval)
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

	llmClient := llamacpp.New(endpoint)
	chatTemplate := cfg.ChatTemplateFor(req.Model)
	err := llmClient.Infer(ctx, req, chatTemplate, func(delta string, toolCallsDelta json.RawMessage, done bool, finishReason string, usage *types.UsageInfo) {
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
	if err != nil {
		if ctx.Err() == nil {
			log.Error("worker: infer error", "request_id", req.ID, "error", err)
		}
		_ = send(types.ErrorMsg{
			Type:      "error",
			RequestID: req.ID,
			Message:   err.Error(),
		})
	}
}
