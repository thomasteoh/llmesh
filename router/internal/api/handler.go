package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"llmesh/pkg/types"
	routerPkg "llmesh/router"
	"llmesh/router/internal/correlation"
	"llmesh/router/internal/queue"
	"llmesh/router/internal/scheduler"
	"llmesh/router/internal/translate"
)

type Handler struct {
	Config      *routerPkg.Config
	Queue       *queue.Queue
	Correlation *correlation.Store
	Scheduler   *scheduler.Scheduler
}

func (h *Handler) enqueue(
	w http.ResponseWriter,
	r *http.Request,
	inbound func([]byte) (*types.InferenceRequest, error),
) {
	key := ExtractBearer(r)
	if key == "" || !h.validKey(key) {
		unauthorised(w)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		internalError(w)
		return
	}

	req, err := inbound(body)
	if err != nil {
		b, _ := json.Marshal(err.Error())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":{"message":%s}}`+"\n", b)
		return
	}

	req.ID = uuid.New().String()
	req.Priority = h.Config.PriorityFor(key)
	req.EnqueuedAt = time.Now()

	ch := h.Correlation.Create(req.ID)
	defer h.Correlation.Delete(req.ID)

	h.Queue.Push(*req)
	h.Scheduler.Wake()

	if req.Stream {
		h.streamResponse(w, r, req, ch)
	} else {
		h.batchResponse(w, r, req, ch)
	}
}

func (h *Handler) streamResponse(w http.ResponseWriter, r *http.Request, req *types.InferenceRequest, ch <-chan types.ChunkMsg) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		internalError(w)
		return
	}

	timeout := time.NewTimer(60 * time.Second)
	defer timeout.Stop()

	flushed := false

	for {
		select {
		case chunk, open := <-ch:
			if !open {
				return
			}
			switch req.SourceFmt {
			case "anthropic":
				if chunk.Delta != "" {
					fmt.Fprintf(w, "%s\n\n", translate.AnthropicSSEChunk(chunk))
					flushed = true
					flusher.Flush()
				}
				if chunk.Done {
					for _, l := range translate.AnthropicSSEDone(chunk.FinishReason) {
						fmt.Fprintf(w, "%s\n\n", l)
					}
					flushed = true
					flusher.Flush()
					return
				}
			case "openai-responses":
				if chunk.Delta != "" {
					fmt.Fprintf(w, "%s\n\n", translate.OpenAIResponsesSSEChunk(req.ID, chunk))
					flushed = true
					flusher.Flush()
				}
				if chunk.Done {
					fmt.Fprintf(w, "%s\n\n", translate.OpenAIResponsesSSEDone())
					flushed = true
					flusher.Flush()
					return
				}
			default: // "openai"
				if chunk.Delta != "" {
					fmt.Fprintf(w, "%s\n\n", translate.OpenAISSEChunk(req.ID, chunk))
					flushed = true
					flusher.Flush()
				}
				if chunk.Done {
					fmt.Fprintf(w, "%s\n\n", translate.OpenAISSEDone())
					flushed = true
					flusher.Flush()
					return
				}
			}
		case <-timeout.C:
			if !flushed {
				serviceUnavailable(w, "request timed out waiting for a worker")
			} else {
				fmt.Fprintf(w, "data: {\"error\":\"request timed out\"}\n\n")
				flusher.Flush()
			}
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (h *Handler) batchResponse(w http.ResponseWriter, r *http.Request, req *types.InferenceRequest, ch <-chan types.ChunkMsg) {
	timeout := time.NewTimer(60 * time.Second)
	defer timeout.Stop()

	var sb strings.Builder
	finishReason := "stop"

	for {
		select {
		case chunk, open := <-ch:
			if !open {
				h.writeBatch(w, req, sb.String(), finishReason)
				return
			}
			if chunk.Done {
				if chunk.FinishReason != "" {
					finishReason = chunk.FinishReason
				}
				h.writeBatch(w, req, sb.String(), finishReason)
				return
			}
			sb.WriteString(chunk.Delta)
		case <-timeout.C:
			serviceUnavailable(w, "request timed out")
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (h *Handler) writeBatch(w http.ResponseWriter, req *types.InferenceRequest, content, finishReason string) {
	w.Header().Set("Content-Type", "application/json")
	var resp map[string]any
	switch req.SourceFmt {
	case "anthropic":
		resp = translate.AnthropicFullResponse(req.ID, req.Model, content, finishReason)
	case "openai-responses":
		resp = translate.OpenAIResponsesFullResponse(req.ID, req.Model, content)
	default:
		resp = translate.OpenAIFullResponse(req.ID, content, finishReason)
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("api: encode response: %v", err)
	}
}

func (h *Handler) validKey(key string) bool {
	for _, k := range h.Config.APIKeys {
		if k.Key == key {
			return true
		}
	}
	return false
}

func (h *Handler) OpenAI() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.enqueue(w, r, translate.OpenAIInbound)
	}
}

func (h *Handler) Anthropic() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.enqueue(w, r, translate.AnthropicInbound)
	}
}

func (h *Handler) Responses() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.enqueue(w, r, translate.ResponsesInbound)
	}
}
