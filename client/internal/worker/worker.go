package worker

import (
	"context"
	"log"

	clientPkg "llmesh/client"
	"llmesh/client/internal/llamacpp"
	"llmesh/pkg/types"
)

// SendFn sends a JSON-encodable message back to the router over WebSocket.
type SendFn func(msg any) error

// Handle processes a single job from the router.
// ctx is cancelled when the WS connection drops (so in-flight llama.cpp requests abort).
func Handle(ctx context.Context, job types.JobMsg, cfg *clientPkg.Config, send SendFn) {
	req := job.Request
	endpoint := cfg.EndpointFor(req.Model)
	if endpoint == "" {
		log.Printf("worker: no endpoint for model %s", req.Model)
		_ = send(types.ErrorMsg{
			Type:      "error",
			RequestID: req.ID,
			Message:   "model not available on this client",
		})
		return
	}

	llmClient := llamacpp.New(endpoint)
	err := llmClient.Infer(ctx, req, func(delta string, done bool, finishReason string) {
		chunk := types.ChunkMsg{
			Type:         "chunk",
			RequestID:    req.ID,
			Delta:        delta,
			Done:         done,
			FinishReason: finishReason,
		}
		if sendErr := send(chunk); sendErr != nil {
			log.Printf("worker: send error: %v", sendErr)
		}
	})
	if err != nil {
		log.Printf("worker: infer error for %s: %v", req.ID, err)
		_ = send(types.ErrorMsg{
			Type:      "error",
			RequestID: req.ID,
			Message:   err.Error(),
		})
	}
}
