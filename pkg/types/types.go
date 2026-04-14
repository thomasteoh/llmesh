// pkg/types/types.go
package types

import "time"

// Priority tiers — lower number = higher priority
type Priority int

const (
	PriorityHigh   Priority = 0
	PriorityNormal Priority = 1
	PriorityLow    Priority = 2
)

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
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// InferenceRequest is the canonical internal representation of an LLM request.
type InferenceRequest struct {
	ID          string    `json:"id"`
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature"`
	TopP        float64   `json:"top_p"`
	Stream      bool      `json:"stream"`
	SourceFmt   string    `json:"source_fmt"` // "openai" | "anthropic" | "openai-responses"
	Priority    Priority  `json:"priority"`
	EnqueuedAt  time.Time `json:"enqueued_at"`
}

// --- WebSocket message types ---
// All WS messages include a "type" field for dispatch.

// ModelInfo describes a model a client supports.
type ModelInfo struct {
	Name string `json:"name"`
}

// RegisterMsg is sent by the client on connect to advertise capabilities.
type RegisterMsg struct {
	Type          string      `json:"type"` // "register"
	Models        []ModelInfo `json:"models"`
	MaxConcurrent int         `json:"max_concurrent"`
}

// JobMsg is sent by the router to dispatch an inference request to a client.
type JobMsg struct {
	Type    string           `json:"type"` // "job"
	Request InferenceRequest `json:"request"`
}

// ChunkMsg is sent by the client for each token chunk (or full response if non-streaming).
type ChunkMsg struct {
	Type         string `json:"type"`          // "chunk"
	RequestID    string `json:"request_id"`
	Delta        string `json:"delta"`
	Done         bool   `json:"done"`
	FinishReason string `json:"finish_reason,omitempty"`
}

// ErrorMsg is sent by the client when inference fails.
type ErrorMsg struct {
	Type      string `json:"type"` // "error"
	RequestID string `json:"request_id"`
	Message   string `json:"message"`
}
