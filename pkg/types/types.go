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

// --- WebSocket message types ---
// All WS messages include a "type" field for dispatch.

// ModelInfo describes a model a client supports.
type ModelInfo struct {
	Name        string `json:"name"`
	ContextSize int    `json:"context_size,omitempty"` // context window in tokens; 0 = unknown
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

// ReleaseMsg is sent by a client to return a job to the router queue.
// The router re-queues the request for another client to handle.
type ReleaseMsg struct {
	Type      string `json:"type"`       // "release"
	RequestID string `json:"request_id"`
	Reason    string `json:"reason"`     // "model_failed" | "timeout" | "client_shutdown"
}
