package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"

	clientPkg "llmesh/client"
	"llmesh/client/internal/llamacpp"
	"llmesh/pkg/types"
)

var log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

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
