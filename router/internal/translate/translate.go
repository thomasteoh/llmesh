package translate

import (
	"encoding/json"
	"fmt"
	"llmesh/pkg/types"
)

// --- Inbound translators ---

type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature float64         `json:"temperature"`
	TopP        float64         `json:"top_p"`
	Stream      bool            `json:"stream"`
}

type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or array
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
	}
	for _, m := range r.Messages {
		content, err := extractContent(m.Content)
		if err != nil {
			return nil, err
		}
		req.Messages = append(req.Messages, types.Message{Role: m.Role, Content: content})
	}
	return req, nil
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// AnthropicInbound converts an Anthropic messages body to an InferenceRequest.
func AnthropicInbound(body []byte) (*types.InferenceRequest, error) {
	var r anthropicRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("anthropic parse: %w", err)
	}
	req := &types.InferenceRequest{
		Model:     r.Model,
		MaxTokens: r.MaxTokens,
		Stream:    r.Stream,
		SourceFmt: "anthropic",
	}
	if r.System != "" {
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
	var inputStr string
	if err := json.Unmarshal(r.Input, &inputStr); err == nil {
		req.Messages = []types.Message{{Role: "user", Content: inputStr}}
	} else {
		var msgs []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
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

func extractContent(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("content parse: %w", err)
	}
	result := ""
	for _, p := range parts {
		if p.Type == "text" {
			result += p.Text
		}
	}
	return result, nil
}

// --- Outbound SSE formatters ---

type openAIChunkPayload struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Choices []openAIChunkChoice `json:"choices"`
}

type openAIChunkChoice struct {
	Index        int         `json:"index"`
	Delta        openAIDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type openAIDelta struct {
	Content string `json:"content"`
}

// OpenAISSEChunk formats a ChunkMsg as an OpenAI SSE line (without trailing newlines).
func OpenAISSEChunk(requestID string, chunk types.ChunkMsg) string {
	var finishReason *string
	if chunk.Done && chunk.FinishReason != "" {
		finishReason = &chunk.FinishReason
	}
	payload := openAIChunkPayload{
		ID:     requestID,
		Object: "chat.completion.chunk",
		Choices: []openAIChunkChoice{{
			Index:        0,
			Delta:        openAIDelta{Content: chunk.Delta},
			FinishReason: finishReason,
		}},
	}
	b, _ := json.Marshal(payload)
	return "data: " + string(b)
}

// OpenAISSEDone returns the terminal SSE line for OpenAI streaming.
func OpenAISSEDone() string {
	return "data: [DONE]"
}

// OpenAIFullResponse assembles a complete (non-streaming) OpenAI chat response.
func OpenAIFullResponse(requestID string, content string, finishReason string) map[string]any {
	return map[string]any{
		"id":     requestID,
		"object": "chat.completion",
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": finishReason,
			},
		},
	}
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
	b, _ := json.Marshal(payload)
	return "data: " + string(b)
}

// AnthropicSSEDone returns the terminal SSE events for Anthropic streaming.
func AnthropicSSEDone() []string {
	stop, _ := json.Marshal(map[string]any{"type": "message_stop"})
	return []string{"data: " + string(stop)}
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
	b, _ := json.Marshal(payload)
	return "data: " + string(b)
}

// OpenAIResponsesSSEDone returns the terminal SSE event for OpenAI Responses API streaming.
func OpenAIResponsesSSEDone() string {
	b, _ := json.Marshal(map[string]any{"type": "response.completed"})
	return "data: " + string(b)
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
