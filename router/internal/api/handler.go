package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"llmesh/pkg/types"
	"llmesh/router/internal/correlation"
	"llmesh/router/internal/queue"
	"llmesh/router/internal/scheduler"
	"llmesh/router/internal/translate"
)

var _apiLog atomic.Pointer[slog.Logger]

func init() {
	l := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	_apiLog.Store(l)
}

// SetLogger replaces the package logger atomically. Safe to call before ListenAndServe.
func SetLogger(l *slog.Logger) { _apiLog.Store(l) }

func apiLogger() *slog.Logger { return _apiLog.Load() }

// APIKeyStore is satisfied by *admin.State (duck typing — no import needed).
type APIKeyStore interface {
	ValidAPIKey(key string) bool
	PriorityFor(key string) types.Priority
	OwnerFor(key string) string
	LabelFor(key string) string
}

// ModelStore is satisfied by *hub.Hub (duck typing — no import needed).
type ModelStore interface {
	ActiveModels() []string
	ActiveModelInfos() []types.ModelInfo
}

// AliasStore is satisfied by *admin.State (duck typing — no import needed).
type AliasStore interface {
	AliasMap() map[string][]string
}

// StatsRecorder is satisfied by *stats.Stats (duck typing — no import needed).
type StatsRecorder interface {
	Record(model, user string, prompt, completion int)
}

// Canceller is satisfied by *hub.Hub (duck typing — no import needed).
// CancelRequest broadcasts a cancel to the client holding the given requestID.
type Canceller interface {
	CancelRequest(requestID string)
}

type Handler struct {
	Keys         APIKeyStore
	Models       ModelStore
	Aliases      AliasStore
	Stats        StatsRecorder
	Queue        *queue.Queue
	Correlation  *correlation.Store
	Scheduler    *scheduler.Scheduler
	Canceller    Canceller // optional; nil = no cancellation
	requestCount atomic.Int64
}

// Count returns the total number of API requests handled since startup.
func (h *Handler) Count() int64 {
	return h.requestCount.Load()
}

// cancelRequest removes a request from the queue (if not yet dispatched) and
// sends a cancel message to the client (if already dispatched). Both are no-ops
// if the request has already completed.
func (h *Handler) cancelRequest(reqID string) {
	h.Queue.PopByID(reqID) // no-op if already dispatched
	if h.Canceller != nil {
		h.Canceller.CancelRequest(reqID)
	}
}

// recordStats records token usage for a completed request.
// req.Model is already the canonical model name by the time this is called
// (alias resolved by the scheduler before dispatch).
func (h *Handler) recordStats(req *types.InferenceRequest, usage *types.UsageInfo) {
	if h.Stats == nil || usage == nil {
		return
	}
	h.Stats.Record(req.Model, req.Owner, usage.PromptTokens, usage.CompletionTokens)
}

