// pkg/types/types.go
package types

import (
	"encoding/json"
	"strings"
	"time"
)

// Input modality slugs. ModalityText is a sentinel meaning "capabilities are
// known": a client that has determined what its backend accepts always
// advertises at least ModalityText, so an empty advertised set can be told
// apart on the wire from a known text-only backend.
const (
	ModalityText   = "text"
	ModalityVision = "vision"
	ModalityAudio  = "audio"
	ModalityVideo  = "video"
)

// ModalityForContentType maps an OpenAI/Anthropic/Responses content-part "type"
// to the non-text input modality it requires, or "" for plain-text or unknown
// parts. Matching is substring-based so it covers the various spellings
// ("image_url", "input_image", "image"; "input_audio", "audio"; "video_url").
func ModalityForContentType(t string) string {
	switch {
	case strings.Contains(t, "image"):
		return ModalityVision
	case strings.Contains(t, "audio"):
		return ModalityAudio
	case strings.Contains(t, "video"):
		return ModalityVideo
	default:
		return ""
	}
}

// ModalitiesCompatible reports whether a client advertising the given input
// modalities can serve a request requiring `required` (non-text) modalities.
// An empty advertised set means the client's capabilities are unknown, so it is
// treated as compatible and never excluded — this preserves pass-through for
// backends that don't report their capabilities.
func ModalitiesCompatible(advertised, required []string) bool {
	if len(required) == 0 || len(advertised) == 0 {
		return true
	}
	for _, r := range required {
		found := false
		for _, a := range advertised {
			if a == r {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Priority tiers — lower number = higher priority
type Priority int

const (
	PriorityHigh   Priority = 0
	PriorityNormal Priority = 1
	PriorityLow    Priority = 2
)

// PriorityFromString parses a priority string ("high", "normal", "low").
// Any unrecognised value maps to PriorityNormal.
func PriorityFromString(s string) Priority {
	switch s {
	case "high":
		return PriorityHigh
	case "low":
		return PriorityLow
	default:
		return PriorityNormal
	}
}

// Message is a normalised role/content pair used internally.
// Content is json.RawMessage to preserve tool_use/tool_result blocks without stripping.
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

// InferenceRequest is the canonical internal representation of an LLM request.
type InferenceRequest struct {
	ID        string    `json:"id"`
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens"`
	// Temperature and TopP are pointers so an explicit 0 (e.g. greedy decoding)
	// is distinguishable from "unset". nil means the caller did not specify the
	// value and the backend default applies.
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        []string        `json:"stop,omitempty"` // stop sequences; forwarded to the backend
	Stream      bool            `json:"stream"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	SourceFmt   string          `json:"source_fmt"` // "openai" | "anthropic" | "openai-responses"
	Priority    Priority        `json:"priority"`
	Owner       string          `json:"owner"`                   // username of the API-key holder
	APIKeyLabel string          `json:"api_key_label,omitempty"` // "owner/label" of the key used
	WordCount   int             `json:"word_count,omitempty"`    // approximate word count across all messages
	// Modalities lists the non-text input modalities this request carries
	// (e.g. "vision", "audio"), detected from message content parts at ingress.
	// Empty for plain-text requests. Used to route to capable clients.
	Modalities []string  `json:"modalities,omitempty"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	Attempts   int       `json:"attempts,omitempty"`  // number of times this request has errored and been retried
	OriginID   string    `json:"origin_id,omitempty"` // request ID assigned by the originating router; set by upstream connector for cross-hop tracing
}

// RequestOptimization holds the router-wide toggles that shape inbound requests
// before dispatch. All default to false (no transformation), preserving the
// request exactly as received. Configured via the admin portal and read on the
// per-request hot path, so callers should cache the value rather than refetch.
type RequestOptimization struct {
	// CoalesceNormalize canonicalises message content (JSON key ordering +
	// whitespace) when computing the dedup hash, so semantically identical
	// requests coalesce even if their JSON byte layout differs. Hash-only;
	// never changes what is sent to the model.
	CoalesceNormalize bool `json:"coalesce_normalize"`
	// PrefixAffinity routes requests sharing a leading prefix (system + first
	// user turn) to the client that last served that prefix, so the backend's
	// prompt KV cache stays warm across conversation turns.
	PrefixAffinity bool `json:"prefix_affinity"`
	// CleanRequests drops empty/null messages and trims leading/trailing
	// whitespace on plain-string content. Conservative; structured content
	// (tool/multimodal blocks) is left untouched.
	CleanRequests bool `json:"clean_requests"`
	// CleanAggressive additionally collapses interior whitespace runs in
	// plain-string content. Higher token savings, higher risk of altering
	// model output. Only takes effect when CleanRequests is also on.
	CleanAggressive bool `json:"clean_aggressive"`
	// ClampParams clamps out-of-range sampling parameters (temperature, top_p)
	// into their valid ranges rather than forwarding invalid values.
	ClampParams bool `json:"clamp_params"`
}

// --- WebSocket message types ---
// All WS messages include a "type" field for dispatch.

// ModelInfo describes a model a client supports.
type ModelInfo struct {
	Name         string `json:"name"`
	ContextSize  int    `json:"context_size,omitempty"`  // n_ctx: configured context window in tokens; 0 = unknown
	ContextTrain int    `json:"context_train,omitempty"` // n_ctx_train: model's training context length; 0 = unknown
	// Modalities lists the input modalities this backend accepts. When the
	// client has determined its capabilities it includes at least
	// ModalityText plus any detected extras ("vision", "audio"). Empty means
	// the capabilities are unknown (the backend did not report them and none
	// were configured), in which case the router never excludes the client.
	Modalities []string `json:"modalities,omitempty"`
}

// ModelSlots reports live inference-slot availability for one model from the
// perspective of a particular caller (owner). AvailableSlots is how many slots
// the caller could acquire right now; TotalSlots is the caller-usable capacity
// ceiling. Both account for per-client owner-slot reservations, so they mirror
// what the scheduler would actually grant that caller. ContextSize is the
// largest configured context window advertised for the model.
type ModelSlots struct {
	Model          string
	AvailableSlots int
	TotalSlots     int
	ContextSize    int
}

// RegisterMsg is sent by the client on connect to advertise capabilities.
type RegisterMsg struct {
	Type          string      `json:"type"` // "register"
	Models        []ModelInfo `json:"models"`
	MaxConcurrent int         `json:"max_concurrent"`
	Version       string      `json:"version,omitempty"`
}

// JobMsg is sent by the router to dispatch an inference request to a client.
type JobMsg struct {
	Type    string           `json:"type"` // "job"
	Request InferenceRequest `json:"request"`
}

// UsageInfo carries token counts from the model. The cache fields are best-effort
// pass-through: they are populated only when the backend reports prompt-cache
// accounting, and left zero otherwise. They use neutral internal names; the
// translate layer maps them onto each API's own usage shape on the way out.
type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CacheReadTokens is the number of prompt tokens served from a warm cache
	// (OpenAI usage.prompt_tokens_details.cached_tokens; Anthropic
	// usage.cache_read_input_tokens). A subset of PromptTokens.
	CacheReadTokens int `json:"cache_read_tokens,omitempty"`
	// CacheCreationTokens is the number of prompt tokens written to the cache on
	// this request (Anthropic usage.cache_creation_input_tokens). Backends that
	// only do automatic caching (llama.cpp, ds4) do not report this.
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
}

// ChunkMsg is sent by the client for each token chunk (or full response if non-streaming).
type ChunkMsg struct {
	Type           string          `json:"type"` // "chunk"
	RequestID      string          `json:"request_id"`
	Delta          string          `json:"delta"`
	ToolCallsDelta json.RawMessage `json:"tool_calls_delta,omitempty"`
	Done           bool            `json:"done"`
	FinishReason   string          `json:"finish_reason,omitempty"`
	Usage          *UsageInfo      `json:"usage,omitempty"`
}

// ErrorMsg is sent by the client when inference fails.
type ErrorMsg struct {
	Type      string `json:"type"` // "error"
	RequestID string `json:"request_id"`
	Message   string `json:"message"`
}

// CancelMsg is sent by the router to abort an in-flight inference on the client.
type CancelMsg struct {
	Type      string `json:"type"` // "cancel"
	RequestID string `json:"request_id"`
}

// UpdateMsg is sent by the router to request the client perform an in-place binary update.
type UpdateMsg struct {
	Type string `json:"type"` // "update"
}

// ReleaseMsg is sent by a client to return a job to the router queue.
// The router re-queues the request for another client to handle.
type ReleaseMsg struct {
	Type      string `json:"type"` // "release"
	RequestID string `json:"request_id"`
	Reason    string `json:"reason"` // "model_failed" | "timeout" | "client_shutdown"
}

// MaxAttempts is the total number of times a request may be dispatched before
// being failed back to the caller (initial attempt + retries on client errors/disconnects).
// Defined here so the api package can use it without importing hub.
const MaxAttempts = 3

// ClientSummary is a snapshot of an available client used by the scheduler.
// Defined here (rather than in the hub package) so the scheduler can depend
// on it via an interface without importing hub.
type ClientSummary struct {
	ID                string
	Owner             string
	Models            map[string]bool
	MaxConcurrent     int
	InFlight          int            // current in-flight job count
	ModelContextSizes map[string]int // n_ctx per model; 0 = unknown
	OwnerSlots        map[string]int // model → slots reserved for owner; 0/unset = fully shared
	// ModelModalities maps model name → advertised input modalities. A model
	// with no entry (or an empty list) has unknown capabilities and is never
	// excluded by the modality check.
	ModelModalities map[string][]string
}

// EstimateTokens returns an approximate token count for a request given an input
// word count and max completion tokens. Uses 4/3 tokens per word (standard LLM
// tokeniser approximation). Returns 0 when both inputs are zero.
func EstimateTokens(wordCount, maxTokens int) int {
	return wordCount*4/3 + maxTokens
}

// BetterRequest reports whether request a is a better dispatch choice than b.
// aOwnerMatch and bOwnerMatch indicate whether each request matches the
// preferred client owner (affinity). Ordering: affinity > priority tier > FIFO.
func BetterRequest(a, b InferenceRequest, aOwnerMatch, bOwnerMatch bool) bool {
	if aOwnerMatch != bOwnerMatch {
		return aOwnerMatch
	}
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	return a.EnqueuedAt.Before(b.EnqueuedAt)
}
