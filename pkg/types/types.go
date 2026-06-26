// pkg/types/types.go
package types

import (
	"encoding/json"
	"time"
)

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
	ID          string          `json:"id"`
	Model       string          `json:"model"`
	Messages    []Message       `json:"messages"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature float64         `json:"temperature"`
	TopP        float64         `json:"top_p"`
	Stream      bool            `json:"stream"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	SourceFmt   string          `json:"source_fmt"` // "openai" | "anthropic" | "openai-responses"
	Priority    Priority        `json:"priority"`
	Owner       string          `json:"owner"`        // username of the API-key holder
	APIKeyLabel string          `json:"api_key_label,omitempty"` // "owner/label" of the key used
	WordCount   int             `json:"word_count,omitempty"`    // approximate word count across all messages
	EnqueuedAt  time.Time       `json:"enqueued_at"`
	Attempts    int             `json:"attempts,omitempty"` // number of times this request has errored and been retried
	OriginID    string          `json:"origin_id,omitempty"` // request ID assigned by the originating router; set by upstream connector for cross-hop tracing
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

// UsageInfo carries token counts from the model.
type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChunkMsg is sent by the client for each token chunk (or full response if non-streaming).
type ChunkMsg struct {
	Type           string          `json:"type"`                     // "chunk"
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
	Type      string `json:"type"`       // "release"
	RequestID string `json:"request_id"`
	Reason    string `json:"reason"`     // "model_failed" | "timeout" | "client_shutdown"
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