func (h *Handler) enqueue(
	w http.ResponseWriter,
	r *http.Request,
	inbound func([]byte) (*types.InferenceRequest, error),
) {
	key := ExtractBearer(r)
	if key == "" || !h.Keys.ValidAPIKey(key) {
		apiLogger().Error("api: unauthorized", "ip", clientIP(r), "key_prefix", maskKey(key), "path", r.URL.Path)
		unauthorised(w)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		apiLogger().Warn("api: read body failed", "error", err, "ip", clientIP(r))
		internalError(w)
		return
	}

	req, err := inbound(body)
	if err != nil {
		apiLogger().Warn("api: bad request", "error", err, "model", sanitizeModel(req), "ip", clientIP(r))
		b, _ := json.Marshal(err.Error())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":{"message":%s}}`+"\n", b)
		return
	}

	// Validate model/alias exists against currently connected clients.
	// Keep original value (possibly an alias) so the scheduler can distribute
	// across all clients that serve that model.
	if h.Models != nil {
		activeModels := make(map[string]bool)
		for _, m := range h.Models.ActiveModels() {
			activeModels[m] = true
		}
		var aliases map[string][]string
		if h.Aliases != nil {
			aliases = h.Aliases.AliasMap()
		}
		// Valid if the model is directly served, or if any alias target is served.
		valid := activeModels[req.Model]
		if !valid && aliases != nil {
			for _, target := range aliases[req.Model] {
				if activeModels[target] {
					valid = true
					break
				}
			}
		}
		if !valid {
			apiLogger().Warn("api: model not found", "model", req.Model, "available_models", h.Models.ActiveModels(), "ip", clientIP(r))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			available := h.Models.ActiveModels()
			b, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message":           fmt.Sprintf("model %q not found", req.Model),
					"available_models":  available,
					"available_aliases": aliases,
				},
			})
			w.Write(b)
			return
		}
	}

	req.ID = uuid.New().String()
	req.Priority = h.Keys.PriorityFor(key)
	req.Owner = h.Keys.OwnerFor(key)
	req.APIKeyLabel = h.Keys.LabelFor(key)
	req.WordCount = messageWordCount(req)
	h.requestCount.Add(1)
	req.EnqueuedAt = time.Now()

	apiLogger().Info("api: request enqueued", "request_id", req.ID, "model", req.Model, "owner", req.Owner, "key_label", req.APIKeyLabel, "priority", priorityName(int(req.Priority)), "stream", req.Stream, "word_count", req.WordCount, "ip", clientIP(r))

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

	// Phase 1 — queue + TTFT timer: 15min to receive the first chunk.
	// Covers queue wait, prompt evaluation, and time-to-first-token.
	// Generous because large-context prompt eval on slow local hardware can take
	// many minutes before producing the first output token.
	queueTimer := time.NewTimer(15 * time.Minute)
	defer queueTimer.Stop()

	// Phase 2 — activity timer: resets on every chunk. Fires only if the worker
	// goes silent for 5min, indicating a crash or stuck inference.
	// Generous enough for slow local hardware while avoiding long waits on
	// genuinely stuck inference.
	activityTimer := time.NewTimer(5 * time.Minute)
	activityTimer.Stop() // activated when first chunk arrives
	defer activityTimer.Stop()

	resetActivity := func() {
		if !activityTimer.Stop() {
			select {
			case <-activityTimer.C:
			default:
			}
		}
		activityTimer.Reset(5 * time.Minute)
	}

	// SSE keep-alive: send a comment line every 15s while waiting for first chunk.
	// Prevents HTTP clients from treating an idle connection as timed out.
	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()

	started := false

	for {
		select {
		case chunk, open := <-ch:
			if !open {
				return
			}
			if !started {
				started = true
				if !queueTimer.Stop() {
					select {
					case <-queueTimer.C:
					default:
					}
				}
			}
			resetActivity()
			switch req.SourceFmt {
			case "anthropic":
				if chunk.Delta != "" {
					fmt.Fprintf(w, "%s\n\n", translate.AnthropicSSEChunk(chunk))
					flusher.Flush()
				}
				if chunk.Done {
					elapsed := time.Since(req.EnqueuedAt)
					apiLogger().Info("api: request completed", "request_id", req.ID, "model", req.Model, "owner", req.Owner, "elapsed_ms", elapsed.Milliseconds(), "stream", true, "finish_reason", chunk.FinishReason)
					h.recordStats(req, chunk.Usage)
					for _, l := range translate.AnthropicSSEDone(chunk.FinishReason) {
						fmt.Fprintf(w, "%s\n\n", l)
					}
					flusher.Flush()
					return
				}
			case "openai-responses":
				if chunk.Delta != "" {
					fmt.Fprintf(w, "%s\n\n", translate.OpenAIResponsesSSEChunk(req.ID, chunk))
					flusher.Flush()
				}
				if chunk.Done {
					elapsed := time.Since(req.EnqueuedAt)
					apiLogger().Info("api: request completed", "request_id", req.ID, "model", req.Model, "owner", req.Owner, "elapsed_ms", elapsed.Milliseconds(), "stream", true, "finish_reason", chunk.FinishReason)
					h.recordStats(req, chunk.Usage)
					fmt.Fprintf(w, "%s\n\n", translate.OpenAIResponsesSSEDone())
					flusher.Flush()
					return
				}
			default: // "openai"
				if chunk.Delta != "" || len(chunk.ToolCallsDelta) > 0 {
					fmt.Fprintf(w, "%s\n\n", translate.OpenAISSEChunk(req.ID, chunk))
					flusher.Flush()
				}
				if chunk.Done {
					elapsed := time.Since(req.EnqueuedAt)
					apiLogger().Info("api: request completed", "request_id", req.ID, "model", req.Model, "owner", req.Owner, "elapsed_ms", elapsed.Milliseconds(), "stream", true, "finish_reason", chunk.FinishReason)
					h.recordStats(req, chunk.Usage)
					// Emit final chunk with finish_reason and usage before [DONE]
					fmt.Fprintf(w, "%s\n\n", translate.OpenAISSEChunk(req.ID, chunk))
					flusher.Flush()
					fmt.Fprintf(w, "%s\n\n", translate.OpenAISSEDone())
					flusher.Flush()
					return
				}
			}
		case <-keepAlive.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case <-queueTimer.C:
			apiLogger().Error("api: stream timeout", "request_id", req.ID, "timeout", "15min")
			serviceUnavailable(w, "request timed out waiting for a worker")
			return
		case <-activityTimer.C:
			apiLogger().Warn("api: stream worker silent", "request_id", req.ID, "timeout", "5min")
			fmt.Fprintf(w, "data: {\"error\":\"worker stopped responding\"}\n\n")
			flusher.Flush()
			return
		case <-r.Context().Done():
			apiLogger().Info("api: stream client disconnected", "request_id", req.ID)
			h.cancelRequest(req.ID)
			return
		}
	}
}

