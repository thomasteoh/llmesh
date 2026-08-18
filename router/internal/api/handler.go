package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
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
	AvailableSlotsByModel(owner string) []types.ModelSlots
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

// UsageRecorder persists per-request usage for time-series reporting.
// Satisfied by *admin.UsageRecorder (duck typing — no import needed).
type UsageRecorder interface {
	RecordUsage(model, owner, keyLabel string, prompt, completion int)
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

// ModalityChecker is satisfied by *hub.Hub (duck typing — no import needed).
// ModelModalityVerdict reports whether any connected client serving the model
// can handle the required non-text input modalities, and whether any serving
// client's capabilities are unknown (so a request is only hard-rejected when
// certain no client can serve it).
type ModalityChecker interface {
	ModelModalityVerdict(model string, aliases map[string][]string, required []string) (anyCompatible, anyUnknown bool)
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
	Keys         APIKeyStore
	Models       ModelStore
	Aliases      AliasStore
	Opts         OptStore // optional; nil = no request optimization
	Stats        StatsRecorder
	Usage        UsageRecorder
	Queue        Enqueuer
	Correlation  ResponseStore
	Scheduler    Waker
	Canceller    Canceller       // optional; nil = no cancellation
	Workers      WorkerChecker   // optional; nil = skip worker fast-fail
	ContextSizes ContextChecker  // optional; nil = skip context size validation
	Modalities   ModalityChecker // optional; nil = skip modality fast-fail
	InFlight     OwnerInFlighter // optional; nil = skip per-owner concurrency check
	Limits       LimitProvider   // optional; nil = no per-key concurrency limits
	Dedup        *dedup.Registry // optional; nil = no coalescing
	// MaxRequestBytes caps the inbound request body size. 0 = default (8 MiB);
	// values above the 15 MiB ceiling are clamped so a body that clears ingress
	// still fits the client WebSocket frame limit once wrapped in a job.
	MaxRequestBytes   int
	TTFTTimeout       time.Duration
	ActivityTimeout   time.Duration
	BatchTimeout      time.Duration
	KeepAliveInterval time.Duration
	requestCount      atomic.Int64
	oversizeCount     atomic.Int64 // requests rejected for exceeding MaxRequestBytes
	multimodalCount   atomic.Int64 // requests carrying non-text input modalities
}

const (
	defaultMaxRequestBytes = 8 << 20 // 8 MiB — comfortably fits typical base64 images
	// maxRequestBytesCeiling keeps the configurable limit below the 16 MiB
	// WebSocket frame cap (hub.maxReadBytes) so a body that clears ingress still
	// fits the client's read limit once wrapped in a JobMsg envelope.
	maxRequestBytesCeiling = 15 << 20 // 15 MiB
)

func (h *Handler) maxRequestBytes() int64 {
	n := h.MaxRequestBytes
	if n <= 0 {
		n = defaultMaxRequestBytes
	}
	if n > maxRequestBytesCeiling {
		n = maxRequestBytesCeiling
	}
	return int64(n)
}

// RejectedTooLarge returns the number of requests rejected for exceeding the
// body-size limit since startup.
func (h *Handler) RejectedTooLarge() int64 { return h.oversizeCount.Load() }

// MultimodalRequests returns the number of accepted requests carrying non-text
// input modalities (images/audio) since startup.
func (h *Handler) MultimodalRequests() int64 { return h.multimodalCount.Load() }

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

// attributedModel returns the model a completed request is attributed to, for
// both accounting and the completion log.
//
// served is the concrete name the hub stamped on the response chunks, and is
// the only authority: it is the model the scheduler actually dispatched to,
// after alias resolution, tier fallback, and any retries. It is empty only when
// no chunk carried one — a timeout, a shutdown drain, a terminal error chunk the
// router synthesised for itself, or a chunk from a client that no longer holds
// the job.
//
// There is deliberately no fallback that resolves an alias to one of its
// targets. Guessing that way is what misattributed alias usage in the first
// place, and keeping the guess on the rare path would only make the same error
// rarer without making it visible. Attributing to the name the caller used
// instead leaves the tokens and request counted, prices them at zero (no
// pricing row matches an alias), and surfaces them in unpriced_requests, where
// a request nobody could attribute shows up as one.
//
// Takes no handler state on purpose: attribution consults nothing it could
// disagree with.
func attributedModel(req *types.InferenceRequest, served string) string {
	if served != "" {
		return served
	}
	return req.Model
}

// recordStats records token usage for a completed request against the model
// that actually served it. See attributedModel for how that is determined.
func (h *Handler) recordStats(req *types.InferenceRequest, usage *types.UsageInfo, served string) {
	if usage == nil {
		return
	}
	model := attributedModel(req, served)
	if h.Stats != nil {
		h.Stats.Record(model, req.Owner, usage.PromptTokens, usage.CompletionTokens)
	}
	if h.Usage != nil {
		h.Usage.RecordUsage(model, req.Owner, req.APIKeyLabel, usage.PromptTokens, usage.CompletionTokens)
	}
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

	// MaxBytesReader (not LimitReader) so an over-limit body is rejected with a
	// clear 413 rather than silently truncated into invalid or partial JSON.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxRequestBytes())
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			h.oversizeCount.Add(1)
			apiLogger().Warn("api: request body too large", "limit_bytes", h.maxRequestBytes(), "ip", clientIP(r))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			w.Write([]byte(`{"error":{"message":"request body too large","type":"invalid_request_error"}}` + "\n"))
			return
		}
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

	// Analyse message content once: approximate prompt word count (used for the
	// context fast-fail and scheduler context check) and the non-text input
	// modalities the request carries (used for capability routing).
	wordCount, modalities := analyzeMessages(req)
	req.Modalities = modalities
	if len(modalities) > 0 {
		h.multimodalCount.Add(1)
	}

	// Fast-fail: reject a request carrying input modalities that no connected
	// client can serve. Only rejects when certain — if any serving client is
	// compatible, or any has unknown capabilities, the request proceeds so
	// pass-through keeps working for backends that don't report capability.
	if h.Modalities != nil && len(modalities) > 0 {
		if compatible, unknown := h.Modalities.ModelModalityVerdict(req.Model, aliases, modalities); !compatible && !unknown {
			apiLogger().Warn("api: model does not support required input modalities",
				"model", req.Model, "modalities", modalities, "ip", clientIP(r))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			b, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message": fmt.Sprintf("model %q does not support %v input", req.Model, modalities),
					"type":    "invalid_request_error",
					"code":    "unsupported_modality",
				},
			})
			w.Write(b)
			return
		}
	}

	// Fast-fail: reject if the estimated token count exceeds what every connected
	// client for this model can handle. If some clients have enough context but are
	// busy, we queue normally and the scheduler will wait for one of them.
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
			subCh := dedup.MakeSubscriberChan(r.Context(), buf, live)
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

	// Anthropic streaming uses a stateful encoder to emit the required named
	// event lifecycle (message_start → content_block_* → message_delta → stop).
	var anthropicStreamer *translate.AnthropicStreamer
	if req.SourceFmt == "anthropic" {
		anthropicStreamer = translate.NewAnthropicStreamer(req.ID, req.Model)
	}

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
	// The concrete model behind req.Model, learned from the chunks themselves.
	// Last one wins: a mid-stream retry re-resolves the alias, and the model
	// that finished the response is the one the tokens should be billed to.
	var servedModel string

	for {
		select {
		case chunk, open := <-ch:
			if !open {
				// The channel closed without a Done chunk: abnormal termination
				// (cancellation, correlation dropped, or a coalesced original
				// that ended without completing). Emit a terminal error rather
				// than letting the stream stop silently mid-response.
				apiLogger().Warn("api: stream ended without completion", "request_id", req.ID)
				h.writeStreamError(w, flusher, req, "response ended unexpectedly")
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
			if chunk.Model != "" {
				servedModel = chunk.Model
			}
			if dedupHash != "" && h.Dedup != nil {
				h.Dedup.Forward(dedupHash, chunk)
			}
			switch req.SourceFmt {
			case "anthropic":
				if events := anthropicStreamer.Delta(chunk); len(events) > 0 {
					for _, l := range events {
						writeSSE(w, l)
					}
					flusher.Flush()
				}
				if chunk.Done {
					logRequestDone(req, chunk.Usage, firstTokenAt, true, chunk.FinishReason, servedModel)
					h.recordStats(req, chunk.Usage, servedModel)
					for _, l := range anthropicStreamer.Done(chunk.FinishReason, chunk.Usage) {
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
					logRequestDone(req, chunk.Usage, firstTokenAt, true, chunk.FinishReason, servedModel)
					h.recordStats(req, chunk.Usage, servedModel)
					writeSSE(w, translate.OpenAIResponsesSSEDone())
					flusher.Flush()
					return
				}
			default: // "openai"
				if chunk.Delta != "" || len(chunk.ToolCallsDelta) > 0 {
					// A worker may put content and Done on the same chunk, as the
					// batch path does. Emit the content alone here and let the
					// terminal write below carry finish_reason and usage, so
					// neither the text nor the reason goes out twice. Chunks that
					// are not Done already look like this, so nothing changes for
					// a worker that streams content and completion separately.
					content := chunk
					content.Done = false
					content.FinishReason = ""
					content.Usage = nil
					writeSSE(w, translate.OpenAISSEChunk(req.ID, req.Model, content))
					flusher.Flush()
				}
				if chunk.Done {
					logRequestDone(req, chunk.Usage, firstTokenAt, true, chunk.FinishReason, servedModel)
					h.recordStats(req, chunk.Usage, servedModel)
					// Emit final chunk with finish_reason and usage before [DONE].
					// Any content it carried went out above.
					final := chunk
					final.Delta = ""
					final.ToolCallsDelta = nil
					writeSSE(w, translate.OpenAISSEChunk(req.ID, req.Model, final))
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
			h.writeStreamError(w, flusher, req, "request timed out waiting for a worker")
			return
		case <-activityTimer.C:
			apiLogger().Warn("api: stream worker silent", "request_id", req.ID, "timeout", activity.String())
			h.cancelRequest(req.ID)
			h.writeStreamError(w, flusher, req, "worker stopped responding")
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
	// See streamResponse: the concrete model behind req.Model, per attempt.
	var servedModel string

	for {
		select {
		case chunk, open := <-ch:
			if !open {
				// Channel closed without a Done chunk: the response was
				// truncated. Returning the partial accumulation as HTTP 200
				// would present an incomplete answer as complete, so fail.
				apiLogger().Warn("api: batch response ended without completion", "request_id", req.ID)
				serviceUnavailable(w, "response ended unexpectedly")
				return
			}
			if dedupHash != "" && h.Dedup != nil {
				h.Dedup.Forward(dedupHash, chunk)
			}
			if chunk.Model != "" {
				servedModel = chunk.Model
			}
			// Accumulate before testing Done: a worker answering a non-streaming
			// request in one shot puts the whole response and the Done flag on the
			// same chunk, which is what llama.cpp's readBatch and the shim's
			// handleBatch both send. Finalising first dropped that content and
			// returned an empty message with a full token count.
			sb.WriteString(chunk.Delta)
			if len(chunk.ToolCallsDelta) > 0 {
				toolCalls = chunk.ToolCallsDelta
			}
			if chunk.Done {
				if chunk.FinishReason != "" {
					finishReason = chunk.FinishReason
				}
				if chunk.Usage != nil {
					usage = chunk.Usage
				}
				h.writeBatch(w, req, sb.String(), finishReason, toolCalls, usage, servedModel)
				return
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
				servedModel = ""
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

func (h *Handler) writeBatch(w http.ResponseWriter, req *types.InferenceRequest, content, finishReason string, toolCalls json.RawMessage, usage *types.UsageInfo, served string) {
	h.recordStats(req, usage, served)
	logRequestDone(req, usage, time.Time{}, false, finishReason, served)
	w.Header().Set("Content-Type", "application/json")
	var resp map[string]any
	switch req.SourceFmt {
	case "anthropic":
		resp = translate.AnthropicFullResponse(req.ID, req.Model, content, finishReason, toolCalls, usage)
	case "openai-responses":
		resp = translate.OpenAIResponsesFullResponse(req.ID, req.Model, content, usage)
	default:
		resp = translate.OpenAIFullResponse(req.ID, req.Model, content, finishReason, toolCalls, usage)
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		apiLogger().Error("api: encode response", "error", err)
	}
}

const (
	// perImagePromptWords / perAudioPromptWords approximate the prompt budget a
	// vision/audio content part consumes once the backend tokenises it,
	// expressed as word-equivalents (EstimateTokens multiplies words by 4/3).
	// Bounded constants keep a media part from being ignored entirely (0 tokens)
	// while never letting a base64 blob dominate the estimate. Approximate by
	// design — the context fast-fail only needs the right order of magnitude.
	perImagePromptWords = 600
	perAudioPromptWords = 400
	// rawFallbackWordCap bounds the whitespace-split fallback for content that
	// is neither a plain string nor a recognised parts array, so an unusual
	// structured shape cannot dominate the estimate.
	rawFallbackWordCap = 4096
)

// analyzeMessages returns an approximate prompt word count and the sorted set of
// non-text input modalities present across all messages. Plain-string content
// is counted by words; a structured content-part array contributes its text
// parts' words plus a fixed word-equivalent allowance for each image/audio part
// (which would otherwise count as zero, under-estimating a vision prompt).
// Content that parses as neither falls back to a capped whitespace split.
func analyzeMessages(req *types.InferenceRequest) (wordCount int, modalities []string) {
	seen := map[string]bool{}
	addModality := func(m string) {
		if m == "" || seen[m] {
			return
		}
		seen[m] = true
		modalities = append(modalities, m)
	}
	for _, m := range req.Messages {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			wordCount += len(strings.Fields(s))
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &parts) == nil {
			for _, p := range parts {
				wordCount += len(strings.Fields(p.Text))
				switch mod := types.ModalityForContentType(p.Type); mod {
				case types.ModalityVision, types.ModalityVideo:
					wordCount += perImagePromptWords
					addModality(mod)
				case types.ModalityAudio:
					wordCount += perAudioPromptWords
					addModality(mod)
				}
			}
			continue
		}
		if n := len(strings.Fields(string(m.Content))); n > rawFallbackWordCap {
			wordCount += rawFallbackWordCap
		} else {
			wordCount += n
		}
	}
	sort.Strings(modalities)
	return wordCount, modalities
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

// ModelSlots handles GET /v1/models/slots, a non-standard (llmesh-specific)
// endpoint returning each model the caller can currently obtain a slot on,
// with the number of available slots. Access is governed by per-client
// owner-slot reservations: a model is returned only when the caller's key can
// acquire at least one slot on it right now. Aliases that resolve to a listed
// model are included so callers know every name that reaches that capacity.
func (h *Handler) ModelSlots() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := ExtractBearer(r)
		if key == "" || !h.Keys.ValidAPIKey(key) {
			unauthorised(w)
			return
		}
		owner := h.Keys.OwnerFor(key)
		slots := h.Models.AvailableSlotsByModel(owner)

		// Invert alias→targets into model→[]aliases.
		aliasesByModel := make(map[string][]string)
		if h.Aliases != nil {
			for alias, targets := range h.Aliases.AliasMap() {
				for _, t := range targets {
					aliasesByModel[t] = append(aliasesByModel[t], alias)
				}
			}
		}

		type slotEntry struct {
			Model          string   `json:"model"`
			AvailableSlots int      `json:"available_slots"`
			TotalSlots     int      `json:"total_slots"`
			ContextWindow  int      `json:"context_window,omitempty"`
			Aliases        []string `json:"aliases,omitempty"`
		}
		entries := make([]slotEntry, 0, len(slots))
		for _, s := range slots {
			// Only surface models the key can actually use right now.
			if s.AvailableSlots <= 0 {
				continue
			}
			al := aliasesByModel[s.Model]
			sort.Strings(al)
			entries = append(entries, slotEntry{
				Model:          s.Model,
				AvailableSlots: s.AvailableSlots,
				TotalSlots:     s.TotalSlots,
				ContextWindow:  s.ContextSize,
				Aliases:        al,
			})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Model < entries[j].Model })

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

// writeStreamError emits a protocol-appropriate terminal error into an
// already-committed SSE stream. Once keep-alive pings have flushed the 200
// response, an HTTP error status can no longer be sent, so a timeout or
// abnormal close must be surfaced as an in-band error event followed by the
// stream terminator — otherwise the client sees a stream that just stops.
func (h *Handler) writeStreamError(w io.Writer, flusher http.Flusher, req *types.InferenceRequest, message string) {
	switch req.SourceFmt {
	case "anthropic":
		writeSSE(w, translate.AnthropicErrorEvent(message))
	case "openai-responses":
		writeSSE(w, translate.OpenAIResponsesSSEError(message))
	default:
		writeSSE(w, translate.OpenAISSEError(message))
		writeSSE(w, translate.OpenAISSEDone())
	}
	flusher.Flush()
}

// logRequestDone emits the "api: request completed" structured log with timing and token stats.
// For streaming requests, firstTokenAt is used to compute TTFT (queue wait + prompt eval + first token)
// and tok_per_sec (completion tokens / generation time). Pass zero time for batch requests.
// served is the concrete model the hub reported, or empty if none reached us.
func logRequestDone(req *types.InferenceRequest, usage *types.UsageInfo, firstTokenAt time.Time, stream bool, finishReason, served string) {
	now := time.Now()
	elapsed := now.Sub(req.EnqueuedAt)
	model := attributedModel(req, served)
	args := []any{
		"request_id", req.ID,
		"model", model,
	}
	// Carry the caller's name too when it differs, so a request routed by alias
	// can be read off one line instead of joined against the dispatch log.
	if model != req.Model {
		args = append(args, "requested_model", req.Model)
	}
	args = append(args,
		"owner", req.Owner,
		"elapsed_ms", elapsed.Milliseconds(),
		"stream", stream,
		"finish_reason", finishReason,
	)
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

// maskKey returns a log-safe identifier for an API key: a short non-secret
// prefix for a plausibly-real key, or "****" for anything too short to mask
// (never the raw key material — a mistyped real key must not land in logs).
func maskKey(key string) string {
	if len(key) < 12 {
		return "****"
	}
	return key[:8] + "…"
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
