package worker

import (
	"context"
	"log/slog"
	"os"
	"time"

	"llmesh/pkg/types"
	"llmesh/shim/internal/backend"
	"llmesh/shim/internal/stats"
)

var log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// keepAliveInterval is how often to send an empty chunk to the router to reset its
// TTFT and activity timers during long upstream calls. Must be shorter than the
// router's configured activity_timeout (default 5 min).
const keepAliveInterval = 60 * time.Second

// Handle processes a single job from the router. Returns non-nil only when inference
// itself failed (not on ctx cancellation — that is not an error from the shim's perspective).
func Handle(ctx context.Context, job types.JobMsg, spec *backend.Spec, send func(any) error, st *stats.Stats) error {
	req := &job.Request

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

	if req.Stream {
		return handleStream(ctx, req, spec, send, st)
	}
	return handleBatch(ctx, req, spec, send, st)
}

func handleBatch(ctx context.Context, req *types.InferenceRequest, spec *backend.Spec, send func(any) error, st *stats.Stats) error {
	res, err := backend.RunBatch(ctx, spec, req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		log.Error("worker: batch error", "request_id", req.ID, "error", err)
		_ = send(types.ErrorMsg{Type: "error", RequestID: req.ID, Message: err.Error()})
		return err
	}
	_ = send(types.ChunkMsg{
		Type:           "chunk",
		RequestID:      req.ID,
		Delta:          res.Content,
		ReasoningDelta: res.Reasoning,
		ToolCallsDelta: res.ToolCalls,
		Done:           true,
		FinishReason:   res.FinishReason,
	})
	return nil
}

func handleStream(ctx context.Context, req *types.InferenceRequest, spec *backend.Spec, send func(any) error, st *stats.Stats) error {
	firstToken := true
	err := backend.RunStream(ctx, spec, req, func(c backend.Chunk) {
		if firstToken && c.Delta != "" {
			firstToken = false
		}
		if c.Usage != nil {
			st.TotalTokens.Add(int64(c.Usage.CompletionTokens))
		}
		chunk := types.ChunkMsg{
			Type:           "chunk",
			RequestID:      req.ID,
			Delta:          c.Delta,
			ReasoningDelta: c.Reasoning,
			ToolCallsDelta: c.ToolCalls,
			Done:           c.Done,
			FinishReason:   c.FinishReason,
			Usage:          c.Usage,
		}
		if sendErr := send(chunk); sendErr != nil {
			if ctx.Err() == nil {
				log.Error("worker: send error", "request_id", req.ID, "error", sendErr)
			}
		}
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		log.Error("worker: stream error", "request_id", req.ID, "error", err)
		_ = send(types.ErrorMsg{Type: "error", RequestID: req.ID, Message: err.Error()})
		return err
	}
	return nil
}