func (h *Handler) batchResponse(w http.ResponseWriter, r *http.Request, req *types.InferenceRequest, ch <-chan types.ChunkMsg) {
	// 10min covers queue wait + full inference for large coding requests.
	timeout := time.NewTimer(10 * time.Minute)
	defer timeout.Stop()

	var sb strings.Builder
	var toolCalls json.RawMessage
	var usage *types.UsageInfo
	finishReason := "stop"

	for {
		select {
		case chunk, open := <-ch:
			if !open {
				h.writeBatch(w, req, sb.String(), finishReason, toolCalls, usage)
				return
			}
			if chunk.Done {
				if chunk.FinishReason != "" {
					finishReason = chunk.FinishReason
				}
				if len(chunk.ToolCallsDelta) > 0 {
					toolCalls = chunk.ToolCallsDelta
				}
				if chunk.Usage != nil {
					usage = chunk.Usage
				}
				h.writeBatch(w, req, sb.String(), finishReason, toolCalls, usage)
				return
			}
			sb.WriteString(chunk.Delta)
			if len(chunk.ToolCallsDelta) > 0 {
				toolCalls = chunk.ToolCallsDelta
			}
		case <-timeout.C:
			apiLogger().Error("api: batch timeout", "request_id", req.ID, "timeout", "10min")
			serviceUnavailable(w, "request timed out")
			h.cancelRequest(req.ID)
			return
		case <-r.Context().Done():
			apiLogger().Info("api: batch client disconnected", "request_id", req.ID)
			h.cancelRequest(req.ID)
			return
		}
	}
}

