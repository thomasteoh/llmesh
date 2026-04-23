package translate

import (
	"encoding/json"
	"fmt"
	"llmesh/pkg/types"
)

// --- Inbound translators ---

type openAIMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature float64         `json:"temperature"`
	TopP        float64         `json:"top_p"`
	Stream      bool            `json:"stream"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
}

// OpenAIInbound converts an OpenAI chat completions body to an InferenceRequest.
func OpenAIInbound(body []byte) (*types.InferenceRequest, error) {
	var r openAIRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("openai parse: %w", err)
	}
	req := &types.InferenceRequest{
		Model:       r.Model,
		MaxTokens:   r.MaxTokens,
		Temperature: r.Temperature,
		TopP:        r.TopP,
		Stream:      r.Stream,
		SourceFmt:   "openai",
		Tools:       r.Tools,
		ToolChoice:  r.ToolChoice,
	}
	for _, m := range r.Messages {
		req.Messages = append(req.Messages, types.Message{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		})
	}
	return req, nil
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicRequest struct {
	Model      string             `json:"model"`
	System     json.RawMessage    `json:"system,omitempty"`
	Messages   []anthropicMessage `json:"messages"`
	MaxTokens  int                `json:"max_tokens"`
	Stream     bool               `json:"stream"`
	Tools      json.RawMessage    `json:"tools,omitempty"`
	ToolChoice json.RawMessage    `json:"tool_choice,omitempty"`
}

// AnthropicInbound converts an Anthropic messages body to an InferenceRequest.
func AnthropicInbound(body []byte) (*types.InferenceRequest, error) {
	var r anthropicRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("anthropic parse: %w", err)
	}
	req := &types.InferenceRequest{
		Model:      r.Model,
		MaxTokens:  r.MaxTokens,
		Stream:     r.Stream,
		SourceFmt:  "anthropic",
		Tools:      r.Tools,
		ToolChoice: r.ToolChoice,
	}
	if len(r.System) > 0 && string(r.System) != "null" {
		req.Messages = append(req.Messages, types.Message{Role: "system", Content: r.System})
	}
	for _, m := range r.Messages {
		req.Messages = append(req.Messages, types.Message{Role: m.Role, Content: m.Content})
	}
	return req, nil
}

type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Stream          bool            `json:"stream"`
}

// ResponsesInbound converts an OpenAI Responses API body to an InferenceRequest.
func ResponsesInbound(body []byte) (*types.InferenceRequest, error) {
	var r responsesRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("responses parse: %w", err)
	}
	req := &types.InferenceRequest{
		Model:     r.Model,
		MaxTokens: r.MaxOutputTokens,
		Stream:    r.Stream,
		SourceFmt: "openai-responses",
	}
	// Input is either a plain string or an array of message objects.
	var inputStr string
	if err := json.Unmarshal(r.Input, &inputStr); err == nil {
		// r.Input is already a JSON string literal — use directly as content.
		req.Messages = []types.Message{{Role: "user", Content: r.Input}}
	} else {
		var msgs []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(r.Input, &msgs); err != nil {
			return nil, fmt.Errorf("responses input parse: %w", err)
		}
		for _, m := range msgs {
			req.Messages = append(req.Messages, types.Message{Role: m.Role, Content: m.Content})
		}
	}
	return req, nil
}

// mustMarshal marshals v to JSON and panics if it fails.
// Used for SSE formatters where the payload shape is fixed and marshal failure is a programmer error.
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("translate: marshal error: %v", err))
	}
	return b
}

// --- Outbound SSE formatters ---

type openAIChunkChoice struct {
	Index        int         `json:"index"`
	Delta        openAIDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type openAIDelta struct {
	Content   string          `json:"content,omitempty"`
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
}

// OpenAISSEChunk formats a ChunkMsg as an OpenAI SSE line (without trailing newlines).
func OpenAISSEChunk(requestID string, chunk types.ChunkMsg) string {
	var finishReason *string
	if chunk.Done && chunk.FinishReason != "" {
		finishReason = &chunk.FinishReason
	}
	delta := openAIDelta{Content: chunk.Delta}
	if len(chunk.ToolCallsDelta) > 0 {
		delta.ToolCalls = chunk.ToolCallsDelta
	}
	payload := map[string]any{
		"id":     requestID,
		"object": "chat.completion.chunk",
		"choices": []openAIChunkChoice{{
			Index:        0,
			Delta:        delta,
			FinishReason: finishReason,
		}},
	}
	if chunk.Usage != nil {
		payload["usage"] = chunk.Usage
	}
	b, _ := json.Marshal(payload)
	return "data: " + string(b)
}

// OpenAISSEDone returns the terminal SSE line for OpenAI streaming.
func OpenAISSEDone() string {
	return "data: [DONE]"
}

// OpenAIFullResponse assembles a complete (non-streaming) OpenAI chat response.
func OpenAIFullResponse(requestID string, content string, finishReason string, toolCalls json.RawMessage, usage *types.UsageInfo) map[string]any {
	message := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	resp := map[string]any{
		"id":     requestID,
		"object": "chat.completion",
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp
}

// AnthropicSSEChunk formats a ChunkMsg as an Anthropic SSE line.
func AnthropicSSEChunk(chunk types.ChunkMsg) string {
	payload := map[string]any{
		"type": "content_block_delta",
		"delta": map[string]any{
			"type": "text_delta",
			"text": chunk.Delta,
		},
	}
	return "data: " + string(mustMarshal(payload))
}

// AnthropicSSEDone returns the terminal SSE events for Anthropic streaming.
// The Anthropic protocol requires message_delta before message_stop.
func AnthropicSSEDone(stopReason string) []string {
	delta := mustMarshal(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
	})
	stop := mustMarshal(map[string]any{"type": "message_stop"})
	return []string{"data: " + string(delta), "data: " + string(stop)}
}

// AnthropicFullResponse assembles a complete non-streaming Anthropic response.
func AnthropicFullResponse(requestID string, model string, content string, stopReason string) map[string]any {
	return map[string]any{
		"id":    requestID,
		"type":  "message",
		"role":  "assistant",
		"model": model,
		"content": []map[string]any{
			{"type": "text", "text": content},
		},
		"stop_reason": stopReason,
	}
}

// OpenAIResponsesSSEChunk formats a ChunkMsg as an OpenAI Responses API SSE line.
func OpenAIResponsesSSEChunk(requestID string, chunk types.ChunkMsg) string {
	payload := map[string]any{
		"type":  "response.output_text.delta",
		"delta": chunk.Delta,
	}
	return "data: " + string(mustMarshal(payload))
}

// OpenAIResponsesSSEDone returns the terminal SSE event for OpenAI Responses API streaming.
func OpenAIResponsesSSEDone() string {
	return "data: " + string(mustMarshal(map[string]any{"type": "response.completed"}))
}

// OpenAIResponsesFullResponse assembles a complete non-streaming Responses API response.
func OpenAIResponsesFullResponse(requestID string, model string, content string) map[string]any {
	return map[string]any{
		"id":     requestID,
		"object": "response",
		"model":  model,
		"output": []map[string]any{
			{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": content}},
			},
		},
	}
}
