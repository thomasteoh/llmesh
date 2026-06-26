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
	"llmesh/router/internal/dedup"
	"llmesh/router/internal/reqopt"
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

// OptStore supplies the request-optimization toggles. Satisfied by *admin.State.
type OptStore interface {
	RequestOpts() types.RequestOptimization
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

// WorkerChecker is satisfied by *hub.Hub (duck typing — no import needed).
// HasWorkerForModel reports whether any connected client serves model.
type WorkerChecker interface {
	HasWorkerForModel(model string, aliases map[string][]string) bool
}

// ContextChecker is satisfied by *hub.Hub (duck typing — no import needed).
// MaxContextForModel returns the largest n_ctx across all connected clients
// serving model (or its aliases). Returns 0 if unknown or no clients connected.
type ContextChecker interface {
	MaxContextForModel(model string, aliases map[string][]string) int
}

// OwnerInFlighter is satisfied by *hub.Hub (duck typing — no import needed).
// OwnerInFlight returns the number of jobs currently in flight for owner.
type OwnerInFlighter interface {
	OwnerInFlight(owner string) int
}

// LimitProvider is satisfied by *admin.State (duck typing — no import needed).
// MaxConcurrentFor returns the max concurrent limit for a key (0 = unlimited).
type LimitProvider interface {
	MaxConcurrentFor(key string) int
}

// Enqueuer accepts new inference requests. Satisfied by *queue.Queue.
type Enqueuer interface {
	TryPush(req types.InferenceRequest) bool
	Push(req types.InferenceRequest)
	PopByID(id string) *types.InferenceRequest
	Len() int
}

// ResponseStore routes response chunks to HTTP handlers. Satisfied by *correlation.Store.
type ResponseStore interface {
	Create(requestID string) <-chan types.ChunkMsg
	Delete(requestID string)
}

// Waker triggers the dispatch scheduler. Satisfied by *scheduler.Scheduler.
type Waker interface {
	Wake()
}

type Handler struct {
	Keys              APIKeyStore
	Models            ModelStore
	Aliases           AliasStore
	Opts              OptStore // optional; nil = no request optimization
	Stats             StatsRecorder
	Queue             Enqueuer
	Correlation       ResponseStore
	Scheduler         Waker
	Canceller         Canceller       // optional; nil = no cancellation
	Workers           WorkerChecker   // optional; nil = skip worker fast-fail
	ContextSizes      ContextChecker  // optional; nil = skip context size validation
	InFlight          OwnerInFlighter // optional; nil = skip per-owner concurrency check
	Limits            LimitProvider   // optional; nil = no per-key concurrency limits
	Dedup             *dedup.Registry // optional; nil = no coalescing
	TTFTTimeout       time.Duration
	ActivityTimeout   time.Duration
	BatchTimeout      time.Duration
	KeepAliveInterval time.Duration
	requestCount      atomic.Int64
}

func (h *Handler) ttftTimeout() time.Duration {
	if h.TTFTTimeout > 0 {
		return h.TTFTTimeout
	}
	return 15 * time.Minute
}

func (h *Handler) activityTimeout() time.Duration {
	if h.ActivityTimeout > 0 {
		return h.ActivityTimeout
	}
	return 5 * time.Minute
}

func (h *Handler) batchTimeout() time.Duration {
	if h.BatchTimeout > 0 {
		return h.BatchTimeout
	}
	return 10 * time.Minute
}

func (h *Handler) keepAliveInterval() time.Duration {
	if h.KeepAliveInterval > 0 {
		return h.KeepAliveInterval
	}
	return 15 * time.Second
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
// Alias names are resolved to their first underlying model before recording
// so stats always accumulate under the canonical model name.
func (h *Handler) recordStats(req *types.InferenceRequest, usage *types.UsageInfo) {
	if h.Stats == nil || usage == nil {
		return
	}
	model := req.Model
	if h.Aliases != nil {
		if targets := h.Aliases.AliasMap()[model]; len(targets) > 0 {
			model = targets[0]
		}
	}
	h.Stats.Record(model, req.Owner, usage.PromptTokens, usage.CompletionTokens)
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

	// Per-key concurrency limit: check before body parse to keep the fast path cheap.
	if h.Limits != nil && h.InFlight != nil {
		limit := h.Limits.MaxConcurrentFor(key)
		if limit > 0 {
			owner := h.Keys.OwnerFor(key)
			if h.InFlight.OwnerInFlight(owner) >= limit {
				apiLogger().Warn("api: per-key concurrency limit reached", "owner", owner, "limit", limit, "ip", clientIP(r))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"error":{"message":"concurrency limit reached for your API key — try again shortly","type":"rate_limit_error"}}` + "\n"))
				return
			}
		}
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

	// Request optimization: shape the request before any downstream work
	// (word count, hashing, dispatch) so all of it sees the cleaned form.
	var opts types.RequestOptimization
	if h.Opts != nil {
		opts = h.Opts.RequestOpts()
		reqopt.Clean(req, opts)
	}

	var aliases map[string][]string
	if h.Aliases != nil {
		aliases = h.Aliases.AliasMap()
	}

	// Fast-fail: reject if no connected worker can serve this model.
	// This fires before the active-models check to give a more specific error when
	// a model is registered in state (e.g. alias) but no client is online for it.
	if h.Workers != nil {
		if !h.Workers.HasWorkerForModel(req.Model, aliases) {
			apiLogger().Warn("api: no worker for model", "model", req.Model, "ip", clientIP(r))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			b, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message": fmt.Sprintf("no worker available for model %q — all clients offline", req.Model),
					"type":    "server_error",
				},
			})
			w.Write(b)
			return
		}
	}

	// Fast-fail: reject if the estimated token count exceeds what every connected
	// client for this model can handle. If some clients have enough context but are
	// busy, we queue normally and the scheduler will wait for one of them.
	wordCount := messageWordCount(req)
	if h.ContextSizes != nil {
		if wordCount > 0 {
			maxCtx := h.ContextSizes.MaxContextForModel(req.Model, aliases)
			if maxCtx > 0 {
				needed := types.EstimateTokens(wordCount, req.MaxTokens)
				if needed > maxCtx {
					apiLogger().Warn("api: context too large for model",
						"model", req.Model, "estimated_tokens", needed,
						"max_context", maxCtx, "ip", clientIP(r))
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					b, _ := json.Marshal(map[string]any{
						"error": map[string]any{
							"message": fmt.Sprintf(
								"estimated token count (%d) exceeds the maximum context size (%d) for model %q",
								needed, maxCtx, req.Model),
							"type": "context_length_exceeded",
							"code": "context_length_exceeded",
						},
					})
					w.Write(b)
					return
				}
			}
		}
	}

	// Validate model/alias exists against currently connected clients.
	// Keep original value (possibly an alias) so the scheduler can distribute
	// across all clients that serve that model.
	if h.Models != nil {
		activeModels := make(map[string]bool)
		for _, m := range h.Models.ActiveModels() {
			activeModels[m] = true
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

	// Coalescing: if an identical request is already in-flight, subscribe to its
	// response instead of occupying a new worker slot.
	if h.Dedup != nil {
		hash := dedup.ContentHashOpts(req, opts.CoalesceNormalize)
		isOriginal, buf, live := h.Dedup.RegisterOrSubscribe(hash)
		if !isOriginal {
			req.ID = uuid.New().String()
			req.Priority = h.Keys.PriorityFor(key)
			req.Owner = h.Keys.OwnerFor(key)
			req.APIKeyLabel = h.Keys.LabelFor(key)
			req.WordCount = wordCount
			h.requestCount.Add(1)
			req.EnqueuedAt = time.Now()
			apiLogger().Info("api: request coalesced", "request_id", req.ID, "model", req.Model, "owner", req.Owner, "key_label", req.APIKeyLabel, "ip", clientIP(r))
			subCh := dedup.MakeSubscriberChan(buf, live)
			if req.Stream {
				h.streamResponse(w, r, req, subCh, "")
			} else {
				h.batchResponse(w, r, req, subCh, "")
			}
			return
		}
		// Original: must unregister when done (success or failure).
		defer h.Dedup.Unregister(hash)
		// Store hash for forwarding chunks to subscribers below.
		req.ID = uuid.New().String()
		req.Priority = h.Keys.PriorityFor(key)
		req.Owner = h.Keys.OwnerFor(key)
		req.APIKeyLabel = h.Keys.LabelFor(key)
		req.WordCount = wordCount
		h.requestCount.Add(1)
		req.EnqueuedAt = time.Now()

		ch := h.Correlation.Create(req.ID)
		defer h.Correlation.Delete(req.ID)

		if !h.Queue.TryPush(*req) {
			apiLogger().Warn("api: queue full, rejecting request", "request_id", req.ID, "model", req.Model, "owner", req.Owner, "queue_depth", h.Queue.Len())
			h.Correlation.Delete(req.ID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"server busy, queue is full — try again shortly","type":"server_error"}}` + "\n"))
			return
		}
		apiLogger().Info("api: request enqueued", "request_id", req.ID, "model", req.Model, "owner", req.Owner, "key_label", req.APIKeyLabel, "priority", priorityName(int(req.Priority)), "stream", req.Stream, "word_count", req.WordCount, "ip", clientIP(r), "queue_depth", h.Queue.Len())
		h.Scheduler.Wake()

		if req.Stream {
			h.streamResponse(w, r, req, ch, hash)
		} else {
			h.batchResponse(w, r, req, ch, hash)
		}
		return
	}

	req.ID = uuid.New().String()
	req.Priority = h.Keys.PriorityFor(key)
	req.Owner = h.Keys.OwnerFor(key)
	req.APIKeyLabel = h.Keys.LabelFor(key)
	req.WordCount = messageWordCount(req)
	h.requestCount.Add(1)
	req.EnqueuedAt = time.Now()

	ch := h.Correlation.Create(req.ID)
	defer h.Correlation.Delete(req.ID)

	if !h.Queue.TryPush(*req) {
		apiLogger().Warn("api: queue full, rejecting request", "request_id", req.ID, "model", req.Model, "owner", req.Owner, "queue_depth", h.Queue.Len())
		h.Correlation.Delete(req.ID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"server busy, queue is full — try again shortly","type":"server_error"}}` + "\n"))
		return
	}
	apiLogger().Info("api: request enqueued", "request_id", req.ID, "model", req.Model, "owner", req.Owner, "key_label", req.APIKeyLabel, "priority", priorityName(int(req.Priority)), "stream", req.Stream, "word_count", req.WordCount, "ip", clientIP(r), "queue_depth", h.Queue.Len())
	h.Scheduler.Wake()

	if req.Stream {
		h.streamResponse(w, r, req, ch, "")
	} else {
		h.batchResponse(w, r, req, ch, "")
	}
}

func (h *Handler) streamResponse(w http.ResponseWriter, r *http.Request, req *types.InferenceRequest, ch <-chan types.ChunkMsg, dedupHash string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		internalError(w)
		return
	}

	ttft := h.ttftTimeout()
	activity := h.activityTimeout()

	// Phase 1 — queue + TTFT timer: covers queue wait, prompt evaluation, and
	// time-to-first-token. Generous because large-context prompt eval on slow
	// local hardware can take many minutes before producing the first output token.
	queueTimer := time.NewTimer(ttft)
	defer queueTimer.Stop()

	// Phase 2 — activity timer: resets on every chunk. Fires only if the worker
	// goes silent, indicating a crash or stuck inference.
	activityTimer := time.NewTimer(activity)
	activityTimer.Stop() // activated when first chunk arrives
	defer activityTimer.Stop()

	resetActivity := func() {
		if !activityTimer.Stop() {
			select {
			case <-activityTimer.C:
			default:
			}
		}
		activityTimer.Reset(activity)
	}

	// SSE keep-alive: send a comment line periodically while waiting for first chunk.
	// Prevents HTTP clients from treating an idle connection as timed out.
	keepAlive := time.NewTicker(h.keepAliveInterval())
	defer keepAlive.Stop()

	started := false
	var firstTokenAt time.Time

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
			if chunk.Delta != "" && firstTokenAt.IsZero() {
				firstTokenAt = time.Now()
			}
			if dedupHash != "" && h.Dedup != nil {
				h.Dedup.Forward(dedupHash, chunk)
			}
			switch req.SourceFmt {
			case "anthropic":
				if chunk.Delta != "" {
					writeSSE(w, translate.AnthropicSSEChunk(chunk))
					flusher.Flush()
				}
				if chunk.Done {
					logRequestDone(req, chunk.Usage, firstTokenAt, true, chunk.FinishReason)
					h.recordStats(req, chunk.Usage)
					for _, l := range translate.AnthropicSSEDone(chunk.FinishReason) {
						writeSSE(w, l)
					}
					flusher.Flush()
					return
				}
			case "openai-responses":
				if chunk.Delta != "" {
					writeSSE(w, translate.OpenAIResponsesSSEChunk(req.ID, chunk))
					flusher.Flush()
				}
				if chunk.Done {
					logRequestDone(req, chunk.Usage, firstTokenAt, true, chunk.FinishReason)
					h.recordStats(req, chunk.Usage)
					writeSSE(w, translate.OpenAIResponsesSSEDone())
					flusher.Flush()
					return
				}
			default: // "openai"
				if chunk.Delta != "" || len(chunk.ToolCallsDelta) > 0 {
					writeSSE(w, translate.OpenAISSEChunk(req.ID, chunk))
					flusher.Flush()
				}
				if chunk.Done {
					logRequestDone(req, chunk.Usage, firstTokenAt, true, chunk.FinishReason)
					h.recordStats(req, chunk.Usage)
					// Emit final chunk with finish_reason and usage before [DONE]
					writeSSE(w, translate.OpenAISSEChunk(req.ID, chunk))
					flusher.Flush()
					writeSSE(w, translate.OpenAISSEDone())
					flusher.Flush()
					return
				}
			}
		case <-keepAlive.C:
			io.WriteString(w, ": ping\n\n")
			flusher.Flush()
		case <-queueTimer.C:
			apiLogger().Error("api: stream timeout", "request_id", req.ID, "timeout", ttft.String())
			h.cancelRequest(req.ID)
			serviceUnavailable(w, "request timed out waiting for a worker")
			return
		case <-activityTimer.C:
			apiLogger().Warn("api: stream worker silent", "request_id", req.ID, "timeout", activity.String())
			h.cancelRequest(req.ID)
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

func (h *Handler) batchResponse(w http.ResponseWriter, r *http.Request, req *types.InferenceRequest, ch <-chan types.ChunkMsg, dedupHash string) {
	batch := h.batchTimeout()
	timeout := time.NewTimer(batch)
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
			if dedupHash != "" && h.Dedup != nil {
				h.Dedup.Forward(dedupHash, chunk)
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
			req.Attempts++
			if req.Attempts < types.MaxAttempts {
				apiLogger().Warn("api: batch timeout, requeuing",
					"request_id", req.ID, "attempt", req.Attempts, "max_attempts", types.MaxAttempts)
				h.cancelRequest(req.ID)
				// Drain any stale chunks from the cancelled dispatch.
			drainStale:
				for {
					select {
					case <-ch:
					default:
						break drainStale
					}
				}
				sb.Reset()
				toolCalls = nil
				usage = nil
				finishReason = "stop"
				h.Queue.Push(*req)
				h.Scheduler.Wake()
				timeout.Reset(batch)
				continue
			}
			apiLogger().Error("api: batch timeout", "request_id", req.ID, "timeout", batch.String(), "attempts", req.Attempts)
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
	h.recordStats(req, usage)
	logRequestDone(req, usage, time.Time{}, false, finishReason)
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

// writeSSE writes one SSE event (payload plus the terminating blank line) using
// direct string writes rather than fmt.Fprintf, avoiding format-string parsing
// and reflection on the per-token streaming hot path.
func writeSSE(w io.Writer, payload string) {
	io.WriteString(w, payload)
	io.WriteString(w, "\n\n")
}

// logRequestDone emits the "api: request completed" structured log with timing and token stats.
// For streaming requests, firstTokenAt is used to compute TTFT (queue wait + prompt eval + first token)
// and tok_per_sec (completion tokens / generation time). Pass zero time for batch requests.
func logRequestDone(req *types.InferenceRequest, usage *types.UsageInfo, firstTokenAt time.Time, stream bool, finishReason string) {
	now := time.Now()
	elapsed := now.Sub(req.EnqueuedAt)
	args := []any{
		"request_id", req.ID,
		"model", req.Model,
		"owner", req.Owner,
		"elapsed_ms", elapsed.Milliseconds(),
		"stream", stream,
		"finish_reason", finishReason,
	}
	if usage != nil {
		args = append(args, "prompt_tokens", usage.PromptTokens, "completion_tokens", usage.CompletionTokens)
		if stream && !firstTokenAt.IsZero() {
			ttftMs := firstTokenAt.Sub(req.EnqueuedAt).Milliseconds()
			genDur := now.Sub(firstTokenAt)
			args = append(args, "ttft_ms", ttftMs)
			if genDur > 0 {
				args = append(args, "tok_per_sec", int(float64(usage.CompletionTokens)/genDur.Seconds()))
			}
		} else if !stream && elapsed > 0 {
			args = append(args, "tok_per_sec", int(float64(usage.CompletionTokens)/elapsed.Seconds()))
		}
	}
	apiLogger().Info("api: request completed", args...)
}

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