func (h *Handler) writeBatch(w http.ResponseWriter, req *types.InferenceRequest, content, finishReason string, toolCalls json.RawMessage, usage *types.UsageInfo) {
	elapsed := time.Since(req.EnqueuedAt)
	h.recordStats(req, usage)
	apiLogger().Info("api: request completed", "request_id", req.ID, "model", req.Model, "owner", req.Owner, "elapsed_ms", elapsed.Milliseconds(), "stream", false, "finish_reason", finishReason)
	w.Header().Set("Content-Type", "application/json")
	var resp map[string]any
	switch req.SourceFmt {
	case "anthropic":
		resp = translate.AnthropicFullResponse(req.ID, req.Model, content, finishReason)
	case "openai-responses":
		resp = translate.OpenAIResponsesFullResponse(req.ID, req.Model, content)
	default:
		resp = translate.OpenAIFullResponse(req.ID, content, finishReason, toolCalls, usage)
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		apiLogger().Error("api: encode response", "error", err)
	}
}

// messageWordCount returns an approximate word count across all messages in req.
// It tries to extract plain text from content fields; falls back to raw bytes.
func messageWordCount(req *types.InferenceRequest) int {
	count := 0
	for _, m := range req.Messages {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			count += len(strings.Fields(s))
			continue
		}
		var blocks []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &blocks) == nil {
			for _, b := range blocks {
				count += len(strings.Fields(b.Text))
			}
			continue
		}
		count += len(strings.Fields(string(m.Content)))
	}
	return count
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

// ModelList handles GET /v1/models returning an OpenAI-compatible model list.
// Includes real models (with context_window) plus aliases as additional entries.
func (h *Handler) ModelList() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := ExtractBearer(r)
		if key == "" || !h.Keys.ValidAPIKey(key) {
			unauthorised(w)
			return
		}
		infos := h.Models.ActiveModelInfos()

		type modelEntry struct {
			ID            string `json:"id"`
			Object        string `json:"object"`
			Created       int    `json:"created"`
			OwnedBy       string `json:"owned_by"`
			ContextWindow int    `json:"context_window,omitempty"`
		}

		// Build context size lookup for alias resolution
		ctxByModel := make(map[string]int, len(infos))
		for _, m := range infos {
			ctxByModel[m.Name] = m.ContextSize
		}

		entries := make([]modelEntry, 0, len(infos))
		for _, m := range infos {
			entries = append(entries, modelEntry{
				ID:            m.Name,
				Object:        "model",
				Created:       0,
				OwnedBy:       "system",
				ContextWindow: m.ContextSize,
			})
		}

		// Add alias entries: context_window = minimum of all reachable targets
		if h.Aliases != nil {
			aliases := h.Aliases.AliasMap()
			for alias, targets := range aliases {
				minCtx := 0
				for _, t := range targets {
					if ctx := ctxByModel[t]; ctx > 0 {
						if minCtx == 0 || ctx < minCtx {
							minCtx = ctx
						}
					}
				}
				entries = append(entries, modelEntry{
					ID:            alias,
					Object:        "model",
					Created:       0,
					OwnedBy:       "system",
					ContextWindow: minCtx,
				})
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   entries,
		})
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// clientIP extracts the client IP from the request, checking X-Forwarded-For first.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.Split(fwd, ",")[0]
	}
	if ip := r.RemoteAddr; ip != "" {
		// Strip port
		if idx := strings.LastIndex(ip, ":"); idx != -1 {
			return ip[:idx]
		}
		return ip
	}
	return "-"
}

// maskKey returns the first 8 chars of a key for log-safe identification.
func maskKey(key string) string {
	if len(key) <= 8 {
		return "****" + key
	}
	return key[:8] + "****"
}

// sanitizeModel returns a safe string for logging before parsing succeeds.
func sanitizeModel(req *types.InferenceRequest) string {
	if req == nil {
		return "<nil>"
	}
	return req.Model
}

// priorityName converts a Priority int to a display string.
func priorityName(p int) string {
	switch p {
	case 0:
		return "high"
	case 2:
		return "low"
	default:
		return "normal"
	}
}
